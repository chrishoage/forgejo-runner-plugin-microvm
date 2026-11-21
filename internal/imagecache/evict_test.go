// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// lockedBuffer is a log destination that goroutines may share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// cappedCache returns an image cache whose cap is maxSize bytes, and its log.
func cappedCache(t *testing.T, maxSize int64) (*Cache, *lockedBuffer) {
	t.Helper()
	var log lockedBuffer
	c, err := New(slog.New(slog.NewTextHandler(&log, nil)), t.TempDir(), WithMaxSize(maxSize))
	if err != nil {
		t.Fatal(err)
	}
	return c, &log
}

// sizedLayer returns a layer of one 64 KiB file, name, so its EROFS file
// takes more than a block or two.
func sizedLayer(t *testing.T, name string) v1.Layer {
	t.Helper()
	body := bytes.Repeat([]byte(name), (64<<10)/len(name))
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
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

// allocated is the allocated size of the file at p.
func allocated(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Blocks * 512
}

// layerSizes converts layers in a cache of its own, on the same filesystem
// as the test's caches, and returns each one's allocated size.
func layerSizes(t *testing.T, layers ...v1.Layer) []int64 {
	t.Helper()
	c := newCache(t)
	if _, err := c.Acquire(testCtx(t), Request{Job: "probe", Ref: push(t, image(t, layers...))}); err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, l := range layers {
		out = append(out, allocated(t, c.layerPath(diffID(t, l))))
	}
	return out
}

// cacheSize is the allocated size of the layers, image configs and tags c
// holds: what its cap counts.
func cacheSize(t *testing.T, c *Cache) int64 {
	t.Helper()
	var n int64
	for _, d := range []string{layersDir, imagesDir, tagsDir} {
		for _, f := range names(t, c.path(d)) {
			n += allocated(t, c.path(d, f))
		}
	}
	return n
}

// setCap sets c's cap to n bytes. No conversion of c may be running.
func setCap(c *Cache, n int64) {
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	c.maxSize = n
}

// cachedLayers lists the layers c holds, by diffID.
func cachedLayers(t *testing.T, c *Cache) []v1.Hash {
	t.Helper()
	var out []v1.Hash
	for _, n := range names(t, c.path(layersDir)) {
		h, err := v1.NewHash("sha256:" + strings.TrimSuffix(n, ".erofs"))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	return out
}

// diffIDs returns the layers' diffIDs, sorted.
func diffIDs(t *testing.T, layers ...v1.Layer) []v1.Hash {
	t.Helper()
	var out []v1.Hash
	for _, l := range layers {
		out = append(out, diffID(t, l))
	}
	slices.SortFunc(out, func(a, b v1.Hash) int { return strings.Compare(a.Hex, b.Hex) })
	return out
}

// acquire acquires ref for job's VM 0, failing t on error.
func acquire(ctx context.Context, t *testing.T, c *Cache, job, ref string) Image {
	t.Helper()
	img, err := c.Acquire(ctx, Request{Job: job, Ref: ref})
	if err != nil {
		t.Fatalf("Acquire %s for %s: %v", ref, job, err)
	}
	return img
}

func release(ctx context.Context, t *testing.T, c *Cache, job string) {
	t.Helper()
	if err := c.Release(ctx, job); err != nil {
		t.Fatalf("Release %s: %v", job, err)
	}
}

// Filling the cache past its cap and releasing the jobs evicts the
// least-recently-used layers until it fits, each logged with its size
// (IMG-14, LOG-1).
func TestEvictionKeepsTheCacheUnderItsCap(t *testing.T) {
	t.Parallel()
	a, b, c := sizedLayer(t, "a"), sizedLayer(t, "b"), sizedLayer(t, "c")
	sizes := layerSizes(t, a, b, c)
	refA, refB, refC := push(t, image(t, a)), push(t, image(t, b)), push(t, image(t, c))

	// One layer too many: a was used again after b, so b is the least
	// recently used.
	t.Run("lru", func(t *testing.T) {
		t.Parallel()
		cache, log := cappedCache(t, sizes[0]+sizes[1]+sizes[2]-1)
		ctx := testCtx(t)
		for i, ref := range []string{refA, refB, refA} {
			job := fmt.Sprint("job-", i)
			acquire(ctx, t, cache, job, ref)
			release(ctx, t, cache, job)
		}
		if got, want := cachedLayers(t, cache), diffIDs(t, a, b); !slices.Equal(got, want) {
			t.Fatalf("under the cap, layers/ holds %v, want %v", got, want)
		}
		acquire(ctx, t, cache, "job-3", refC)
		release(ctx, t, cache, "job-3")
		if got, want := cachedLayers(t, cache), diffIDs(t, a, c); !slices.Equal(got, want) {
			t.Errorf("layers/ holds %v, want %v: b, least recently used, evicted", got, want)
		}
		want := fmt.Sprintf("msg=evicted layer=%s size=%d", diffID(t, b), sizes[1])
		if !strings.Contains(log.String(), want) {
			t.Errorf("the log has no %q:\n%s", want, log)
		}
		if n := strings.Count(log.String(), "msg=evicted"); n != 1 {
			t.Errorf("%d evictions logged, want 1:\n%s", n, log)
		}
	})

	// A cap of one image, its layer, config and tag, evicts until one
	// fits.
	t.Run("until it fits", func(t *testing.T) {
		t.Parallel()
		cache, _ := cappedCache(t, 1<<40)
		ctx := testCtx(t)
		acquire(ctx, t, cache, "job-a", refA)
		acquire(ctx, t, cache, "job-b", refB)
		acquire(ctx, t, cache, "job-c", refC)
		r, err := name.ParseReference(refC)
		if err != nil {
			t.Fatal(err)
		}
		setCap(cache, slices.Max(sizes)+allocated(t, cache.tagPath(r))+allocated(t, cache.imagePath(imageDigest(t, image(t, c)))))
		for _, job := range []string{"job-a", "job-b", "job-c"} {
			release(ctx, t, cache, job)
		}
		if got, want := cachedLayers(t, cache), diffIDs(t, c); !slices.Equal(got, want) {
			t.Errorf("layers/ holds %v, want only c, the most recently used: %v", got, want)
		}
	})
}

// A layer linked by a job still running survives eviction over the cap, and
// its VM can still read it; once that job is released it is evicted.
func TestLayerInUseSurvivesEviction(t *testing.T) {
	t.Parallel()
	a, b, c := sizedLayer(t, "a"), sizedLayer(t, "b"), sizedLayer(t, "c")
	refA, refB, refC := push(t, image(t, a)), push(t, image(t, b)), push(t, image(t, c))
	// A cap of the largest layer holds at most one.
	cache, _ := cappedCache(t, slices.Max(layerSizes(t, a, b, c)))
	ctx := testCtx(t)
	running := acquire(ctx, t, cache, "job-a", refA)
	acquire(ctx, t, cache, "job-b", refB)
	release(ctx, t, cache, "job-b")
	if got, want := cachedLayers(t, cache), diffIDs(t, a); !slices.Equal(got, want) {
		t.Fatalf("layers/ holds %v, want only a, which job-a uses: %v", got, want)
	}
	if _, err := os.ReadFile(filepath.Join(running.LinkDir, running.Layers[0])); err != nil {
		t.Errorf("job-a's layer: %v", err)
	}
	// a is now the oldest layer; c, newer, is in use.
	acquire(ctx, t, cache, "job-c", refC)
	release(ctx, t, cache, "job-a")
	if got, want := cachedLayers(t, cache), diffIDs(t, c); !slices.Equal(got, want) {
		t.Errorf("layers/ holds %v, want only c, which job-c uses: %v", got, want)
	}
}

// Under the cap, a Release deletes nothing: no layer, and no config or tag,
// not even a config with no cached layer.
func TestUnderTheCapNothingIsDeleted(t *testing.T) {
	t.Parallel()
	a, b := sizedLayer(t, "a"), sizedLayer(t, "b")
	cache, log := cappedCache(t, 1<<40)
	ctx := testCtx(t)
	// A config whose layer was never pulled.
	stray := image(t, sizedLayer(t, "stray"))
	raw, err := stray.RawConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache.imagePath(imageDigest(t, stray)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	acquire(ctx, t, cache, "job-a", push(t, image(t, a)))
	acquire(ctx, t, cache, "job-b", push(t, image(t, b)))
	// The cache is exactly at its cap.
	setCap(cache, cacheSize(t, cache))
	before := snapshot(t, cache)
	release(ctx, t, cache, "job-a")
	release(ctx, t, cache, "job-b")
	after := snapshot(t, cache)
	maps.DeleteFunc(before, func(p string, _ entry) bool { return strings.HasPrefix(p, jobsDir+"/") })
	if !maps.Equal(before, after) {
		t.Errorf("Release under the cap changed the cache:\nbefore %v\nafter  %v", before, after)
	}
	if strings.Contains(log.String(), "evicted") {
		t.Errorf("an eviction under the cap:\n%s", log)
	}
}

// After an eviction, an image config with no cached layer left is gone, and
// so is its tag, though eviction would keep the config, used more recently
// than its layers; an image with some of its layers left keeps both
// (IMG-14).
func TestPruningAfterEviction(t *testing.T) {
	t.Parallel()
	shared, p, q := sizedLayer(t, "shared"), sizedLayer(t, "p"), sizedLayer(t, "q")
	held, imgP, imgQ := image(t, shared), image(t, shared, p), image(t, q)
	refHeld, refP, refQ := push(t, held), push(t, imgP), push(t, imgQ)
	cache, _ := cappedCache(t, 1<<40)
	ctx := testCtx(t)
	// job-held keeps shared in use throughout.
	acquire(ctx, t, cache, "job-held", refHeld)
	acquire(ctx, t, cache, "job-p", refP)
	acquire(ctx, t, cache, "job-q", refQ)
	// Every config is used after every layer, so each eviction below
	// deletes the one layer not in use, and no config.
	future := time.Now().Add(time.Hour)
	for _, n := range names(t, cache.path(imagesDir)) {
		if err := os.Chtimes(cache.path(imagesDir, n), future, future); err != nil {
			t.Fatal(err)
		}
	}
	tagged := func(ref string) bool {
		t.Helper()
		r, err := name.ParseReference(ref)
		if err != nil {
			t.Fatal(err)
		}
		b, err := cache.readTagFile(r)
		if err != nil {
			t.Fatal(err)
		}
		return b != nil
	}
	configured := func(img v1.Image) bool {
		_, err := os.Stat(cache.imagePath(imageDigest(t, img)))
		return err == nil
	}

	setCap(cache, cacheSize(t, cache)-1)
	release(ctx, t, cache, "job-p")
	if got, want := cachedLayers(t, cache), diffIDs(t, shared, q); !slices.Equal(got, want) {
		t.Fatalf("layers/ holds %v, want %v", got, want)
	}
	if !configured(imgP) || !tagged(refP) {
		t.Errorf("P lost its config (%v) or tag (%v), with shared still cached", configured(imgP), tagged(refP))
	}

	setCap(cache, cacheSize(t, cache)-1)
	release(ctx, t, cache, "job-q")
	if got, want := cachedLayers(t, cache), diffIDs(t, shared); !slices.Equal(got, want) {
		t.Fatalf("layers/ holds %v, want %v", got, want)
	}
	if configured(imgQ) || tagged(refQ) {
		t.Errorf("Q kept its config (%v) or tag (%v), with no layer cached", configured(imgQ), tagged(refQ))
	}
	for name, img := range map[string]v1.Image{refHeld: held, refP: imgP} {
		if !configured(img) || !tagged(name) {
			t.Errorf("%s lost its config (%v) or tag (%v)", name, configured(img), tagged(name))
		}
	}
	if got := len(names(t, cache.path(imagesDir))); got != 2 {
		t.Errorf("images/ holds %d configs, want 2", got)
	}
	if got := len(names(t, cache.path(tagsDir))); got != 2 {
		t.Errorf("tags/ holds %d tags, want 2", got)
	}
}

// An eviction between an Acquire's look at a cached layer and its link
// skips that layer, so the Acquire never fails.
func TestEvictionSkipsALayerAnAcquireNeeds(t *testing.T) {
	t.Parallel()
	a, b := sizedLayer(t, "a"), sizedLayer(t, "b")
	refA, refB := push(t, image(t, a)), push(t, image(t, b))
	cache, _ := cappedCache(t, 1<<40)
	ctx := testCtx(t)
	acquire(ctx, t, cache, "job-1", refA)
	// The cache holds A and nothing more.
	setCap(cache, cacheSize(t, cache))
	release(ctx, t, cache, "job-1")
	// a is cached, unlinked and the oldest when job-3's Release evicts,
	// while job-2's Acquire of a has seen it cached but not yet linked it.
	// The hook runs again for job-3's own link: only the first call acts.
	var fired atomic.Bool
	cache.beforeLink = func() {
		if fired.CompareAndSwap(false, true) {
			acquire(ctx, t, cache, "job-3", refB)
			release(ctx, t, cache, "job-3")
		}
	}
	acquire(ctx, t, cache, "job-2", refA)
	if got, want := cachedLayers(t, cache), diffIDs(t, a); !slices.Equal(got, want) {
		t.Errorf("layers/ holds %v, want %v: b evicted, not a", got, want)
	}
}

// Evictions racing concurrent Acquires of the same layers never fail an
// Acquire. Run it with -race.
func TestEvictionRacingAcquires(t *testing.T) {
	t.Parallel()
	x, y, z := sizedLayer(t, "x"), sizedLayer(t, "y"), sizedLayer(t, "z")
	ref := push(t, image(t, x, y, z))
	// A cap of one layer evicts every layer but one, and the config, no
	// VM is using at every Release.
	cache, _ := cappedCache(t, slices.Max(layerSizes(t, x, y, z)))
	ctx := testCtx(t)
	var g sync.WaitGroup
	for w := range 4 {
		g.Go(func() {
			for i := range 15 {
				job := fmt.Sprintf("job-%d-%d", w, i)
				if _, err := cache.Acquire(ctx, Request{Job: job, Ref: ref}); err != nil {
					t.Errorf("Acquire for %s: %v", job, err)
					return
				}
				if err := cache.Release(ctx, job); err != nil {
					t.Errorf("Release %s: %v", job, err)
					return
				}
			}
		})
	}
	g.Wait()
	unpinned(ctx, t, cache)
	// An eviction under a cap of 1 byte deletes every layer, and the
	// image's config, and pruning its tag.
	setCap(cache, 1)
	cache.evict(false)
	emptyCache(t, cache)
}

// A config that an Acquire read from the cache and that is pruned before
// the Acquire records it is written again, so the image keeps its config.
func TestRecordRewritesAPrunedConfig(t *testing.T) {
	t.Parallel()
	img := image(t, sizedLayer(t, "a"))
	ref := push(t, img)
	cache := newCache(t)
	ctx := testCtx(t)
	acquire(ctx, t, cache, "job-1", ref)
	config := cache.imagePath(imageDigest(t, img))
	var fired atomic.Bool
	cache.beforeLink = func() {
		if fired.CompareAndSwap(false, true) {
			if err := os.Remove(config); err != nil {
				t.Error(err)
			}
		}
	}
	acquire(ctx, t, cache, "job-2", ref)
	if _, err := os.Stat(config); err != nil {
		t.Errorf("the image's config: %v", err)
	}
}

// An eviction just after an Acquire's lookup found the image cached skips
// the image's layers and keeps its config and tag, so the Acquire makes no
// registry request (IMG-7) and succeeds with the registry down.
func TestEvictionAfterACachedLookup(t *testing.T) {
	t.Parallel()
	a, b := sizedLayer(t, "a"), sizedLayer(t, "b")
	// A's registry fails every request once job-1 has acquired A, with a
	// status go-containerregistry doesn't retry.
	var down atomic.Bool
	var after requestLog
	fail := after.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusNotFound)
	}))
	hostA := serveRegistry(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if down.Load() {
				fail.ServeHTTP(w, r)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	refA, refB := pushTo(t, hostA, image(t, a)), push(t, image(t, b))
	cache, _ := cappedCache(t, 1<<40)
	ctx := testCtx(t)
	acquire(ctx, t, cache, "job-1", refA)
	// The cache holds A and nothing more.
	setCap(cache, cacheSize(t, cache))
	release(ctx, t, cache, "job-1")
	down.Store(true)
	// a is cached, unlinked and the oldest when job-3's Release evicts,
	// just after job-2's lookup of A found it cached. The hook runs again
	// for job-3's own lookup: only the first call acts.
	var fired atomic.Bool
	cache.afterLookup = func() {
		if fired.CompareAndSwap(false, true) {
			acquire(ctx, t, cache, "job-3", refB)
			release(ctx, t, cache, "job-3")
		}
	}
	acquire(ctx, t, cache, "job-2", refA)
	if reqs := after.take(); len(reqs) > 0 {
		t.Errorf("A's registry got %v after job-1, want no request", reqs)
	}
	if got, want := cachedLayers(t, cache), diffIDs(t, a); !slices.Equal(got, want) {
		t.Errorf("layers/ holds %v, want %v: b evicted, not a", got, want)
	}
	unpinned(ctx, t, cache)
}

// Image configs and tags count against the cap, and a config no job is
// using is evicted by its last use like a layer, its tag
// pruned after it: images that share one warm layer can't grow images/ and
// tags/ past the cap. A config an Acquire in flight has looked up survives,
// and so does the warm layer, used last (IMG-14).
func TestConfigsAreEvictedOldestFirst(t *testing.T) {
	t.Parallel()
	base := sizedLayer(t, "base")
	const n = 6
	var imgs []v1.Image
	var refs []string
	for i := range n + 1 {
		img := withEnv(t, image(t, base), fmt.Sprint("I=", i))
		imgs, refs = append(imgs, img), append(refs, push(t, img))
	}
	cache, log := cappedCache(t, 1<<40)
	ctx := testCtx(t)
	for i := range n {
		job := fmt.Sprint("job-", i)
		acquire(ctx, t, cache, job, refs[i])
		release(ctx, t, cache, job)
	}
	config := func(i int) string { return cache.imagePath(imageDigest(t, imgs[i])) }
	tag := func(i int) string {
		r, err := name.ParseReference(refs[i])
		if err != nil {
			t.Fatal(err)
		}
		return cache.tagPath(r)
	}
	// Every config, and every tag, takes the same blocks.
	cfgSize, tagSize := allocated(t, config(0)), allocated(t, tag(0))
	if cfgSize == 0 {
		t.Skip("this filesystem stores small files in no blocks of their own")
	}
	// The cap holds the layer, the n+1 tags, and three configs: the
	// oldest, pinned, and the two newest.
	limit := allocated(t, cache.layerPath(diffID(t, base))) + (n+1)*tagSize + 3*cfgSize
	// Image 0, the oldest, is looked up and pinned when image n's job is
	// acquired and released, and evicts.
	var fired atomic.Bool
	cache.afterLookup = func() {
		if fired.CompareAndSwap(false, true) {
			setCap(cache, limit)
			acquire(ctx, t, cache, "job-n", refs[n])
			release(ctx, t, cache, "job-n")
		}
	}
	acquire(ctx, t, cache, "job-pinned", refs[0])
	release(ctx, t, cache, "job-pinned")

	var want []string
	for _, i := range []int{0, n - 1, n} {
		want = append(want, filepath.Base(config(i)))
	}
	slices.Sort(want)
	if got := names(t, cache.path(imagesDir)); !slices.Equal(got, want) {
		t.Errorf("images/ holds %v, want the pinned config and the two newest: %v", got, want)
	}
	want = nil
	for _, i := range []int{0, n - 1, n} {
		want = append(want, filepath.Base(tag(i)))
	}
	slices.Sort(want)
	if got := names(t, cache.path(tagsDir)); !slices.Equal(got, want) {
		t.Errorf("tags/ holds %v, want the tags of the configs kept: %v", got, want)
	}
	if got, want := cachedLayers(t, cache), diffIDs(t, base); !slices.Equal(got, want) {
		t.Errorf("layers/ holds %v, want the warm layer: %v", got, want)
	}
	// Oldest first: configs 1 to n-2, in order.
	at := 0
	for i := 1; i <= n-2; i++ {
		line := fmt.Sprintf("msg=evicted config=%s size=", imageDigest(t, imgs[i]))
		j := strings.Index(log.String()[at:], line)
		if j < 0 {
			t.Fatalf("no %q after the earlier evictions:\n%s", line, log)
		}
		at += j + len(line)
	}
	if got := strings.Count(log.String(), "msg=evicted"); got != n-2 {
		t.Errorf("%d evictions logged, want %d:\n%s", got, n-2, log)
	}
	unpinned(ctx, t, cache)
}

// Pruning reads and parses the image configs without the mutex that links
// and records take, so a Start never waits on that parse: an Acquire runs to
// its end while pruning has read the configs and goes no further.
func TestPruningScansWithoutTheStoreMutex(t *testing.T) {
	t.Parallel()
	a, b := sizedLayer(t, "a"), sizedLayer(t, "b")
	refA, refB := push(t, image(t, a)), push(t, image(t, b))
	cache, _ := cappedCache(t, 1<<40)
	ctx := testCtx(t)
	acquire(ctx, t, cache, "job-1", refA)
	acquire(ctx, t, cache, "job-2", refB)
	release(ctx, t, cache, "job-2")
	// job-1's Release evicts A's layer, and prunes.
	setCap(cache, cacheSize(t, cache)-1)
	done := make(chan error, 1)
	var fired atomic.Bool
	cache.pruneScanned = func() {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		go func() {
			_, err := cache.Acquire(ctx, Request{Job: "job-3", Ref: refB})
			done <- err
		}()
		wait, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		select {
		case err := <-done:
			done <- err
		case <-wait.Done():
			t.Error("an Acquire waited on pruning's scan of the configs")
		}
	}
	release(ctx, t, cache, "job-1")
	if !fired.Load() {
		t.Fatal("job-1's Release didn't prune")
	}
	if err := recv(ctx, t, done, "the Acquire"); err != nil {
		t.Errorf("Acquire during pruning: %v", err)
	}
}

// The configs of images that jobs still run are in use until those jobs'
// Releases, like their layers, so an eviction over the cap keeps them and
// their tags: another job's Start of such an image makes no registry
// request (IMG-7) and gets the digest the running jobs have.
func TestRunningImagesKeepTheirConfigs(t *testing.T) {
	t.Parallel()
	a, b, c := sizedLayer(t, "a"), sizedLayer(t, "b"), sizedLayer(t, "c")
	// Every registry fails every request once the images are pulled,
	// with a status go-containerregistry doesn't retry.
	var down atomic.Bool
	var after requestLog
	fail := after.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusNotFound)
	}))
	host := func() string {
		return serveRegistry(t, func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if down.Load() {
					fail.ServeHTTP(w, r)
					return
				}
				h.ServeHTTP(w, r)
			})
		})
	}
	imgA, imgB := image(t, a), image(t, b)
	refA, refB, refC := pushTo(t, host(), imgA), pushTo(t, host(), imgB), pushTo(t, host(), image(t, c))
	// A cap of the largest layer: the layers job-1 and job-2 use pass it.
	cache, log := cappedCache(t, slices.Max(layerSizes(t, a, b, c)))
	ctx := testCtx(t)
	acquire(ctx, t, cache, "job-1", refA)
	acquire(ctx, t, cache, "job-2", refB)
	acquire(ctx, t, cache, "job-c", refC)
	down.Store(true)
	release(ctx, t, cache, "job-c")
	for _, img := range []v1.Image{imgA, imgB} {
		if line := fmt.Sprintf("config=%s", imageDigest(t, img)); strings.Contains(log.String(), line) {
			t.Errorf("a running image's config was evicted or pruned:\n%s", log)
		}
	}
	if n := strings.Count(log.String(), "pruned tag="); n != 1 || !strings.Contains(log.String(), "pruned tag="+refName(t, refC)+"\n") {
		t.Errorf("%d tags pruned, want only job-c's:\n%s", n, log)
	}
	img := acquire(ctx, t, cache, "job-3", refA)
	if reqs := after.take(); len(reqs) > 0 {
		t.Errorf("the registries got %v, want no request", reqs)
	}
	if got, want := img.Digest, imageDigest(t, imgA).String(); got != want {
		t.Errorf("job-3 got %s, want job-1's %s", got, want)
	}
	unpinned(ctx, t, cache)
	for _, job := range []string{"job-1", "job-2", "job-3"} {
		release(ctx, t, cache, job)
	}
	unpinned(ctx, t, cache)
	// Released, their configs are evicted like any other.
	setCap(cache, 1)
	cache.evict(false)
	emptyCache(t, cache)
}
