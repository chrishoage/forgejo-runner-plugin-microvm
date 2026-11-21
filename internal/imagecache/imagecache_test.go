// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

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

// Acquire returns the image's digest and environment.
func TestAcquireReturnsTheImage(t *testing.T) {
	t.Parallel()
	img, err := mutate.Config(image(t, headerLayer(t, file("f"))), v1.Config{Env: []string{"PATH=/usr/bin:/bin", "A=b"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := newCache(t).Acquire(testCtx(t), Request{Job: "job-1", Ref: push(t, img)})
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
}

func TestAcquireFails(t *testing.T) {
	t.Parallel()
	c := newCache(t)
	ctx := testCtx(t)
	host := serveRegistry(t, nil)
	for name, ref := range map[string]string{
		"malformed reference": "UPPER/case::x",
		"missing image":       host + "/no/such:1",
	} {
		if _, err := c.Acquire(ctx, Request{Job: "job-1", Ref: ref}); err == nil {
			t.Errorf("%s: Acquire succeeded", name)
		}
	}
	// A cancelled pull returns.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Acquire(cctx, Request{Job: "job-2", Ref: pushTo(t, host, image(t, headerLayer(t, file("f"))))}); err == nil {
		t.Fatal("a cancelled Acquire succeeded")
	}
	emptyCache(t, c)
}
