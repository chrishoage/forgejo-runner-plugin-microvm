// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// entry is one tar entry of a test archive.
type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func archive(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		typ, mode := e.typ, e.mode
		if typ == 0 {
			typ = tar.TypeReg
		}
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.name, Typeflag: typ, Mode: mode, Linkname: e.link, Size: int64(len(e.body))}
		if typ != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExtractFiles(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "root")
	err := extract(bytes.NewReader(archive(t,
		entry{name: "./bin/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "bin/busybox", body: "#!", mode: 0o755},
		entry{name: "bin/sh", typ: tar.TypeSymlink, link: "/bin/busybox"},
		entry{name: "bin/ash", typ: tar.TypeLink, link: "bin/busybox"},
		// Read-only, with a file under it written afterwards.
		entry{name: "ro", typ: tar.TypeDir, mode: 0o555},
		entry{name: "ro/f", body: "x"},
		// A file replaced by a directory, and a directory by a file.
		entry{name: "a", body: "file"},
		entry{name: "a", typ: tar.TypeDir},
		entry{name: "b/c", body: "under b"},
		entry{name: "b", body: "now a file"},
		entry{name: "dev/null", typ: tar.TypeChar},
	)), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "bin/ash")); got != "#!" {
		t.Errorf("bin/ash = %q", got)
	}
	if got, err := os.Readlink(filepath.Join(dir, "bin/sh")); err != nil || got != "/bin/busybox" {
		t.Errorf("bin/sh -> %q, %v", got, err)
	}
	fi, err := os.Stat(filepath.Join(dir, "ro"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o555 {
		t.Errorf("ro has mode %v, want 0555", fi.Mode().Perm())
	}
	if got := readFile(t, filepath.Join(dir, "ro/f")); got != "x" {
		t.Errorf("ro/f = %q", got)
	}
	if fi, err := os.Stat(filepath.Join(dir, "a")); err != nil || !fi.IsDir() {
		t.Errorf("a is not a directory: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "b")); got != "now a file" {
		t.Errorf("b = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "dev/null")); !os.IsNotExist(err) {
		t.Errorf("a device node was extracted: %v", err)
	}
	// The test can clean the read-only directory up.
	if err := os.Chmod(filepath.Join(dir, "ro"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Nothing an archive says can write outside the directory.
func TestExtractStaysInside(t *testing.T) {
	t.Parallel()
	for name, entries := range map[string][]entry{
		"dot-dot name":      {{name: "../../escaped", body: "x"}},
		"absolute name":     {{name: "/escaped", body: "x"}},
		"through a symlink": {{name: "up", typ: tar.TypeSymlink, link: "../.."}, {name: "up/escaped", body: "x"}},
		"through an absolute symlink": {
			{name: "abs", typ: tar.TypeSymlink, link: "OUTSIDE"}, {name: "abs/escaped", body: "x"},
		},
		"hard link out": {{name: "escaped", typ: tar.TypeLink, link: "../../outside-file"}},
		"file over a symlink": {
			{name: "s", typ: tar.TypeSymlink, link: "OUTSIDE/escaped"}, {name: "s", body: "x"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			top := t.TempDir()
			outside := filepath.Join(top, "outside")
			if err := os.Mkdir(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(top, "outside-file"), []byte("secret"), 0o644); err != nil {
				t.Fatal(err)
			}
			for i := range entries {
				entries[i].link = strings.ReplaceAll(entries[i].link, "OUTSIDE", outside)
			}
			dir := filepath.Join(top, "a", "root")
			if err := os.Mkdir(filepath.Dir(dir), 0o755); err != nil {
				t.Fatal(err)
			}
			_ = extract(bytes.NewReader(archive(t, entries...)), dir)
			for _, p := range []string{filepath.Join(top, "escaped"), filepath.Join(top, "a", "escaped"), filepath.Join(outside, "escaped")} {
				if _, err := os.Lstat(p); !os.IsNotExist(err) {
					t.Errorf("%s was written: %v", p, err)
				}
			}
			if fi, err := os.Stat(filepath.Join(top, "outside-file")); err != nil || fi.Sys() == nil {
				t.Fatal(err)
			}
			if got := readFile(t, filepath.Join(top, "outside-file")); got != "secret" {
				t.Errorf("outside-file = %q", got)
			}
		})
	}
}

func TestExtractShortArchive(t *testing.T) {
	t.Parallel()
	b := archive(t, entry{name: "f", body: strings.Repeat("x", 4096)})
	err := extract(bytes.NewReader(b[:1024]), filepath.Join(t.TempDir(), "root"))
	if err == nil {
		t.Fatal("a truncated archive extracted without error")
	}
}

func layer(t *testing.T, entries ...entry) v1.Layer {
	t.Helper()
	b := archive(t, entries...)
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// push serves img from an in-process registry and returns its reference.
func push(t *testing.T, img v1.Image) string {
	t.Helper()
	return pushTo(t, serveRegistry(t, nil), img)
}

// serveRegistry serves an in-process registry, through wrap when it is set,
// and returns its host.
func serveRegistry(t *testing.T, wrap func(http.Handler) http.Handler) string {
	t.Helper()
	h := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// pushTo pushes img to the registry at host and returns its reference.
func pushTo(t *testing.T, host string, img v1.Image) string {
	t.Helper()
	ref := host + "/test/image:1"
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(r, img); err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestAcquireFlattensLayers(t *testing.T) {
	t.Parallel()
	img, err := mutate.AppendLayers(empty.Image,
		layer(t, entry{name: "etc/os-release", body: "lower"}, entry{name: "gone", body: "x"}, entry{name: "keep", body: "k"}),
		layer(t, entry{name: "etc/os-release", body: "upper"}, entry{name: ".wh.gone"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.Config(img, v1.Config{Env: []string{"PATH=/usr/bin:/bin", "A=b"}})
	if err != nil {
		t.Fatal(err)
	}
	ref := push(t, img)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	root := filepath.Join(t.TempDir(), "root")
	c := newCache(t)
	got, err := c.Acquire(ctx, Request{Job: "job-1", Ref: ref, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	want, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != want.String() {
		t.Errorf("digest = %s, want %s", got.Digest, want)
	}
	if strings.Join(got.Env, " ") != "PATH=/usr/bin:/bin A=b" {
		t.Errorf("env = %q", got.Env)
	}
	if s := readFile(t, filepath.Join(root, "etc/os-release")); s != "upper" {
		t.Errorf("etc/os-release = %q, want the upper layer's", s)
	}
	if s := readFile(t, filepath.Join(root, "keep")); s != "k" {
		t.Errorf("keep = %q", s)
	}
	if _, err := os.Lstat(filepath.Join(root, "gone")); !os.IsNotExist(err) {
		t.Errorf("a whited-out file is in the root: %v", err)
	}
	var names []string
	_ = filepath.WalkDir(root, func(p string, _ fs.DirEntry, _ error) error {
		names = append(names, p)
		return nil
	})
	for _, n := range names {
		if strings.Contains(n, ".wh.") {
			t.Errorf("whiteout file extracted: %s", n)
		}
	}
}

func TestAcquireFails(t *testing.T) {
	t.Parallel()
	c := newCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	for name, ref := range map[string]string{
		"malformed reference": "UPPER/case::x",
		"missing image":       strings.TrimPrefix(srv.URL, "http://") + "/no/such:1",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := c.Acquire(ctx, Request{Job: "job-1", Ref: ref, Root: filepath.Join(t.TempDir(), "root")}); err == nil {
				t.Fatal("Acquire succeeded")
			}
		})
	}
	// A cancelled pull returns.
	cctx, ccancel := context.WithCancel(ctx)
	ccancel()
	if _, err := c.Acquire(cctx, Request{Job: "job-2", Ref: strings.TrimPrefix(srv.URL, "http://") + "/x:1", Root: filepath.Join(t.TempDir(), "root")}); err == nil {
		t.Fatal("a cancelled Acquire succeeded")
	}
}
