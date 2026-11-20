// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
)

const testImage = "docker.io/library/alpine:3.24"

// serve starts srv on a fresh unix socket and returns a client connection
// to it. The server stops when the test ends.
func serve(t *testing.T, srv *Server) *grpc.ClientConn {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "plugin.sock")
	lis, err := new(net.ListenConfig).Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, lis, srv) }()
	conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return conn
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("got %v (%v), want %v", got, err, want)
	}
}

func TestHealthServing(t *testing.T) {
	t.Parallel()
	conn := serve(t, NewServer(discardLog()))
	resp, err := healthv1.NewHealthClient(conn).Check(testContext(t), &healthv1.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus() != healthv1.HealthCheckResponse_SERVING {
		t.Fatalf("health = %v, want SERVING", resp.GetStatus())
	}
}

func TestCapabilities(t *testing.T) {
	t.Parallel()
	c := pluginv1alpha.NewBackendPluginClient(serve(t, NewServer(discardLog())))
	resp, err := c.Capabilities(testContext(t), &pluginv1alpha.CapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetName() != "runner-microvm" {
		t.Fatalf("name = %q", resp.GetName())
	}
}

func TestCreateReturnsPathsAndPlatform(t *testing.T) {
	t.Parallel()
	c := pluginv1alpha.NewBackendPluginClient(serve(t, NewServer(discardLog())))
	resp, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{Name: "echo", LabelArg: testImage})
	if err != nil {
		t.Fatal(err)
	}
	id := resp.GetEnvironmentId()
	if !strings.HasPrefix(id, "job-") || len(id) != len("job-")+8 {
		t.Errorf("environment id %q is not job- and 8 hex digits", id)
	}
	resp.EnvironmentId = ""
	want := &pluginv1alpha.CreateResponse{
		RootPath:      "/",
		ActPath:       "/var/run/act",
		ToolCachePath: "/opt/hostedtoolcache",
		TempPath:      "/tmp",
		Os:            "Linux",
		Arch:          "X64",
	}
	if !proto.Equal(resp, want) {
		t.Errorf("Create = %v, want %v", resp, want)
	}
}

func TestCreateRefuses(t *testing.T) {
	t.Parallel()
	c := pluginv1alpha.NewBackendPluginClient(serve(t, NewServer(discardLog())))
	for name, req := range map[string]*pluginv1alpha.CreateRequest{
		"no image":        {},
		"container image": {Image: testImage, LabelArg: testImage},
		"label options":   {LabelArg: testImage + ";profile=big"},
		"services":        {LabelArg: testImage, Services: []*pluginv1alpha.ServiceContainer{{Name: "db", Image: "postgres"}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.Create(testContext(t), req)
			wantCode(t, err, codes.InvalidArgument)
		})
	}
}

func TestCreateGivesEachJobItsOwnID(t *testing.T) {
	t.Parallel()
	c := pluginv1alpha.NewBackendPluginClient(serve(t, NewServer(discardLog())))
	seen := map[string]bool{}
	for range 20 {
		resp, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{LabelArg: testImage})
		if err != nil {
			t.Fatal(err)
		}
		if seen[resp.GetEnvironmentId()] {
			t.Fatalf("environment id %s returned twice", resp.GetEnvironmentId())
		}
		seen[resp.GetEnvironmentId()] = true
	}
}

func TestRemove(t *testing.T) {
	t.Parallel()
	c := pluginv1alpha.NewBackendPluginClient(serve(t, NewServer(discardLog())))
	ctx := testContext(t)
	_, err := c.Remove(ctx, &pluginv1alpha.RemoveRequest{EnvironmentId: "job-00000000"})
	wantCode(t, err, codes.NotFound)

	resp, err := c.Create(ctx, &pluginv1alpha.CreateRequest{LabelArg: testImage})
	if err != nil {
		t.Fatal(err)
	}
	id := resp.GetEnvironmentId()
	if _, err := c.Remove(ctx, &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatal(err)
	}
	_, err = c.Remove(ctx, &pluginv1alpha.RemoveRequest{EnvironmentId: id})
	wantCode(t, err, codes.NotFound)
}

func TestDirsFromEnv(t *testing.T) {
	t.Parallel()
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	got, err := DirsFromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got != builtinDirs {
		t.Errorf("with no %s: %+v, want the built-in paths", BaseDirEnv, got)
	}
	got, err = DirsFromEnv(env(map[string]string{BaseDirEnv: "/srv/rm"}))
	if err != nil {
		t.Fatal(err)
	}
	want := Dirs{
		State:   "/srv/rm/state",
		Cache:   "/srv/rm/cache",
		Runtime: "/srv/rm/runtime",
		Listen:  "/srv/rm/runtime/runner-microvm.sock",
	}
	if got != want {
		t.Errorf("DirsFromEnv = %+v, want %+v", got, want)
	}
}

// A socket in a directory deeper than bind's 107-byte limit still binds and
// connects through the open directory.
func TestSockDirDeepPath(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), strings.Repeat("d", 60), strings.Repeat("e", 60))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lis, closeLis, err := listen(t.Context(), filepath.Join(dir, "plugin.sock"))
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan error, 1)
	go func() {
		c, err := lis.Accept()
		if err == nil {
			err = c.Close()
		}
		accepted <- err
	}()
	d, err := openSockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	c, err := new(net.Dialer).DialContext(t.Context(), "unix", d.path("plugin.sock"))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	closeLis()
	if _, err := os.Lstat(filepath.Join(dir, "plugin.sock")); !os.IsNotExist(err) {
		t.Errorf("socket left after close: %v", err)
	}
}

// Cancelling Serve's context ends a stream that is still open, closes the
// listener, and returns.
func TestServeStopsWithAStreamOpen(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(t.TempDir(), "plugin.sock")
	lis, err := new(net.ListenConfig).Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, lis, NewServer(discardLog())) }()
	conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	watch, err := healthv1.NewHealthClient(conn).Watch(testContext(t), &healthv1.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := watch.Recv(); err != nil {
		t.Fatal(err)
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return with a stream open")
	}
	// The stream ends at once, not when the test's context does.
	if _, err := watch.Recv(); status.Code(err) != codes.Unavailable {
		t.Errorf("the open stream got %v, want Unavailable", err)
	}
	if c, err := new(net.Dialer).DialContext(t.Context(), "unix", sock); err == nil {
		_ = c.Close()
		t.Error("the listener still accepts after Serve returned")
	}
}

// Startup fails, naming the path, when a file is already at the listen
// socket's path, and leaves the file and no open descriptor behind. Not
// parallel: it counts the process's open descriptors.
func TestListenRefusesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "stale.sock")
	l, err := new(net.ListenConfig).Listen(t.Context(), "unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = l.Close()
	file := filepath.Join(dir, "file.sock")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{stale, file} {
		before := openFDs(t)
		lis, closeLis, err := listen(t.Context(), path)
		if err == nil {
			closeLis()
			t.Fatalf("listen on %s, where a file is, succeeded on %v", path, lis.Addr())
		}
		if msg := err.Error(); !strings.Contains(msg, path) || !strings.Contains(msg, "a file is already there") {
			t.Errorf("error %q does not say a file is at %s", msg, path)
		}
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("the file at %s is gone: %v", path, err)
		}
		if after := openFDs(t); after != before {
			t.Errorf("%d open descriptors after a failed listen, %d before", after, before)
		}
	}
}

// openFDs counts the process's open descriptors.
func openFDs(t *testing.T) int {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(fds)
}
