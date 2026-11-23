// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// The images these tests boot are built in the test and served to the plugin
// from go-containerregistry's in-process registry, never pulled by the
// plugin from a public registry.
var testRegistry struct {
	once sync.Once
	host string
	// base is the image the tests build on, in the in-process registry.
	base name.Reference
	err  error
}

// baseImage returns the image the tests build on, e2e/image, served from the
// in-process registry, whose host it also returns. It is copied into the
// registry once per test binary, so each test's own images, built in Go on
// top of it, are pushed and pulled locally.
func baseImage(t *testing.T) (v1.Image, string) {
	t.Helper()
	r := &testRegistry
	r.once.Do(func() {
		srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
		// The registry serves every test in the binary, and ends with it.
		r.host = strings.TrimPrefix(srv.URL, "http://")
		var src name.Reference
		if src, r.err = name.ParseReference(readImage(t)); r.err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var img v1.Image
		if img, r.err = remote.Image(src, remote.WithContext(ctx)); r.err != nil {
			return
		}
		if r.base, r.err = name.ParseReference(r.host + "/base:1"); r.err != nil {
			return
		}
		r.err = remote.Write(r.base, img, remote.WithContext(ctx))
	})
	if r.err != nil {
		t.Fatalf("the base image: %v", r.err)
	}
	img, err := remote.Image(r.base, remote.WithContext(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	return img, r.host
}

// pushImage pushes img to the in-process registry as repo:1 and returns its
// reference.
func pushImage(t *testing.T, host, repo string, img v1.Image) string {
	t.Helper()
	ref, err := name.ParseReference(host + "/" + repo + ":1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img, remote.WithContext(t.Context())); err != nil {
		t.Fatal(err)
	}
	return ref.String()
}

// tarLayer returns a layer of the tar headers, each regular file holding
// body.
func tarLayer(t *testing.T, body string, hdrs ...*tar.Header) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hdrs {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func regular(name string) *tar.Header {
	return &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}
}

// writeWorkflow writes a workflow_dispatch workflow of jobs, each `name:
// steps` as YAML, and returns its path.
func writeWorkflow(t *testing.T, jobs string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workflow.yml")
	body := "name: test\non: workflow_dispatch\njobs:\n" + jobs
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// step is one job on the microvm label running script, with `sh -e`.
func step(job, script string) string {
	indented := "          " + strings.ReplaceAll(strings.TrimSpace(script), "\n", "\n          ")
	return fmt.Sprintf("  %s:\n    runs-on: microvm\n    steps:\n      - shell: sh -e {0}\n        run: |\n%s\n", job, indented)
}

// expectOutput runs the workflow on image and fails t unless it succeeds
// with want in its log. Each want is computed by the step's shell, so the
// log's copy of the script never matches it.
func expectOutput(t *testing.T, workflow, image string, want ...string) {
	t.Helper()
	log, err := runWorkflow(t, workflow, map[string]string{"microvm": image})
	if err != nil {
		t.Fatalf("the workflow failed: %v", err)
	}
	for _, w := range want {
		if !strings.Contains(log, w) {
			t.Errorf("the job log does not show %q", w)
		}
	}
}

// An image of 120 one-file layers on the base boots: a step sees the top
// layer's file, every layer's file, and not a file a later layer deleted
// (GST-2, GST-3).
func TestManyLayersBoot(t *testing.T) {
	base, host := baseImage(t)
	var layers []v1.Layer
	for i := range 119 {
		hdrs := []*tar.Header{regular(fmt.Sprintf("layers/%d", i))}
		if i == 50 {
			hdrs = append(hdrs, regular("gone"))
		}
		layers = append(layers, tarLayer(t, "x", hdrs...))
	}
	layers = append(layers, tarLayer(t, "top", regular("top"), regular(".wh.gone")))
	img, err := mutate.AppendLayers(base, layers...)
	if err != nil {
		t.Fatal(err)
	}
	expectOutput(t, writeWorkflow(t, step("layers", `
test "$(cat /top)" = top
test ! -e /gone
echo "layer files: $(ls /layers | wc -l)"
echo "layers ok: $((6 * 7))"
`)), pushImage(t, host, "many-layers", img), "layer files: 119", "layers ok: 42")
}

// An image with no /proc, /sys or /dev boots, and the job still gets them
// (GST-2).
func TestImageWithoutMountPointsBoots(t *testing.T) {
	base, host := baseImage(t)
	img := imageWithout(t, base, "proc", "sys", "dev")
	expectOutput(t, writeWorkflow(t, step("bare", `
test -r /proc/self/status
test -d /sys/kernel
test -c /dev/null
echo "mount points ok: $((6 * 7))"
`)), pushImage(t, host, "no-mount-points", img), "mount points ok: 42")
}

// imageWithout returns base's files as one layer, less each path in drop
// and everything below it, with base's config.
func imageWithout(t *testing.T, base v1.Image, drop ...string) v1.Image {
	t.Helper()
	cfg, err := base.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	rc := mutate.Extract(base)
	defer func() { _ = rc.Close() }()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(hdr.Name, "./"), "/"), "/")
		if slices.ContainsFunc(drop, func(d string) bool { return name == d || strings.HasPrefix(name, d+"/") }) {
			continue
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(tw, tr); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, bytesLayer(t, buf.Bytes()))
	if err == nil {
		img, err = mutate.Config(img, cfg.Config)
	}
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// bytesLayer returns the tar b as a layer.
func bytesLayer(t *testing.T, b []byte) v1.Layer {
	t.Helper()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// A step can't write a layer through the layer share, also after
// remounting it read-write: the host enforces read-only (IMG-9).
func TestLayerShareIsReadOnly(t *testing.T) {
	base, host := baseImage(t)
	expectOutput(t, writeWorkflow(t, step("share", `
mkdir -p /mnt/layers
mount -t virtiofs layers /mnt/layers
test -s /mnt/layers/0.erofs
if echo x >> /mnt/layers/0.erofs; then exit 1; fi
if touch /mnt/layers/new; then exit 1; fi
mount -o remount,rw /mnt/layers || echo "remount refused"
if echo x >> /mnt/layers/0.erofs; then exit 1; fi
if touch /mnt/layers/new; then exit 1; fi
echo "share read-only: $((6 * 7))"
`)), pushImage(t, host, "share", base), "share read-only: 42")
}

// A file a job writes to / isn't there in the next job on the same image:
// each VM gets a fresh scratch disk (VM-5).
func TestNextJobStartsFresh(t *testing.T) {
	base, host := baseImage(t)
	second := strings.Replace(step("second", `
test ! -e /left-behind
echo "fresh root: $((6 * 7))"
`), "    runs-on: microvm\n", "    runs-on: microvm\n    needs: first\n", 1)
	expectOutput(t, writeWorkflow(t, step("first", `
echo here > /left-behind
echo "wrote: $(cat /left-behind)"
`)+second), pushImage(t, host, "fresh", base), "wrote: here", "fresh root: 42")
}
