// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// reopen starts the image cache again over dir, as the plugin's next start
// does after a crash, with a cap of maxSize bytes, and returns it and its
// log.
func reopen(t *testing.T, dir string, maxSize int64) (*Cache, *lockedBuffer) {
	t.Helper()
	var log lockedBuffer
	c, err := New(slog.New(slog.NewTextHandler(&log, nil)), dir, WithMaxSize(maxSize))
	if err != nil {
		t.Fatal(err)
	}
	return c, &log
}

// write writes b to p, making its directory.
func write(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Starting over a cache a crash left mid-pull deletes its temp files and
// the link directories of jobs that no longer exist, keeps its complete
// layers, and the next Acquire converts only the layers that weren't
// complete (IMG-13).
func TestStartupDeletesCrashLeftovers(t *testing.T) {
	t.Parallel()
	done, partial := headerLayer(t, file("done")), headerLayer(t, file("partial"))
	ctx := testCtx(t)
	dir := t.TempDir()
	before, _ := reopen(t, dir, 1<<40)
	// done is complete and still linked by job-gone, which the crash
	// ended; partial was mid-conversion.
	acquire(ctx, t, before, "job-gone", push(t, image(t, done)))
	write(t, before.path(tmpDir, diffID(t, partial).Hex+".erofs-123"), []byte("half a layer"))
	write(t, before.path(tmpDir, "config.json-456"), []byte("{"))
	if err := os.MkdirAll(before.path(jobsDir, "job-gone", "1"), 0o700); err != nil {
		t.Fatal(err)
	}

	c, _ := reopen(t, dir, 1<<40)
	for _, d := range []string{tmpDir, jobsDir} {
		if got := names(t, c.path(d)); len(got) != 0 {
			t.Errorf("%s/ holds %v after startup", d, got)
		}
	}
	if got, want := cachedLayers(t, c), diffIDs(t, done); !slices.Equal(got, want) {
		t.Fatalf("layers/ holds %v, want %v", got, want)
	}
	n := countConversions(c)
	acquire(ctx, t, c, "job-next", push(t, image(t, done, partial)))
	if n.Load() != 1 {
		t.Errorf("%d conversions, want 1: only the layer that wasn't complete", n.Load())
	}
}

// Layers that only the link directories of jobs a crash ended keep in use
// are evicted at startup when the cache is over its cap, least recently
// used first, and their images' configs and tags are pruned.
func TestStartupEvictsLayersOnlyLeftoversHeld(t *testing.T) {
	t.Parallel()
	old, recent := sizedLayer(t, "old"), sizedLayer(t, "recent")
	imgOld, imgRecent := image(t, old), image(t, recent)
	refOld, refRecent := push(t, imgOld), push(t, imgRecent)
	ctx := testCtx(t)
	dir := t.TempDir()
	before, _ := reopen(t, dir, 1<<40)
	// The crash came before either job's Release.
	acquire(ctx, t, before, "job-old", refOld)
	acquire(ctx, t, before, "job-recent", refRecent)
	hour := time.Now().Add(-time.Hour)
	if err := os.Chtimes(before.layerPath(diffID(t, old)), hour, hour); err != nil {
		t.Fatal(err)
	}

	// The cap holds everything but old and its config.
	limit := cacheSize(t, before) - allocated(t, before.layerPath(diffID(t, old))) - allocated(t, before.imagePath(imageDigest(t, imgOld)))
	c, log := reopen(t, dir, limit)
	if got, want := cachedLayers(t, c), diffIDs(t, recent); !slices.Equal(got, want) {
		t.Errorf("layers/ holds %v, want %v", got, want)
	}
	if !strings.Contains(log.String(), "evicted layer="+diffID(t, old).String()) {
		t.Errorf("no eviction of old logged:\n%s", log)
	}
	if _, err := os.Stat(c.imagePath(imageDigest(t, imgOld))); err == nil {
		t.Error("the evicted image kept its config")
	}
	if _, err := os.Stat(c.imagePath(imageDigest(t, imgRecent))); err != nil {
		t.Errorf("the cached image's config: %v", err)
	}
	if got := len(names(t, c.path(tagsDir))); got != 1 {
		t.Errorf("tags/ holds %d tags, want 1", got)
	}
}

// Startup prunes even when it evicts nothing: a crash between an eviction
// and its pruning leaves configs with no layer cached, and their tags.
func TestStartupPrunes(t *testing.T) {
	t.Parallel()
	l := headerLayer(t, file("l"))
	img := image(t, l)
	ctx := testCtx(t)
	dir := t.TempDir()
	before, _ := reopen(t, dir, 1<<40)
	acquire(ctx, t, before, "job-1", push(t, img))
	release(ctx, t, before, "job-1")
	if err := os.Remove(before.layerPath(diffID(t, l))); err != nil {
		t.Fatal(err)
	}

	c, _ := reopen(t, dir, 1<<40)
	for _, d := range []string{imagesDir, tagsDir} {
		if got := names(t, c.path(d)); len(got) != 0 {
			t.Errorf("%s/ holds %v after startup", d, got)
		}
	}
}

// Startup cleanup never leaves the cache's own directories: a file in
// layers/, images/ or tags/ that is not the cache's is kept.
func TestStartupKeepsTheStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	before, _ := reopen(t, dir, 1<<40)
	var keep []string
	for _, d := range []string{layersDir, imagesDir} {
		p := before.path(d, "not-the-caches")
		write(t, p, []byte("x"))
		keep = append(keep, p)
	}
	reopen(t, dir, 1)
	for _, p := range keep {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("startup deleted %s: %v", p, err)
		}
	}
}

// A layer deleted from under the image cache is converted again at its next
// use (IMG-15): deleted between two Acquires, or between an Acquire's look
// at the cache and its link, which then retries once.
func TestDeletedLayerIsConvertedAgain(t *testing.T) {
	t.Parallel()
	for name, deleteAtLink := range map[string]bool{"between Acquires": false, "at link": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			x, y := headerLayer(t, file("x")), headerLayer(t, file("y"))
			ref := push(t, image(t, x, y))
			c := newCache(t)
			ctx := testCtx(t)
			acquire(ctx, t, c, "job-1", ref)
			release(ctx, t, c, "job-1")
			n := countConversions(c)
			gone := c.layerPath(diffID(t, x))
			if deleteAtLink {
				var fired atomic.Bool
				c.beforeLink = func() {
					if fired.CompareAndSwap(false, true) {
						if err := os.Remove(gone); err != nil {
							t.Error(err)
						}
					}
				}
			} else if err := os.Remove(gone); err != nil {
				t.Fatal(err)
			}
			img := acquire(ctx, t, c, "job-2", ref)
			if n.Load() != 1 {
				t.Errorf("%d conversions, want 1: x again", n.Load())
			}
			link, err := os.Stat(filepath.Join(img.LinkDir, img.Layers[0]))
			if err != nil {
				t.Fatal(err)
			}
			cached, err := os.Stat(gone)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(link, cached) {
				t.Error("0.erofs is not a hard link to x as cached")
			}
			unpinned(ctx, t, c)
		})
	}
}

// A layer deleted again during the single retry fails the Acquire, naming
// the layer and the cache directory, after one more conversion and with no
// link directory or temp file left.
func TestLayerDeletedTwiceFailsAcquire(t *testing.T) {
	t.Parallel()
	x := headerLayer(t, file("x"))
	ref := push(t, image(t, x))
	c := newCache(t)
	ctx := testCtx(t)
	acquire(ctx, t, c, "job-1", ref)
	release(ctx, t, c, "job-1")
	n := countConversions(c)
	gone := c.layerPath(diffID(t, x))
	var links atomic.Int32
	c.beforeLink = func() {
		links.Add(1)
		if err := os.Remove(gone); err != nil {
			t.Error(err)
		}
	}
	_, err := c.Acquire(ctx, Request{Job: "job-2", Ref: ref})
	if err == nil {
		t.Fatal("Acquire succeeded with its layer deleted at every link")
	}
	for _, want := range []string{diffID(t, x).String(), c.dir} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
	if links.Load() != 2 || n.Load() != 1 {
		t.Errorf("%d links tried and %d conversions, want 2 and 1: one retry", links.Load(), n.Load())
	}
	if got := names(t, c.path(tmpDir)); len(got) != 0 {
		t.Errorf("tmp/ holds %v", got)
	}
	if _, err := os.Stat(c.path(jobsDir, "job-2", "0")); err == nil {
		t.Error("the failed Acquire left its link directory")
	}
	unpinned(ctx, t, c)
}

// A retry after a layer was deleted from under the cache never joins a
// pull another Acquire still holds (IMG-15). Acquires A and C share the pull
// of layer x; A stores x and returns, while C still holds that pull, waiting
// on its slow layer z. x is then deleted. Acquire B, of A's image, joins the
// held pull, which has stored x, so its link misses x; its retry must pull x
// again rather than join that pull once more.
func TestRetryDoesNotJoinAHeldPull(t *testing.T) {
	t.Parallel()
	reg := newHoldingRegistry(t)
	x, z := headerLayer(t, file("x")), headerLayer(t, file("z"))
	// C's image is in test/image, whose blobs reg.hold names.
	refC := pushTo(t, reg.host, image(t, x, z))
	refA := reg.host + "/test/a:1"
	r, err := name.ParseReference(refA)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(r, image(t, x)); err != nil {
		t.Fatal(err)
	}
	zPath := reg.hold(t, z)[0]
	c := newCache(t)
	ctx := testCtx(t)
	// x's first conversion waits for gate, so A and C both join its pull.
	gate := make(chan struct{})
	var conversions atomic.Int32
	c.convert = func(out io.Writer, r io.Reader, tmp string) error {
		if conversions.Add(1) == 1 {
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return convert(out, r, tmp)
	}
	xKey := diffID(t, x).String()
	xJoins := make(chan int, 16)
	c.layers.mu.Lock()
	c.layers.joined = func(key string, refs int) {
		if key == xKey {
			xJoins <- refs
		}
	}
	c.layers.mu.Unlock()

	errA, errC := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := c.Acquire(ctx, Request{Job: "job-a", Ref: refA})
		errA <- err
	}()
	go func() {
		_, err := c.Acquire(ctx, Request{Job: "job-c", Ref: refC})
		errC <- err
	}()
	for n := 0; n != 2; {
		n = recv(ctx, t, xJoins, "a waiter to join x's pull")
	}
	if p := recv(ctx, t, reg.entered, "C's request for z"); p != zPath {
		t.Fatalf("request for %s, want %s", p, zPath)
	}
	close(gate)
	if err := recv(ctx, t, errA, "A's Acquire"); err != nil {
		t.Fatal(err)
	}
	// C still holds x's pull: z is held.
	if err := os.Remove(c.layerPath(diffID(t, x))); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acquire(ctx, Request{Job: "job-b", Ref: refA}); err != nil {
		t.Errorf("B's Acquire: %v", err)
	}
	if got := conversions.Load(); got != 2 {
		t.Errorf("%d conversions before z arrived, want 2: x, then x again", got)
	}
	reg.unhold()
	if err := recv(ctx, t, errC, "C's Acquire"); err != nil {
		t.Errorf("C's Acquire: %v", err)
	}
	unpinned(ctx, t, c)
	if got := names(t, c.path(tmpDir)); len(got) != 0 {
		t.Errorf("tmp/ holds %v", got)
	}
}
