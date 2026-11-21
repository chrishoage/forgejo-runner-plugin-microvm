// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// requestLog records every request an in-process registry serves, as
// "METHOD path".
type requestLog struct {
	mu   sync.Mutex
	reqs []string
}

func (l *requestLog) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.reqs = append(l.reqs, r.Method+" "+r.URL.Path)
		l.mu.Unlock()
		h.ServeHTTP(w, r)
	})
}

// take returns the requests since the last take and forgets them.
func (l *requestLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.reqs
	l.reqs = nil
	return r
}

// countingRegistry serves an in-process registry that logs its requests,
// and returns its host.
func countingRegistry(t *testing.T) (string, *requestLog) {
	t.Helper()
	var l requestLog
	return serveRegistry(t, l.wrap), &l
}

func imageDigest(t *testing.T, img v1.Image) v1.Hash {
	t.Helper()
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func withEnv(t *testing.T, img v1.Image, env ...string) v1.Image {
	t.Helper()
	img, err := mutate.Config(img, v1.Config{Env: env})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// entry is one file or directory of the cache, as a snapshot sees it.
type entry struct {
	mode    fs.FileMode
	content string
	// mtime is kept for every file but a layer and its links, whose
	// mtime is the layer's last use.
	mtime int64
}

// snapshot returns every path under the cache but its lock file.
func snapshot(t *testing.T, c *Cache) map[string]entry {
	t.Helper()
	out := map[string]entry{}
	err := filepath.WalkDir(c.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(c.dir, p)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		e := entry{mode: fi.Mode()}
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			e.content = hex.EncodeToString(sum[:])
			if top := strings.SplitN(rel, "/", 2)[0]; top != layersDir && top != jobsDir {
				e.mtime = fi.ModTime().UnixNano()
			}
		}
		out[rel] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A second Acquire of a cached tag makes no registry request, and changes
// nothing in the cache but its layers' mtimes and the new link directory
// (IMG-6, IMG-7).
func TestCachedTagMakesNoRequest(t *testing.T) {
	t.Parallel()
	host, reqs := countingRegistry(t)
	img := withEnv(t, image(t, headerLayer(t, file("a")), headerLayer(t, file("b"))), "A=1")
	ref := pushTo(t, host, img)
	c := newCache(t)
	ctx := testCtx(t)
	reqs.take()
	if _, err := c.Acquire(ctx, Request{Job: "job-1", Ref: ref}); err != nil {
		t.Fatal(err)
	}
	if len(reqs.take()) == 0 {
		t.Fatal("the first Acquire made no registry request")
	}
	if got := names(t, c.path(imagesDir)); !slices.Equal(got, []string{imageDigest(t, img).Hex + ".json"}) {
		t.Errorf("images/ holds %v", got)
	}
	if got := names(t, c.path(tagsDir)); len(got) != 1 {
		t.Errorf("tags/ holds %v, want one tag", got)
	}
	for _, d := range []string{imagesDir, tagsDir} {
		for _, n := range names(t, c.path(d)) {
			if fi, err := os.Stat(c.path(d, n)); err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf("%s/%s: %v, %v; want mode 0600", d, n, fi.Mode(), err)
			}
		}
		if fi, err := os.Stat(c.path(d)); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s/: %v, %v; want mode 0700", d, fi.Mode(), err)
		}
	}
	before := snapshot(t, c)

	got, err := c.Acquire(ctx, Request{Job: "job-2", Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	if r := reqs.take(); len(r) != 0 {
		t.Errorf("the second Acquire made registry requests: %v", r)
	}
	if got.Digest != imageDigest(t, img).String() || !slices.Equal(got.Env, []string{"A=1"}) {
		t.Errorf("the cached image: digest %s, env %q", got.Digest, got.Env)
	}
	after := snapshot(t, c)
	for p, e := range after {
		if p == filepath.Join(jobsDir, "job-2") || strings.HasPrefix(p, filepath.Join(jobsDir, "job-2")+"/") {
			continue
		}
		if b, ok := before[p]; !ok {
			t.Errorf("%s is new", p)
		} else if b != e {
			t.Errorf("%s changed: %+v, was %+v", p, e, b)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			t.Errorf("%s is gone", p)
		}
	}
}

// blobPath is the path a registry serves the blob h of repo test/image at.
func blobPath(h v1.Hash) string { return "/v2/test/image/blobs/" + h.String() }

// A digest reference whose config and layers are cached makes no registry
// request. With a layer gone, a digest or a cached tag fetches only that
// layer, by digest, and never looks the tag up (IMG-6).
func TestMissingLayerIsFetchedByDigest(t *testing.T) {
	t.Parallel()
	for _, byDigest := range []bool{true, false} {
		host, reqs := countingRegistry(t)
		a, b := headerLayer(t, file("a")), headerLayer(t, file("b"))
		img := image(t, a, b)
		ref := pushTo(t, host, img)
		if byDigest {
			ref = host + "/test/image@" + imageDigest(t, img).String()
		}
		c := newCache(t)
		ctx := testCtx(t)
		if _, err := c.Acquire(ctx, Request{Job: "job-1", Ref: ref}); err != nil {
			t.Fatal(err)
		}
		reqs.take()
		if _, err := c.Acquire(ctx, Request{Job: "job-2", Ref: ref}); err != nil {
			t.Fatal(err)
		}
		if r := reqs.take(); len(r) != 0 {
			t.Errorf("by digest %v: the cached image made registry requests: %v", byDigest, r)
		}
		// An image's own digest needs no tag file.
		if got := names(t, c.path(tagsDir)); byDigest && len(got) != 0 {
			t.Errorf("tags/ holds %v for an image's digest", got)
		}

		if err := os.Remove(c.layerPath(diffID(t, b))); err != nil {
			t.Fatal(err)
		}
		got, err := c.Acquire(ctx, Request{Job: "job-3", Ref: ref})
		if err != nil {
			t.Fatal(err)
		}
		r := reqs.take()
		digest, err := b.Digest()
		if err != nil {
			t.Fatal(err)
		}
		var blobs []string
		for _, q := range r {
			if strings.Contains(q, "/manifests/") && !strings.Contains(q, "/manifests/sha256:") {
				t.Errorf("by digest %v: a manifest request by tag: %s", byDigest, q)
			}
			if strings.Contains(q, "/blobs/") {
				blobs = append(blobs, q)
			}
		}
		if !slices.Equal(blobs, []string{"GET " + blobPath(digest)}) {
			t.Errorf("by digest %v: blob requests %v, want only layer b's", byDigest, blobs)
		}
		if len(got.Layers) != 2 {
			t.Errorf("by digest %v: layers %v", byDigest, got.Layers)
		}
		if _, err := os.Stat(c.layerPath(diffID(t, b))); err != nil {
			t.Errorf("by digest %v: layer b is not cached again: %v", byDigest, err)
		}
	}
}

// A layer's blob is read only up to the size its manifest declares: a
// registry that sends the blob and then endless empty gzip members, with no
// Content-Length, fails the Start at once, as does one that ends the blob
// short. Neither leaves anything in the cache.
func TestLayerBlobIsBoundToItsManifestSize(t *testing.T) {
	t.Parallel()
	l := headerLayer(t, file("f"))
	digest, err := l.Digest()
	if err != nil {
		t.Fatal(err)
	}
	rc, err := l.Compressed()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	var member bytes.Buffer
	if err := gzip.NewWriter(&member).Close(); err != nil {
		t.Fatal(err)
	}
	members := bytes.Repeat(member.Bytes(), 64<<10/member.Len())
	for name, tt := range map[string]struct {
		endless bool
		want    string
	}{
		"endless": {endless: true, want: "runs past the " + strconv.Itoa(len(blob)) + " bytes its manifest declares"},
		"short":   {want: "short of the " + strconv.Itoa(len(blob)) + " its manifest declares"},
	} {
		var served atomic.Int64
		host := serveRegistry(t, func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != blobPath(digest) {
					h.ServeHTTP(w, r)
					return
				}
				// Flushing before the body is done makes it chunked,
				// with no Content-Length.
				f := w.(http.Flusher)
				if !tt.endless {
					_, _ = w.Write(blob[:len(blob)-1])
					f.Flush()
					return
				}
				if _, err := w.Write(blob); err != nil {
					return
				}
				f.Flush()
				for r.Context().Err() == nil {
					n, err := w.Write(members)
					served.Add(int64(n))
					if err != nil {
						return
					}
					f.Flush()
				}
			})
		})
		c := newCache(t)
		_, err := c.Acquire(testCtx(t), Request{Job: "job-1", Ref: pushTo(t, host, image(t, l))})
		if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), diffID(t, l).String()) {
			t.Fatalf("%s: err = %v, want one naming the layer with %q", name, err, tt.want)
		}
		// Past what the connection buffers, the registry is no longer
		// read.
		if n := served.Load(); n > 64<<20 {
			t.Errorf("%s: the registry served %d bytes past the blob", name, n)
		}
		emptyCache(t, c)
	}
}

// Short and fully qualified references share one tag file (IMG-1).
func TestReferencesAreNormalised(t *testing.T) {
	t.Parallel()
	c := newCache(t)
	tagFile := func(s string) string {
		t.Helper()
		ref, err := name.ParseReference(s)
		if err != nil {
			t.Fatal(err)
		}
		return c.tagPath(ref)
	}
	node := tagFile("node:22")
	for _, s := range []string{"docker.io/library/node:22", "index.docker.io/library/node:22", "docker.io/node:22"} {
		if got := tagFile(s); got != node {
			t.Errorf("%s has tag file %s, node:22 has %s", s, got, node)
		}
	}
	if tagFile("node:23") == node || tagFile("other/node:22") == node {
		t.Error("two images share a tag file")
	}

	// An implicit latest is latest.
	host, reqs := countingRegistry(t)
	img := image(t, headerLayer(t, file("f")))
	r, err := name.ParseReference(host + "/test/image:latest")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(r, img); err != nil {
		t.Fatal(err)
	}
	ctx := testCtx(t)
	if _, err := c.Acquire(ctx, Request{Job: "job-1", Ref: host + "/test/image"}); err != nil {
		t.Fatal(err)
	}
	reqs.take()
	if _, err := c.Acquire(ctx, Request{Job: "job-2", Ref: host + "/test/image:latest"}); err != nil {
		t.Fatal(err)
	}
	if q := reqs.take(); len(q) != 0 {
		t.Errorf("the fully qualified reference made registry requests: %v", q)
	}
	if got := names(t, c.path(tagsDir)); len(got) != 1 {
		t.Errorf("tags/ holds %v, want one tag", got)
	}
}

// A multi-platform index resolves to linux/<GOARCH> (IMG-2), by tag or by
// the index's digest, and the index's digest resolves once.
func TestIndexResolvesToHostPlatform(t *testing.T) {
	t.Parallel()
	other := "arm64"
	if runtime.GOARCH == other {
		other = "amd64"
	}
	want := withEnv(t, image(t, headerLayer(t, file("host"))), "PLATFORM=host")
	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{
			Add:        withEnv(t, image(t, headerLayer(t, file("other"))), "PLATFORM=other"),
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: other}},
		},
		mutate.IndexAddendum{
			Add:        withEnv(t, image(t, headerLayer(t, file("windows"))), "PLATFORM=windows"),
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "windows", Architecture: runtime.GOARCH}},
		},
		mutate.IndexAddendum{
			Add:        want,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: runtime.GOARCH}},
		},
	)
	host, reqs := countingRegistry(t)
	ref := host + "/test/image:1"
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(r, idx); err != nil {
		t.Fatal(err)
	}
	idxDigest, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}
	c := newCache(t)
	ctx := testCtx(t)
	byDigest := host + "/test/image@" + idxDigest.String()
	for _, job := range []struct{ id, ref string }{{"job-1", ref}, {"job-2", byDigest}, {"job-3", byDigest}} {
		reqs.take()
		got, err := c.Acquire(ctx, Request{Job: job.id, Ref: job.ref})
		if err != nil {
			t.Fatal(err)
		}
		if got.Digest != imageDigest(t, want).String() || !slices.Equal(got.Env, []string{"PLATFORM=host"}) {
			t.Errorf("%s: digest %s, env %q; want linux/%s's %s", job.ref, got.Digest, got.Env, runtime.GOARCH, imageDigest(t, want))
		}
	}
	// The index's digest resolved once, like a tag.
	if r := reqs.take(); len(r) != 0 {
		t.Errorf("the index's digest, resolved before, made registry requests: %v", r)
	}
}

// With ForcePull, Acquire looks the tag up: it picks up a new push, and a
// registry it can't reach fails it, cached tag or not (IMG-6).
func TestForcePull(t *testing.T) {
	t.Parallel()
	srv := newRegistry(t, nil)
	host := strings.TrimPrefix(srv.URL, "http://")
	v1img := withEnv(t, image(t, headerLayer(t, file("v1"))), "V=1")
	ref := pushTo(t, host, v1img)
	c := newCache(t)
	ctx := testCtx(t)
	if _, err := c.Acquire(ctx, Request{Job: "job-1", Ref: ref}); err != nil {
		t.Fatal(err)
	}
	v2img := withEnv(t, image(t, headerLayer(t, file("v2"))), "V=2")
	pushTo(t, host, v2img)
	for _, tt := range []struct {
		job   string
		force bool
		want  v1.Image
	}{{"job-2", false, v1img}, {"job-3", true, v2img}, {"job-4", false, v2img}} {
		job := tt.job
		got, err := c.Acquire(ctx, Request{Job: job, Ref: ref, ForcePull: tt.force})
		if err != nil {
			t.Fatal(err)
		}
		if got.Digest != imageDigest(t, tt.want).String() {
			t.Errorf("%s, ForcePull %v: digest %s, want %s", job, tt.force, got.Digest, imageDigest(t, tt.want))
		}
	}

	srv.Close()
	if _, err := c.Acquire(ctx, Request{Job: "job-5", Ref: ref, ForcePull: true}); err == nil {
		t.Error("ForcePull with the registry unreachable succeeded")
	}
	if _, err := os.Stat(c.path(jobsDir, "job-5")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the failed Acquire left a link directory: %v", err)
	}
	if _, err := c.Acquire(ctx, Request{Job: "job-6", Ref: ref}); err != nil {
		t.Errorf("without ForcePull, the cached tag needs no registry: %v", err)
	}
}

// A registry that requires credentials works through the plugin user's
// registry auth file (IMG-3).
//
// Not parallel: it sets the environment.
func TestRegistryCredentials(t *testing.T) {
	const user, pass = "runner", "s3cret"
	var locked sync.Mutex
	authed := false
	host := serveRegistry(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			locked.Lock()
			need := authed
			locked.Unlock()
			if u, p, ok := r.BasicAuth(); need && (!ok || u != user || p != pass) {
				w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	ref := pushTo(t, host, image(t, headerLayer(t, file("f"))))
	locked.Lock()
	authed = true
	locked.Unlock()

	home, cfg := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_CONFIG", cfg)
	t.Setenv("REGISTRY_AUTH_FILE", "")
	writeConfig := func(s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cfg, "config.json"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := newCache(t)
	ctx := testCtx(t)
	writeConfig(`{}`)
	if _, err := c.Acquire(ctx, Request{Job: "job-1", Ref: ref}); err == nil {
		t.Fatal("Acquire without credentials succeeded")
	}
	writeConfig(`{"auths": {"` + host + `": {"auth": "` + base64.StdEncoding.EncodeToString([]byte(user+":"+pass)) + `"}}}`)
	if _, err := c.Acquire(ctx, Request{Job: "job-2", Ref: ref}); err != nil {
		t.Fatalf("Acquire with credentials in DOCKER_CONFIG: %v", err)
	}
}

// An Acquire cancelled mid-pull, its config fetched and its layer not,
// leaves no image config and no tag.
func TestCancelledPullLeavesNoImageOrTag(t *testing.T) {
	t.Parallel()
	l := headerLayer(t, file("a"))
	digest, err := l.Digest()
	if err != nil {
		t.Fatal(err)
	}
	layerPath := blobPath(digest)
	entered := make(chan struct{})
	var once sync.Once
	var blocking sync.Mutex
	block := false
	host := serveRegistry(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			blocking.Lock()
			b := block
			blocking.Unlock()
			if b && r.URL.Path == layerPath {
				once.Do(func() { close(entered) })
				<-r.Context().Done()
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	ref := pushTo(t, host, image(t, l))
	blocking.Lock()
	block = true
	blocking.Unlock()
	c := newCache(t)
	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Acquire(ctx, Request{Job: "job-1", Ref: ref})
		done <- err
	}()
	recv(testCtx(t), t, entered, "the blob request")
	cancel()
	if err := recv(testCtx(t), t, done, "Acquire"); err == nil {
		t.Fatal("the cancelled Acquire succeeded")
	}
	emptyCache(t, c)
}

// tagDigest returns the digest ref's tag file names.
func tagDigest(t *testing.T, c *Cache, ref string) string {
	t.Helper()
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.readTagFile(r)
	if err != nil || b == nil {
		t.Fatalf("tag file of %s: %q, %v", ref, b, err)
	}
	h, err := c.parseTag(r, b)
	if err != nil {
		t.Fatal(err)
	}
	return h.String()
}

// A slow Acquire that resolved a tag before a force_pull moved it doesn't
// put the older digest back in the tag file.
func TestSlowAcquireKeepsNewerTag(t *testing.T) {
	t.Parallel()
	l1 := headerLayer(t, file("v1"))
	d1, err := l1.Digest()
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	host := serveRegistry(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == blobPath(d1) {
				once.Do(func() { close(entered) })
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			h.ServeHTTP(w, r)
		})
	})
	img1 := image(t, l1)
	ref := pushTo(t, host, img1)
	want1 := imageDigest(t, img1).String()
	c := newCache(t)
	ctx := testCtx(t)
	done := make(chan error, 1)
	go func() {
		got, err := c.Acquire(ctx, Request{Job: "job-1", Ref: ref})
		if err == nil && got.Digest != want1 {
			err = errors.New("the slow Acquire got " + got.Digest)
		}
		done <- err
	}()
	recv(ctx, t, entered, "the slow Acquire's layer request")

	img2 := image(t, headerLayer(t, file("v2")))
	pushTo(t, host, img2)
	if _, err := c.Acquire(ctx, Request{Job: "job-2", Ref: ref, ForcePull: true}); err != nil {
		t.Fatal(err)
	}
	if got := tagDigest(t, c, ref); got != imageDigest(t, img2).String() {
		t.Fatalf("after force_pull the tag names %s", got)
	}
	close(release)
	if err := recv(ctx, t, done, "the slow Acquire"); err != nil {
		t.Fatal(err)
	}
	if got := tagDigest(t, c, ref); got != imageDigest(t, img2).String() {
		t.Errorf("the slow Acquire put back %s; want force_pull's %s", got, imageDigest(t, img2))
	}
}

// A tag file that names another reference, or isn't JSON, fails Acquire
// naming the file.
func TestBadTagFileFailsAcquire(t *testing.T) {
	t.Parallel()
	host := serveRegistry(t, nil)
	ref := pushTo(t, host, image(t, headerLayer(t, file("f"))))
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	for i, tt := range []struct{ name, content, want string }{
		{"another reference", `{"reference":"` + host + `/test/other:1","digest":"sha256:` + strings.Repeat("0", 64) + `"}`, "names"},
		{"not JSON", `{`, "unexpected end"},
	} {
		c := newCache(t)
		ctx := testCtx(t)
		if _, err := c.Acquire(ctx, Request{Job: "job-1", Ref: ref}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(c.tagPath(r), []byte(tt.content), 0o600); err != nil {
			t.Fatal(err)
		}
		job := "job-" + strconv.Itoa(i+2)
		_, err := c.Acquire(ctx, Request{Job: job, Ref: ref})
		if err == nil || !strings.Contains(err.Error(), c.tagPath(r)) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: Acquire: %v; want an error naming %s and %q", tt.name, err, c.tagPath(r), tt.want)
		}
		if _, err := os.Stat(c.path(jobsDir, job)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: the failed Acquire left a link directory: %v", tt.name, err)
		}
	}
}

// A missing layer's manifest, fetched by digest, must name the cached config:
// a cached config that isn't the image's fails Acquire.
func TestManifestMustNameCachedConfig(t *testing.T) {
	t.Parallel()
	host := serveRegistry(t, nil)
	a, b := headerLayer(t, file("a")), headerLayer(t, file("b"))
	img := withEnv(t, image(t, a, b), "A=1")
	ref := pushTo(t, host, img)
	c := newCache(t)
	ctx := testCtx(t)
	if _, err := c.Acquire(ctx, Request{Job: "job-1", Ref: ref}); err != nil {
		t.Fatal(err)
	}
	// Another config with the same layers.
	other, err := withEnv(t, img, "A=2").RawConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.imagePath(imageDigest(t, img)), other, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(c.layerPath(diffID(t, b))); err != nil {
		t.Fatal(err)
	}
	_, err = c.Acquire(ctx, Request{Job: "job-2", Ref: ref})
	if err == nil || !strings.Contains(err.Error(), "not the one cached") {
		t.Errorf("Acquire: %v; want the cached config refused", err)
	}
	if _, err := os.Stat(c.layerPath(diffID(t, b))); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("layer b was cached again: %v", err)
	}
}

// rawManifest is a manifest pushed as it is.
type rawManifest struct {
	b  []byte
	mt types.MediaType
}

func (m rawManifest) RawManifest() ([]byte, error)        { return m.b, nil }
func (m rawManifest) MediaType() (types.MediaType, error) { return m.mt, nil }

// A manifest with fewer layers than its config's diffIDs fails Acquire.
func TestManifestMustHaveConfigsLayers(t *testing.T) {
	t.Parallel()
	host := serveRegistry(t, nil)
	img := image(t, headerLayer(t, file("a")), headerLayer(t, file("b")))
	pushTo(t, host, img)
	mf, err := img.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	mf.Layers = mf.Layers[:1]
	b, err := json.Marshal(mf)
	if err != nil {
		t.Fatal(err)
	}
	ref := host + "/test/image:short"
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(r, rawManifest{b: b, mt: mf.MediaType}); err != nil {
		t.Fatal(err)
	}
	c := newCache(t)
	_, err = c.Acquire(testCtx(t), Request{Job: "job-1", Ref: ref})
	if err == nil || !strings.Contains(err.Error(), "has 1 layers, its config 2") {
		t.Errorf("Acquire: %v; want the manifest refused", err)
	}
	emptyCache(t, c)
}

// A config whose write fails fails Acquire, and its link directory is
// deleted.
func TestFailedRecordDeletesLinkDir(t *testing.T) {
	t.Parallel()
	host := serveRegistry(t, nil)
	ref := pushTo(t, host, image(t, headerLayer(t, file("a"))))
	c := newCache(t)
	// Once the layer is converting, after resolve and before record,
	// images/ stops being a directory.
	var once sync.Once
	c.convert = func(out io.Writer, r io.Reader, tmp string) error {
		once.Do(func() {
			if err := os.Rename(c.path(imagesDir), c.path("images.gone")); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(c.path(imagesDir), nil, 0o600); err != nil {
				t.Error(err)
			}
		})
		return convert(out, r, tmp)
	}
	if _, err := c.Acquire(testCtx(t), Request{Job: "job-1", Ref: ref}); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("Acquire: %v; want ENOTDIR from writing the config", err)
	}
	if got := names(t, c.path(jobsDir, "job-1")); len(got) != 0 {
		t.Errorf("the failed Acquire left link directories %v", got)
	}
	if got := names(t, c.path(tagsDir)); len(got) != 0 {
		t.Errorf("tags/ holds %v", got)
	}
}

// A config over maxConfigSize is refused by its manifest's size, before it
// is fetched.
func TestConfigOverCapIsRefused(t *testing.T) {
	t.Parallel()
	host, reqs := countingRegistry(t)
	img := withEnv(t, image(t, headerLayer(t, file("a"))), "BIG="+strings.Repeat("x", maxConfigSize))
	ref := pushTo(t, host, img)
	cfg, err := img.ConfigName()
	if err != nil {
		t.Fatal(err)
	}
	c := newCache(t)
	reqs.take()
	_, err = c.Acquire(testCtx(t), Request{Job: "job-1", Ref: ref})
	if err == nil || !strings.Contains(err.Error(), "at most 8388608") {
		t.Errorf("Acquire: %v; want the config refused by its size", err)
	}
	for _, q := range reqs.take() {
		if strings.Contains(q, cfg.String()) {
			t.Errorf("the config was fetched: %s", q)
		}
	}
	emptyCache(t, c)
}
