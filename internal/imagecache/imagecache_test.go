// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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
	return strings.TrimPrefix(newRegistry(t, wrap).URL, "http://")
}

// newRegistry serves an in-process registry, through wrap when it is set.
// It is closed when the test ends, if not before.
func newRegistry(t *testing.T, wrap func(http.Handler) http.Handler) *httptest.Server {
	t.Helper()
	h := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
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

// Acquire returns the image's digest, environment, user, working directory,
// entrypoint, command and health check.
func TestAcquireReturnsTheImage(t *testing.T) {
	t.Parallel()
	img, err := mutate.Config(image(t, headerLayer(t, file("f"))),
		v1.Config{
			Env: []string{"PATH=/usr/bin:/bin", "A=b"}, User: "builder:staff", WorkingDir: "/src",
			Entrypoint: []string{"/entry", "-x"}, Cmd: []string{"serve", "--port=6379"},
			Healthcheck: &v1.HealthConfig{Test: []string{"CMD", "ping"}, Interval: time.Second, Timeout: 2 * time.Second, StartPeriod: 3 * time.Second, Retries: 4},
		})
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
	if got.User != "builder:staff" || got.WorkingDir != "/src" {
		t.Errorf("user %q, working directory %q, want builder:staff and /src", got.User, got.WorkingDir)
	}
	if !slices.Equal(got.Entrypoint, []string{"/entry", "-x"}) || !slices.Equal(got.Cmd, []string{"serve", "--port=6379"}) {
		t.Errorf("entrypoint %q, command %q, want [/entry -x] and [serve --port=6379]", got.Entrypoint, got.Cmd)
	}
	wantHC := Healthcheck{Test: []string{"CMD", "ping"}, Interval: time.Second, Timeout: 2 * time.Second, StartPeriod: 3 * time.Second, Retries: 4}
	if got.Healthcheck == nil || !reflect.DeepEqual(*got.Healthcheck, wantHC) {
		t.Errorf("health check %+v, want %+v", got.Healthcheck, wantHC)
	}
}

// An image's health check carries Docker's StartInterval, which
// go-containerregistry's config lacks, read from the raw config; an image
// with no HEALTHCHECK has none.
func TestHealthcheckReadsTheStartInterval(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw  string
		want *Healthcheck
	}{
		{`{"config":{}}`, nil},
		{
			`{"config":{"Healthcheck":{"Test":["CMD-SHELL","true"],"Interval":1000000000,"StartPeriod":2000000000,"StartInterval":500000000,"Retries":2}}}`,
			&Healthcheck{Test: []string{"CMD-SHELL", "true"}, Interval: time.Second, StartPeriod: 2 * time.Second, StartInterval: 500 * time.Millisecond, Retries: 2},
		},
		// A StartInterval that is no duration is left to its default.
		{`{"config":{"Healthcheck":{"Test":["NONE"],"StartInterval":"5s"}}}`, &Healthcheck{Test: []string{"NONE"}}},
	} {
		cfg, err := parseConfig([]byte(tc.raw))
		if err != nil {
			t.Fatalf("%s: %v", tc.raw, err)
		}
		if got := healthcheck(cfg, []byte(tc.raw)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: health check %+v, want %+v", tc.raw, got, tc.want)
		}
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
