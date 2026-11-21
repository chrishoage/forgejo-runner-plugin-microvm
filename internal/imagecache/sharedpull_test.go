// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// holdingRegistry is an in-process registry that counts the GETs of each
// blob and holds the GETs of the blobs in held until unhold.
type holdingRegistry struct {
	host string
	// entered gets the path of each held request as it arrives, and
	// ended the path of each held request that ended before release.
	entered, ended chan string
	release        chan struct{}
	once           sync.Once

	mu   sync.Mutex
	held map[string]bool
	gets map[string]int
}

func newHoldingRegistry(t *testing.T) *holdingRegistry {
	t.Helper()
	r := &holdingRegistry{
		entered: make(chan string, 64),
		ended:   make(chan string, 64),
		release: make(chan struct{}),
		held:    map[string]bool{},
		gets:    map[string]int{},
	}
	r.host = serveRegistry(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Method == http.MethodGet {
				r.mu.Lock()
				r.gets[req.URL.Path]++
				hold := r.held[req.URL.Path]
				r.mu.Unlock()
				if hold {
					r.entered <- req.URL.Path
					select {
					case <-r.release:
					case <-req.Context().Done():
						r.ended <- req.URL.Path
						return
					}
				}
			}
			h.ServeHTTP(w, req)
		})
	})
	// Before the server closes, which waits for held requests: a test
	// that fails while holding them ends instead of hanging.
	t.Cleanup(r.unhold)
	return r
}

// unhold lets every held request, and every later one, through.
func (r *holdingRegistry) unhold() { r.once.Do(func() { close(r.release) }) }

// hold holds the GETs of layers' blobs from now on, and returns their
// paths.
func (r *holdingRegistry) hold(t *testing.T, layers ...v1.Layer) []string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var paths []string
	for _, l := range layers {
		d, err := l.Digest()
		if err != nil {
			t.Fatal(err)
		}
		p := blobPath(d)
		r.held[p] = true
		paths = append(paths, p)
	}
	return paths
}

// holdPaths holds the GETs of paths from now on.
func (r *holdingRegistry) holdPaths(paths ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range paths {
		r.held[p] = true
	}
}

// getsOf returns how often each of paths was fetched.
func (r *holdingRegistry) getsOf(paths []string) []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n []int
	for _, p := range paths {
		n = append(n, r.gets[p])
	}
	return n
}

// watchJoins returns a channel that gets each layer pull's number of waiters
// as one joins it.
func watchJoins(c *Cache) <-chan int {
	ch := make(chan int, 256)
	c.layers.mu.Lock()
	c.layers.joined = func(_ string, refs int) { ch <- refs }
	c.layers.mu.Unlock()
	return ch
}

// waitJoins waits until n waiters have joined each of layers pulls.
func waitJoins(ctx context.Context, t *testing.T, joins <-chan int, layers, n int) {
	t.Helper()
	for seen := 0; seen < layers; {
		if recv(ctx, t, joins, "a waiter to join a pull") == n {
			seen++
		}
	}
}

// settle waits until every pull c started has ended, abandoned ones
// included.
func settle(ctx context.Context, t *testing.T, c *Cache) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		c.running.Wait()
		close(done)
	}()
	recv(ctx, t, done, "the pulls to end")
}

// unpinned waits until every pull and lookup c started has ended, and fails
// t if any layer or config is still pinned: every Acquire has returned, so
// every pin must be released. A config stays in use only by a job with a
// layer link directory, one not yet released, and once for each of its VMs.
func unpinned(ctx context.Context, t *testing.T, c *Cache) {
	t.Helper()
	settle(ctx, t, c)
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	if len(c.pinned) > 0 {
		t.Errorf("layers still pinned with no Acquire in flight: %v", c.pinned)
	}
	if len(c.pinnedConfigs) > 0 {
		t.Errorf("configs still pinned with no Acquire in flight: %v", c.pinnedConfigs)
	}
	for job, held := range c.jobConfigs {
		vms, err := os.ReadDir(c.path(jobsDir, job))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		if len(vms) != len(held) {
			t.Errorf("job %s holds configs %v, but has %d layer link directories", job, held, len(vms))
		}
	}
}

func allOnes(n []int) bool {
	for _, v := range n {
		if v != 1 {
			return false
		}
	}
	return true
}

// Eight Acquires at once of one uncached image fetch and convert each layer
// once, and all succeed (IMG-12).
func TestConcurrentAcquiresPullEachLayerOnce(t *testing.T) {
	t.Parallel()
	reg := newHoldingRegistry(t)
	layers := []v1.Layer{headerLayer(t, file("a")), headerLayer(t, file("b")), headerLayer(t, file("c"))}
	ref := pushTo(t, reg.host, image(t, layers...))
	paths := reg.hold(t, layers...)
	c := newCache(t)
	n := countConversions(c)
	joins := watchJoins(c)
	ctx := testCtx(t)
	const jobs = 8
	errs := make(chan error, jobs)
	for i := range jobs {
		go func() {
			_, err := c.Acquire(ctx, Request{Job: fmt.Sprintf("job-%d", i), Ref: ref})
			errs <- err
		}()
	}
	// Every Acquire waits on every layer's pull before any layer arrives.
	waitJoins(ctx, t, joins, len(layers), jobs)
	reg.unhold()
	for range jobs {
		if err := recv(ctx, t, errs, "an Acquire"); err != nil {
			t.Error(err)
		}
	}
	if got := reg.getsOf(paths); !allOnes(got) {
		t.Errorf("layer blobs fetched %v times, want once each", got)
	}
	if got := n.Load(); got != int32(len(layers)) {
		t.Errorf("%d conversions, want %d", got, len(layers))
	}
	settle(ctx, t, c)
	if got := names(t, c.path(tmpDir)); len(got) != 0 {
		t.Errorf("tmp/ holds %v", got)
	}
}

// With two jobs waiting on a held pull, cancelling one returns its Acquire
// at once with context.Canceled, and leaves the pull running; the other's
// Acquire succeeds once the pull is released.
func TestCancelOneOfTwoWaiters(t *testing.T) {
	t.Parallel()
	reg := newHoldingRegistry(t)
	l := headerLayer(t, file("a"))
	ref := pushTo(t, reg.host, image(t, l))
	paths := reg.hold(t, l)
	c := newCache(t)
	joins := watchJoins(c)
	ctx := testCtx(t)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cancelled, other := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := c.Acquire(cctx, Request{Job: "job-1", Ref: ref})
		cancelled <- err
	}()
	go func() {
		_, err := c.Acquire(ctx, Request{Job: "job-2", Ref: ref})
		other <- err
	}()
	waitJoins(ctx, t, joins, 1, 2)
	recv(ctx, t, reg.entered, "the layer request")
	cancel()
	if err := recv(ctx, t, cancelled, "the cancelled Acquire"); !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled Acquire: %v, want context.Canceled", err)
	}
	select {
	case err := <-other:
		t.Fatalf("the other Acquire returned while its pull was held: %v", err)
	case p := <-reg.ended:
		t.Fatalf("the shared request for %s ended with one job cancelled", p)
	default:
	}
	reg.unhold()
	if err := recv(ctx, t, other, "the other Acquire"); err != nil {
		t.Fatal(err)
	}
	if got := reg.getsOf(paths); !allOnes(got) {
		t.Errorf("the layer blob was fetched %v times, want once", got)
	}
}

// Cancelling a pull's only waiter cancels the pull: the registry sees the
// request end, and the cache holds nothing for it.
func TestCancelOnlyWaiterCancelsPull(t *testing.T) {
	t.Parallel()
	reg := newHoldingRegistry(t)
	l := headerLayer(t, file("a"))
	ref := pushTo(t, reg.host, image(t, l))
	paths := reg.hold(t, l)
	c := newCache(t)
	ctx := testCtx(t)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Acquire(cctx, Request{Job: "job-1", Ref: ref})
		done <- err
	}()
	recv(ctx, t, reg.entered, "the layer request")
	cancel()
	if err := recv(ctx, t, done, "the cancelled Acquire"); !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled Acquire: %v, want context.Canceled", err)
	}
	if p := recv(ctx, t, reg.ended, "the registry to see the request end"); p != paths[0] {
		t.Errorf("the request for %s ended, want %s", p, paths[0])
	}
	settle(ctx, t, c)
	for _, d := range []string{tmpDir, layersDir, jobsDir} {
		if got := names(t, c.path(d)); len(got) != 0 {
			t.Errorf("%s/ holds %v", d, got)
		}
	}
}

// Two references that resolve to one manifest, a tag and its digest, share
// the image's layers, one pull each.
func TestReferencesShareLayers(t *testing.T) {
	t.Parallel()
	reg := newHoldingRegistry(t)
	layers := []v1.Layer{headerLayer(t, file("a")), headerLayer(t, file("b"))}
	img := image(t, layers...)
	tag := pushTo(t, reg.host, img)
	byDigest := reg.host + "/test/image@" + imageDigest(t, img).String()
	paths := reg.hold(t, layers...)
	c := newCache(t)
	n := countConversions(c)
	joins := watchJoins(c)
	ctx := testCtx(t)
	errs := make(chan error, 2)
	for i, ref := range []string{tag, byDigest} {
		go func() {
			_, err := c.Acquire(ctx, Request{Job: fmt.Sprintf("job-%d", i), Ref: ref})
			errs <- err
		}()
	}
	waitJoins(ctx, t, joins, len(layers), 2)
	reg.unhold()
	for range 2 {
		if err := recv(ctx, t, errs, "an Acquire"); err != nil {
			t.Error(err)
		}
	}
	if got := reg.getsOf(paths); !allOnes(got) {
		t.Errorf("layer blobs fetched %v times, want once each", got)
	}
	if got := n.Load(); got != int32(len(layers)) {
		t.Errorf("%d conversions, want %d", got, len(layers))
	}
}

// An Acquire whose own layer fails leaves its wait on a layer it shares
// with another job, whose pull of it goes on and succeeds.
func TestFailedAcquireLeavesSharedPull(t *testing.T) {
	t.Parallel()
	reg := newHoldingRegistry(t)
	shared, own := headerLayer(t, file("shared")), headerLayer(t, file("own"))
	bad := otherDiffID{headerLayer(t, file("evil")), diffID(t, own)}
	good := pushTo(t, reg.host, image(t, shared))
	badRef := reg.host + "/test/image:bad"
	r, err := name.ParseReference(badRef)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(r, image(t, shared, bad)); err != nil {
		t.Fatal(err)
	}
	reg.hold(t, shared)
	c := newCache(t)
	joins := watchJoins(c)
	ctx := testCtx(t)
	failed, ok := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := c.Acquire(ctx, Request{Job: "job-2", Ref: good})
		ok <- err
	}()
	recv(ctx, t, reg.entered, "job-2's layer request")
	go func() {
		_, err := c.Acquire(ctx, Request{Job: "job-1", Ref: badRef})
		failed <- err
	}()
	// job-1's bad layer isn't held, so it fails while the shared one is.
	if err := recv(ctx, t, failed, "the failing Acquire"); err == nil || !strings.Contains(err.Error(), "its content has the digest") {
		t.Fatalf("the Acquire with a bad layer: %v, want its diffID mismatch", err)
	}
	if !slices.Contains(drain(joins), 2) {
		t.Error("job-1 never joined job-2's pull of the shared layer")
	}
	select {
	case p := <-reg.ended:
		t.Fatalf("the shared request for %s ended when job-1 failed", p)
	default:
	}
	reg.unhold()
	if err := recv(ctx, t, ok, "the other Acquire"); err != nil {
		t.Fatal(err)
	}
	settle(ctx, t, c)
	if got, want := names(t, c.path(layersDir)), []string{diffID(t, shared).Hex + ".erofs"}; !slices.Equal(got, want) {
		t.Errorf("layers/ holds %v, want only the shared layer %v", got, want)
	}
	if got := names(t, c.path(tmpDir)); len(got) != 0 {
		t.Errorf("tmp/ holds %v", got)
	}
}

// drain returns what ch holds now.
func drain(ch <-chan int) []int {
	var out []int
	for {
		select {
		case v := <-ch:
			out = append(out, v)
		default:
			return out
		}
	}
}

// A pull a Start joined that fails from another image's source is retried
// once from the Start's own: here job-1's image names the shared layer's
// diffID for a blob with other content, and job-2, which joined job-1's
// pull of it, still pulls the real layer from its own repository. job-1's
// failure from its own source is final, and isn't retried.
func TestForeignSourceFailureRetriesOwn(t *testing.T) {
	t.Parallel()
	reg := newHoldingRegistry(t)
	shared := headerLayer(t, file("shared"))
	evil := headerLayer(t, file("evil"))
	good := pushTo(t, reg.host, image(t, shared))
	badRef := reg.host + "/test/evil:1"
	r, err := name.ParseReference(badRef)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(r, image(t, otherDiffID{evil, diffID(t, shared)})); err != nil {
		t.Fatal(err)
	}
	evilBlob, err := evil.Digest()
	if err != nil {
		t.Fatal(err)
	}
	evilPath := "/v2/test/evil/blobs/" + evilBlob.String()
	goodPaths := reg.hold(t, shared)
	reg.holdPaths(evilPath)
	c := newCache(t)
	joins := watchJoins(c)
	ctx := testCtx(t)
	failed, ok := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := c.Acquire(ctx, Request{Job: "job-1", Ref: badRef})
		failed <- err
	}()
	if p := recv(ctx, t, reg.entered, "job-1's layer request"); p != evilPath {
		t.Fatalf("request for %s, want %s", p, evilPath)
	}
	go func() {
		_, err := c.Acquire(ctx, Request{Job: "job-2", Ref: good})
		ok <- err
	}()
	waitJoins(ctx, t, joins, 1, 2)
	reg.unhold()
	if err := recv(ctx, t, failed, "job-1's Acquire"); err == nil || !strings.Contains(err.Error(), "its content has the digest") {
		t.Fatalf("job-1's Acquire: %v, want its diffID mismatch", err)
	}
	if err := recv(ctx, t, ok, "job-2's Acquire"); err != nil {
		t.Fatalf("job-2's Acquire failed with job-1's source's error: %v", err)
	}
	if got := reg.getsOf(append([]string{evilPath}, goodPaths...)); !allOnes(got) {
		t.Errorf("the bad and the real blob fetched %v times, want once each", got)
	}
	settle(ctx, t, c)
	if got, want := names(t, c.path(layersDir)), []string{diffID(t, shared).Hex + ".erofs"}; !slices.Equal(got, want) {
		t.Errorf("layers/ holds %v, want %v", got, want)
	}
	if got := names(t, c.path(tmpDir)); len(got) != 0 {
		t.Errorf("tmp/ holds %v", got)
	}
}

// watchLookups returns a channel that gets each lookup's key and number of
// waiters as one joins it.
func watchLookups(c *Cache) <-chan string {
	ch := make(chan string, 256)
	c.lookups.mu.Lock()
	c.lookups.joined = func(key string, refs int) { ch <- fmt.Sprintf("%s %d", key, refs) }
	c.lookups.mu.Unlock()
	return ch
}

// Concurrent Acquires of one reference share one lookup: one manifest
// request. A force_pull Acquire never shares a lookup with one that isn't:
// one in flight doesn't hold up a concurrent Acquire that has the tag
// cached, and the force_pull one gets the registry's new digest while the
// other gets the cached one (IMG-6, IMG-12).
func TestLookupSharing(t *testing.T) {
	t.Parallel()
	const manifests = "/v2/test/image/manifests/1"
	t.Run("shared", func(t *testing.T) {
		t.Parallel()
		reg := newHoldingRegistry(t)
		ref := pushTo(t, reg.host, image(t, headerLayer(t, file("a"))))
		reg.holdPaths(manifests)
		c := newCache(t)
		lookups := watchLookups(c)
		ctx := testCtx(t)
		const jobs = 4
		errs := make(chan error, jobs)
		for i := range jobs {
			go func() {
				_, err := c.Acquire(ctx, Request{Job: fmt.Sprintf("job-%d", i), Ref: ref})
				errs <- err
			}()
		}
		recv(ctx, t, reg.entered, "the manifest request")
		want := fmt.Sprintf("%s %d", refName(t, ref), jobs)
		for got := ""; got != want; {
			got = recv(ctx, t, lookups, "every Acquire to join the lookup")
		}
		reg.unhold()
		for range jobs {
			if err := recv(ctx, t, errs, "an Acquire"); err != nil {
				t.Error(err)
			}
		}
		if got := reg.getsOf([]string{manifests}); got[0] != 1 {
			t.Errorf("the manifest fetched %d times, want once", got[0])
		}
		unpinned(ctx, t, c)
	})
	t.Run("force_pull apart", func(t *testing.T) {
		t.Parallel()
		reg := newHoldingRegistry(t)
		oldImg := image(t, headerLayer(t, file("old")))
		ref := pushTo(t, reg.host, oldImg)
		c := newCache(t)
		ctx := testCtx(t)
		if _, err := c.Acquire(ctx, Request{Job: "job-0", Ref: ref}); err != nil {
			t.Fatal(err)
		}
		newImg := image(t, headerLayer(t, file("new")))
		pushTo(t, reg.host, newImg)
		before := reg.getsOf([]string{manifests})[0]
		reg.holdPaths(manifests)
		lookups := watchLookups(c)
		forced, cached := make(chan Image, 1), make(chan Image, 1)
		errs := make(chan error, 2)
		go func() {
			img, err := c.Acquire(ctx, Request{Job: "job-1", Ref: ref, ForcePull: true})
			errs <- err
			forced <- img
		}()
		recv(ctx, t, reg.entered, "the force_pull Acquire's manifest request")
		go func() {
			img, err := c.Acquire(ctx, Request{Job: "job-2", Ref: ref})
			errs <- err
			cached <- img
		}()
		recv(ctx, t, lookups, "the force_pull Acquire's lookup")
		if got, want := recv(ctx, t, lookups, "the other Acquire's lookup"), refName(t, ref)+" 1"; got != want {
			t.Fatalf("the other Acquire joined lookup %q, want its own, %q", got, want)
		}
		if err := recv(ctx, t, errs, "the Acquire with a cached tag"); err != nil {
			t.Fatal(err)
		}
		if got, want := recv(ctx, t, cached, "its image").Digest, imageDigest(t, oldImg).String(); got != want {
			t.Errorf("the Acquire with a cached tag got %s, want the cached %s", got, want)
		}
		reg.unhold()
		if err := recv(ctx, t, errs, "the force_pull Acquire"); err != nil {
			t.Fatal(err)
		}
		if got, want := recv(ctx, t, forced, "its image").Digest, imageDigest(t, newImg).String(); got != want {
			t.Errorf("the force_pull Acquire got %s, want the registry's %s", got, want)
		}
		if got := reg.getsOf([]string{manifests})[0] - before; got != 1 {
			t.Errorf("the manifest fetched %d times, want once, by the force_pull Acquire", got)
		}
		unpinned(ctx, t, c)
	})
}

// refName is ref normalised: its lookup's key.
func refName(t *testing.T, ref string) string {
	t.Helper()
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	return r.Name()
}
