// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/containerd/continuity/tarconv"
	erofs "github.com/erofs/go-erofs"
	"github.com/google/go-containerregistry/pkg/compression"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

func newCache(t *testing.T) *Cache {
	t.Helper()
	c, err := New(slog.New(slog.DiscardHandler), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// recv returns the next value from ch, failing t at ctx's deadline.
func recv[T any](ctx context.Context, t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		t.Fatalf("waiting for %s: %v", what, ctx.Err())
		var zero T
		return zero
	}
}

// image returns an image of layers, bottom first.
func image(t *testing.T, layers ...v1.Layer) v1.Image {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, layers...)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// headerLayer returns a layer of the given tar headers, each regular file
// holding its name.
func headerLayer(t *testing.T, hdrs ...*tar.Header) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hdrs {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(h.Name))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(h.Name)); err != nil {
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

func file(name string) *tar.Header {
	return &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}
}

func diffID(t *testing.T, l v1.Layer) v1.Hash {
	t.Helper()
	d, err := l.DiffID()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// names lists dir.
func names(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func lstat(t *testing.T, fsys fs.FS, name string) (fs.FileInfo, *erofs.Stat) {
	t.Helper()
	fi, err := fs.Lstat(fsys, name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	st, ok := fi.Sys().(*erofs.Stat)
	if !ok {
		t.Fatalf("%s: Sys() is %T", name, fi.Sys())
	}
	return fi, st
}

// A layer read back from its EROFS file keeps everything a tar carries, and
// its OCI whiteouts become overlay whiteouts (IMG-4).
func TestLayerKeepsWhatTheTarCarries(t *testing.T) {
	t.Parallel()
	capability := "\x01\x00\x00\x02\x00\x20\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"
	l := headerLayer(t,
		&tar.Header{Name: "dev/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "opq/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "owned", Typeflag: tar.TypeReg, Mode: 0o640, Uid: 123, Gid: 456},
		&tar.Header{Name: "suid", Typeflag: tar.TypeReg, Mode: 0o4755},
		&tar.Header{Name: "ping", Typeflag: tar.TypeReg, Mode: 0o755, Format: tar.FormatPAX, PAXRecords: map[string]string{
			"SCHILY.xattr.security.capability": capability,
			"SCHILY.xattr.user.note":           "hi",
		}},
		&tar.Header{Name: "dev/null", Typeflag: tar.TypeChar, Mode: 0o666, Devmajor: 1, Devminor: 3},
		&tar.Header{Name: "fifo", Typeflag: tar.TypeFifo, Mode: 0o600},
		&tar.Header{Name: "hardlink", Typeflag: tar.TypeLink, Linkname: "owned"},
		file(".wh.gone"),
		file("opq/.wh..wh..opq"),
		file("opq/new"),
	)
	c := newCache(t)
	got, err := c.Acquire(testCtx(t), Request{Job: "job-1", Ref: push(t, image(t, l))})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(got.LinkDir, "0.erofs"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	img, err := erofs.Open(f)
	if err != nil {
		t.Fatal(err)
	}

	fi, owned := lstat(t, img, "owned")
	if owned.UID != 123 || owned.GID != 456 || fi.Mode().Perm() != 0o640 {
		t.Errorf("owned: %d:%d %v, want 123:456 0640", owned.UID, owned.GID, fi.Mode())
	}
	if b, err := fs.ReadFile(img, "owned"); err != nil || string(b) != "owned" {
		t.Errorf("owned holds %q, %v", b, err)
	}
	if fi, _ := lstat(t, img, "suid"); fi.Mode()&fs.ModeSetuid == 0 || fi.Mode().Perm() != 0o755 {
		t.Errorf("suid: %v, want setuid 0755", fi.Mode())
	}
	if _, ping := lstat(t, img, "ping"); ping.Xattrs["security.capability"] != capability || ping.Xattrs["user.note"] != "hi" {
		t.Errorf("ping's xattrs: %q", ping.Xattrs)
	}
	if fi, null := lstat(t, img, "dev/null"); fi.Mode()&fs.ModeCharDevice == 0 ||
		unix.Major(uint64(null.Rdev)) != 1 || unix.Minor(uint64(null.Rdev)) != 3 {
		t.Errorf("dev/null: %v, rdev %#x; want the char device 1:3", fi.Mode(), null.Rdev)
	}
	if fi, _ := lstat(t, img, "fifo"); fi.Mode()&fs.ModeNamedPipe == 0 {
		t.Errorf("fifo: %v", fi.Mode())
	}
	if _, hl := lstat(t, img, "hardlink"); hl.Ino != owned.Ino || hl.Nlink != 2 {
		t.Errorf("hardlink: inode %d with %d links, want owned's %d with 2", hl.Ino, hl.Nlink, owned.Ino)
	}
	// An overlay whiteout is a char device 0:0.
	if fi, gone := lstat(t, img, "gone"); fi.Mode()&fs.ModeCharDevice == 0 || gone.Rdev != 0 {
		t.Errorf("gone: %v, rdev %#x; want an overlay whiteout", fi.Mode(), gone.Rdev)
	}
	if _, opq := lstat(t, img, "opq"); opq.Xattrs["trusted.overlay.opaque"] != "y" {
		t.Errorf("opq's xattrs: %q, want the overlay's opaque marker", opq.Xattrs)
	}
	es, err := fs.ReadDir(img, "opq")
	if err != nil || len(es) != 1 || es[0].Name() != "new" {
		t.Errorf("opq holds %v, %v; want only new", es, err)
	}
	for _, n := range []string{".wh.gone", "opq/.wh..wh..opq"} {
		if _, err := fs.Lstat(img, n); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is in the layer: %v", n, err)
		}
	}
	// The cache's own files are the plugin user's alone.
	for _, p := range []string{c.path(layersDir), c.path(tmpDir), c.path(jobsDir), got.LinkDir, c.layerPath(diffID(t, l))} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if fi.IsDir() {
			want = 0o700
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s has mode %v, want %v", p, fi.Mode().Perm(), want)
		}
	}
}

// countConversions makes c count its layer conversions.
func countConversions(c *Cache) *atomic.Int32 {
	var n atomic.Int32
	c.convert = func(out io.Writer, r io.Reader, tmp string) error {
		n.Add(1)
		return convert(out, r, tmp)
	}
	return &n
}

// Two images that share a layer store it once, converted once (IMG-5).
func TestSharedLayerIsStoredOnce(t *testing.T) {
	t.Parallel()
	base, a, b := headerLayer(t, file("base")), headerLayer(t, file("a")), headerLayer(t, file("b"))
	c := newCache(t)
	n := countConversions(c)
	ctx := testCtx(t)
	for job, img := range map[string]v1.Image{"job-a": image(t, base, a), "job-b": image(t, base, b)} {
		if _, err := c.Acquire(ctx, Request{Job: job, Ref: push(t, img)}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{}
	for _, l := range []v1.Layer{base, a, b} {
		want = append(want, diffID(t, l).Hex+".erofs")
	}
	slices.Sort(want)
	if got := names(t, c.path(layersDir)); !slices.Equal(got, want) {
		t.Errorf("layers/ holds %v, want %v", got, want)
	}
	if n.Load() != 3 {
		t.Errorf("%d conversions, want 3", n.Load())
	}
}

// A VM's layer link directory holds exactly its image's layers, in order,
// a layer that appears twice included (IMG-8).
func TestLinkDirHoldsTheImagesLayers(t *testing.T) {
	t.Parallel()
	x, y := headerLayer(t, file("x")), headerLayer(t, file("y"))
	c := newCache(t)
	n := countConversions(c)
	got, err := c.Acquire(testCtx(t), Request{Job: "job-1", VM: 2, Ref: push(t, image(t, x, y, x))})
	if err != nil {
		t.Fatal(err)
	}
	if got.LinkDir != c.path(jobsDir, "job-1", "2") {
		t.Errorf("link directory %s", got.LinkDir)
	}
	want := []string{"0.erofs", "1.erofs", "2.erofs"}
	if !slices.Equal(got.Layers, want) || !slices.Equal(names(t, got.LinkDir), want) {
		t.Fatalf("layers %v, link directory %v; want %v", got.Layers, names(t, got.LinkDir), want)
	}
	for i, l := range []v1.Layer{x, y, x} {
		link, err := os.Stat(filepath.Join(got.LinkDir, want[i]))
		if err != nil {
			t.Fatal(err)
		}
		cached, err := os.Stat(c.layerPath(diffID(t, l)))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(link, cached) {
			t.Errorf("%s is not a hard link to its cached layer", want[i])
		}
	}
	// x is in use twice: the cache's name and two links.
	if fi, _ := os.Stat(c.layerPath(diffID(t, x))); fi.Sys().(*syscall.Stat_t).Nlink != 3 {
		t.Errorf("x has %d links, want 3", fi.Sys().(*syscall.Stat_t).Nlink)
	}
	if n.Load() != 2 {
		t.Errorf("%d conversions, want 2", n.Load())
	}
}

// An Acquire whose link fails part way, here on a layer deleted from under
// the cache as soon as it is stored, deletes the link directory it began.
func TestFailedLinkDeletesLinkDir(t *testing.T) {
	t.Parallel()
	x, y := headerLayer(t, file("x")), headerLayer(t, file("y"))
	c := newCache(t)
	gone := c.layerPath(diffID(t, y))
	c.rename = func(oldpath, newpath string) error {
		if newpath == gone {
			return os.Remove(oldpath)
		}
		return renameNoReplace(oldpath, newpath)
	}
	if _, err := c.Acquire(testCtx(t), Request{Job: "job-1", Ref: push(t, image(t, x, y))}); err == nil {
		t.Fatal("Acquire succeeded with a layer deleted before its link")
	}
	if _, err := os.Lstat(c.path(jobsDir, "job-1", "0")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the failed Acquire left its link directory: %v", err)
	}
}

// emptyCache fails t unless the cache holds no layer, image config, tag or
// temp file, and no job has a link directory.
func emptyCache(t *testing.T, c *Cache) {
	t.Helper()
	for _, d := range []string{layersDir, imagesDir, tagsDir, tmpDir, jobsDir} {
		if got := names(t, c.path(d)); len(got) != 0 {
			t.Errorf("%s/ holds %v", d, got)
		}
	}
}

// A hard link to a file in an earlier layer fails Acquire with the typed
// error, and leaves nothing in the cache (IMG-10).
func TestCrossLayerHardLinkFails(t *testing.T) {
	t.Parallel()
	c := newCache(t)
	img := image(t,
		headerLayer(t, file("target")),
		headerLayer(t, &tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "target"}),
	)
	_, err := c.Acquire(testCtx(t), Request{Job: "job-1", Ref: push(t, img)})
	var hl *HardLinkError
	if !errors.As(err, &hl) || !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("err = %v, want a HardLinkError saying the cache does not support it", err)
	}
	emptyCache(t, c)
}

// tarconv has no typed error for a hard link whose target is not in the
// layer, so the image cache matches its text. This pins it: a tarconv
// upgrade that changes the wording fails here.
func TestTarconvHardLinkText(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "target"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	err := tarconv.Apply(erofs.Create(io.Discard), &buf)
	if err == nil || !strings.Contains(err.Error(), tarconvHardLink+`"/link"`) {
		t.Fatalf("tarconv's error = %v, want it to contain %q", err, tarconvHardLink+`"/link"`)
	}
}

// panicReader is a decompressor that panics as it is read.
type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("inflate") }

// otherDiffID is a layer whose config entry names another layer's content.
type otherDiffID struct {
	v1.Layer
	diffID v1.Hash
}

func (l otherDiffID) DiffID() (v1.Hash, error) { return l.diffID, nil }

// A malformed layer, a layer whose content doesn't match its diffID, a
// converter that panics and a full disk each fail only their Acquire, and
// leave nothing behind; the next Acquire succeeds (IMG-11).
func TestBadLayerFailsOnlyItsAcquire(t *testing.T) {
	t.Parallel()
	b := []byte(strings.Repeat("not a tar header ", 64))
	garbage, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
	if err != nil {
		t.Fatal(err)
	}
	good := headerLayer(t, file("good"))
	c := newCache(t)
	ctx := testCtx(t)
	for name, tt := range map[string]struct {
		img        v1.Image
		convert    convertFunc
		decompress decompressFunc
		want       string
	}{
		"malformed":       {img: image(t, garbage), want: "tarconv"},
		"wrong diffID":    {img: image(t, otherDiffID{headerLayer(t, file("evil")), diffID(t, good)}), want: "its content has the digest"},
		"converter panic": {img: image(t, good), convert: func(io.Writer, io.Reader, string) error { panic("boom") }, want: "panicked: boom"},
		"decompressor panic": {img: image(t, good), want: "panicked: inflate", decompress: func(io.Reader) (io.ReadCloser, error) {
			return io.NopCloser(panicReader{}), nil
		}},
		"full disk": {img: image(t, good), want: "out of space", convert: func(io.Writer, io.Reader, string) error {
			return fmt.Errorf("write: %w", syscall.ENOSPC)
		}},
	} {
		c.convert, c.decompress = convert, decompress
		if tt.convert != nil {
			c.convert = tt.convert
		}
		if tt.decompress != nil {
			c.decompress = tt.decompress
		}
		_, err := c.Acquire(ctx, Request{Job: "job-1", Ref: push(t, tt.img)})
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Fatalf("%s: err = %v, want one with %q", name, err, tt.want)
		}
		if tt.want == "out of space" && !strings.Contains(err.Error(), c.dir) {
			t.Errorf("%s: err = %v, want it to name the cache directory", name, err)
		}
		emptyCache(t, c)
	}
	c.convert, c.decompress = convert, decompress
	if _, err := c.Acquire(ctx, Request{Job: "job-1", Ref: push(t, image(t, good))}); err != nil {
		t.Fatalf("the Acquire after the failures: %v", err)
	}
}

// At most maxPulls layers convert at once.
func TestPullLimit(t *testing.T) {
	t.Parallel()
	var layers []v1.Layer
	for i := range maxPulls + 2 {
		layers = append(layers, headerLayer(t, file(fmt.Sprint(i))))
	}
	c := newCache(t)
	var active, most atomic.Int32
	entered := make(chan struct{}, len(layers))
	gate := make(chan struct{})
	c.convert = func(out io.Writer, r io.Reader, tmp string) error {
		n := active.Add(1)
		defer active.Add(-1)
		for m := most.Load(); n > m; m = most.Load() {
			if most.CompareAndSwap(m, n) {
				break
			}
		}
		entered <- struct{}{}
		<-gate
		return convert(out, r, tmp)
	}
	ctx := testCtx(t)
	ref := push(t, image(t, layers...))
	done := make(chan error, 1)
	go func() {
		_, err := c.Acquire(ctx, Request{Job: "job-1", Ref: ref})
		done <- err
	}()
	for range maxPulls {
		recv(ctx, t, entered, "a conversion")
	}
	close(gate)
	if err := recv(ctx, t, done, "Acquire"); err != nil {
		t.Fatal(err)
	}
	if most.Load() > maxPulls {
		t.Errorf("%d layers converted at once, want at most %d", most.Load(), maxPulls)
	}
}

// Release deletes the job's link directories, and waits for an Acquire of
// the job in flight, so the Acquire can't make one after the delete.
func TestReleaseWaitsForAcquire(t *testing.T) {
	t.Parallel()
	var blocking atomic.Bool
	entered, unblock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	host := serveRegistry(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if blocking.Load() && strings.Contains(r.URL.Path, "/blobs/") {
				once.Do(func() { close(entered) })
				select {
				case <-unblock:
				case <-r.Context().Done():
				}
			}
			h.ServeHTTP(w, r)
		})
	})
	ref := pushTo(t, host, image(t, headerLayer(t, file("f"))))
	blocking.Store(true)
	c := newCache(t)
	ctx := testCtx(t)
	if err := c.Release(ctx, "job-0"); err != nil {
		t.Fatalf("Release of a job with no link directory: %v", err)
	}

	acquired := make(chan error, 1)
	go func() {
		_, err := c.Acquire(ctx, Request{Job: "job-1", Ref: ref})
		acquired <- err
	}()
	recv(ctx, t, entered, "the blob request")
	waiting := make(chan struct{})
	var wonce sync.Once
	c.mu.Lock()
	c.releaseWaits = func() { wonce.Do(func() { close(waiting) }) }
	c.mu.Unlock()
	released := make(chan error, 1)
	go func() { released <- c.Release(ctx, "job-1") }()
	recv(ctx, t, waiting, "Release to wait")
	select {
	case err := <-released:
		t.Fatalf("Release returned while an Acquire was in flight: %v", err)
	default:
	}
	close(unblock)
	if err := recv(ctx, t, acquired, "Acquire"); err != nil {
		t.Fatal(err)
	}
	if err := recv(ctx, t, released, "Release"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.path(jobsDir, "job-1")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the job's link directories outlived Release: %v", err)
	}
}

// Acquire and Release refuse a job id that is not one path component.
func TestJobMustBeAName(t *testing.T) {
	t.Parallel()
	c := newCache(t)
	for _, job := range []string{"", ".", "..", "a/b", "../x"} {
		if _, err := c.Acquire(testCtx(t), Request{Job: job, Ref: "example.com/x:1"}); err == nil {
			t.Errorf("Acquire of job %q ran", job)
		}
		if err := c.Release(testCtx(t), job); err == nil {
			t.Errorf("Release of job %q ran", job)
		}
	}
}

// A layer lookup that fails returns before any layer converts, so nothing
// runs on after pull returns, holds a pull slot or leaves a temp file.
func TestLayerLookupFailureStartsNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := newCache(t)
		a, b := headerLayer(t, file("a")), headerLayer(t, file("b"))
		// b's cache entry is a symlink to itself, so looking it up fails
		// with ELOOP.
		bad := c.layerPath(diffID(t, b))
		if err := os.Symlink(filepath.Base(bad), bad); err != nil {
			t.Fatal(err)
		}
		var started atomic.Int32
		gate := make(chan struct{})
		c.convert = func(out io.Writer, r io.Reader, tmp string) error {
			started.Add(1)
			<-gate
			return convert(out, r, tmp)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		layers := []v1.Layer{a, b}
		err := c.pull(ctx, cancel, []v1.Hash{diffID(t, a), diffID(t, b)}, func(i int) (v1.Layer, error) { return layers[i], nil })
		if !errors.Is(err, syscall.ELOOP) {
			t.Errorf("pull: %v, want ELOOP", err)
		}
		// Whatever pull started runs until it blocks.
		synctest.Wait()
		if n := started.Load(); n != 0 {
			t.Errorf("%d conversions still running after pull returned", n)
		}
		if n := len(c.pulls); n != 0 {
			t.Errorf("%d pull slots held after pull returned", n)
		}
		close(gate)
		synctest.Wait()
		if got := names(t, c.path(tmpDir)); len(got) != 0 {
			t.Errorf("tmp/ holds %v", got)
		}
	})
}

// A cache directory whose filesystem lacks RENAME_NOREPLACE fails Acquire
// with an error naming the directory and the missing support, and leaves
// nothing in the cache.
func TestRenameNoReplaceUnsupported(t *testing.T) {
	t.Parallel()
	for _, errno := range []syscall.Errno{unix.EINVAL, unix.EOPNOTSUPP} {
		c := newCache(t)
		c.rename = func(string, string) error { return errno }
		_, err := c.Acquire(testCtx(t), Request{Job: "job-1", Ref: push(t, image(t, headerLayer(t, file("f"))))})
		if err == nil || !strings.Contains(err.Error(), c.dir) || !strings.Contains(err.Error(), "RENAME_NOREPLACE") || !errors.Is(err, errno) {
			t.Errorf("%v: err = %v, want one naming %s and RENAME_NOREPLACE", errno, err, c.dir)
		}
		emptyCache(t, c)
	}
}

// A layer over its entry or metadata limit fails only its Acquire, saying
// which, and leaves nothing in the cache; one at the limits converts.
func TestLayerLimits(t *testing.T) {
	t.Parallel()
	xattr := func(name string, n int) *tar.Header {
		return &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Format: tar.FormatPAX, PAXRecords: map[string]string{
			"SCHILY.xattr.user.big": strings.Repeat("x", n),
		}}
	}
	// Each name is 1 byte and the record's key 21: "a" and "b" with 999
	// bytes of xattr are exactly meta.
	const meta = 1 + 1 + 21 + 999
	for name, tt := range map[string]struct {
		layer v1.Layer
		want  string
	}{
		"at the limits":    {layer: headerLayer(t, file("a"), xattr("b", 999))},
		"too many entries": {layer: headerLayer(t, file("a"), file("b"), file("c")), want: "more than 2 entries"},
		"too much metadata": {
			layer: headerLayer(t, xattr("b", 1001)),
			want:  fmt.Sprintf("more than %d bytes of names, link targets and PAX records", meta),
		},
	} {
		c := newCache(t)
		c.limits = tarLimits{entries: 2, metadata: meta}
		_, err := c.Acquire(testCtx(t), Request{Job: "job-1", Ref: push(t, image(t, tt.layer))})
		if tt.want == "" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want one with %q", name, err, tt.want)
		}
		emptyCache(t, c)
	}
}

// Layers compressed with zstd, gzip or nothing all convert; the blob's first
// bytes tell them apart.
func TestDecompress(t *testing.T) {
	t.Parallel()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	if err := tw.WriteHeader(&tar.Header{Name: "f", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("f")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gz, zs bytes.Buffer
	gw := gzip.NewWriter(&gz)
	zw, err := zstd.NewWriter(&zs)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []io.WriteCloser{gw, zw} {
		if _, err := w.Write(raw.Bytes()); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for name, blob := range map[string][]byte{"none": raw.Bytes(), "gzip": gz.Bytes(), "zstd": zs.Bytes()} {
		rc, err := decompress(bytes.NewReader(blob))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := io.ReadAll(rc)
		if err != nil || !bytes.Equal(got, raw.Bytes()) {
			t.Errorf("%s: read %d bytes, %v; want the %d of the tar", name, len(got), err, raw.Len())
		}
		if err := rc.Close(); err != nil {
			t.Errorf("%s: close: %v", name, err)
		}
	}
	// A zstd layer pulled from a registry converts.
	b := raw.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil },
		tarball.WithCompression(compression.ZStd))
	if err != nil {
		t.Fatal(err)
	}
	c := newCache(t)
	if _, err := c.Acquire(testCtx(t), Request{Job: "job-1", Ref: push(t, image(t, l))}); err != nil {
		t.Fatal(err)
	}
}
