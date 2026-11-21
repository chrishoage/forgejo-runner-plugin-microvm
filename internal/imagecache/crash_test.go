// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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
