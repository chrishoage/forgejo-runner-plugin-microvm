// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/config"
	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
)

const testImage = "docker.io/library/alpine:3.24"

// serve starts srv on a fresh unix socket and returns a client connection
// to it. The server stops when the test ends. It binds and connects through
// the open directory, as the plugin does, so the socket's path may outgrow
// bind and connect's 107 bytes: t.TempDir holds the test's name, and the
// host's temporary directory can be long.
func serve(t *testing.T, srv *Server) *grpc.ClientConn {
	t.Helper()
	dir := t.TempDir()
	lis, closeLis, err := listen(t.Context(), filepath.Join(dir, "plugin.sock"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := openSockDir(dir)
	if err != nil {
		closeLis()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, lis, srv) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
		closeLis()
		_ = d.Close()
	})
	conn, err := grpc.NewClient("unix://"+d.path("plugin.sock"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
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
	conn := serve(t, newTestServer(t, &fakeCache{}, &fakeLauncher{}))
	for _, svc := range []string{"", pluginv1alpha.BackendPlugin_ServiceDesc.ServiceName} {
		resp, err := healthv1.NewHealthClient(conn).Check(testContext(t), &healthv1.HealthCheckRequest{Service: svc})
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetStatus() != healthv1.HealthCheckResponse_SERVING {
			t.Errorf("health of %q = %v, want SERVING", svc, resp.GetStatus())
		}
	}
}

func TestCapabilities(t *testing.T) {
	t.Parallel()
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, &fakeCache{}, &fakeLauncher{})))
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
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, &fakeCache{}, &fakeLauncher{})))
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
		// The runner's PATH for a job whose image sets none.
		DefaultPathVariable: proto.String("/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"),
		Os:                  "Linux",
		Arch:                map[string]string{"amd64": "X64", "arm64": "ARM64"}[runtime.GOARCH],
	}
	if !proto.Equal(resp, want) {
		t.Errorf("Create = %v, want %v", resp, want)
	}
}

// The runner's arch follows the host's, and only amd64 and arm64 have one:
// libkrun supports no other.
func TestRunnerArch(t *testing.T) {
	t.Parallel()
	for goarch, want := range map[string]string{"amd64": "X64", "arm64": "ARM64"} {
		if got, err := runnerArch(goarch); got != want || err != nil {
			t.Errorf("runnerArch(%q) = %q, %v; want %q", goarch, got, err, want)
		}
	}
	for _, goarch := range []string{"riscv64", "386", "arm", ""} {
		got, err := runnerArch(goarch)
		if err == nil || !strings.Contains(err.Error(), `"`+goarch+`"`) {
			t.Errorf("runnerArch(%q) = %q, %v; want an error naming it", goarch, got, err)
		}
	}
}

// A job with services is well formed, but the plugin can't run them yet, so
// Create fails with Unimplemented (PROTO-6), whatever else the request
// holds, and adds no job.
func TestCreateRefusesServices(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeCache{}, &fakeLauncher{})
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	services := []*pluginv1alpha.ServiceContainer{{Name: "db", Image: "postgres"}}
	const want = "runner-microvm does not run services: the plugin protocol passes no options to configure a service's VM"
	for name, req := range map[string]*pluginv1alpha.CreateRequest{
		"label image":     {LabelArg: testImage, Services: services},
		"container image": {LabelArg: ";", Image: testImage, Services: services},
		"no image":        {Services: services},
	} {
		_, err := c.Create(testContext(t), req)
		if status.Code(err) != codes.Unimplemented || status.Convert(err).Message() != want {
			t.Errorf("%s: Create = %v; want Unimplemented: %s", name, err, want)
		}
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if n := len(srv.jobs); n != 0 {
		t.Errorf("%d jobs in the table after refused Creates", n)
	}
}

// A job's container: image is the one it boots, in place of the label's;
// without one, the label's (PROTO-4). The label's profile applies either
// way.
func TestCreateUsesTheContainerImage(t *testing.T) {
	t.Parallel()
	const container = "registry.example/team/build:1"
	cache, launcher := &fakeCache{}, &fakeLauncher{}
	srv := newTestServer(t, cache, launcher)
	srv.SetProfiles(config.Profiles{"big": bigProfile})
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	for _, tc := range []struct {
		arg, container, want string
		profile              config.Profile
	}{
		{testImage, container, container, config.BuiltinProfile()},
		{testImage + ";profile=big", container, container, bigProfile},
		{";profile=big", container, container, bigProfile},
		{"", container, container, config.BuiltinProfile()},
		{testImage + ";profile=big", "", testImage, bigProfile},
	} {
		resp, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{LabelArg: tc.arg, Image: tc.container})
		if err != nil {
			t.Fatalf("Create(label %q, container %q): %v", tc.arg, tc.container, err)
		}
		id := resp.GetEnvironmentId()
		if _, err := start(testContext(t), c, id); err != nil {
			t.Fatal(err)
		}
		if req := acquiredBy(t, cache, id); req.Ref != tc.want {
			t.Errorf("label %q, container %q: acquired %q; want %q", tc.arg, tc.container, req.Ref, tc.want)
		}
		if vm := specOf(t, launcher, id).GetVms()[0]; vm.GetCpus() != tc.profile.CPUs || vm.GetMemoryMib() != tc.profile.MemoryMiB {
			t.Errorf("label %q, container %q: VM has %d CPUs, %d MiB; want %+v",
				tc.arg, tc.container, vm.GetCpus(), vm.GetMemoryMib(), tc.profile)
		}
	}
}

// The request fields the plugin ignores are named once in its log: the
// first Create that carries any of them warns, naming which it carries,
// and no later Create warns again. The warning names the job it came
// with, unless that Create was refused before the job existed.
func TestCreateWarnsOnceOfIgnoredFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		first *pluginv1alpha.CreateRequest
		want  string
	}{
		{"backend_options", &pluginv1alpha.CreateRequest{BackendOptions: map[string]string{"flavour": "big"}}, "backend_options"},
		{"cap_add", &pluginv1alpha.CreateRequest{CapAdd: []string{"NET_ADMIN"}}, "cap_add"},
		{"cap_drop", &pluginv1alpha.CreateRequest{CapDrop: []string{"CHOWN"}}, "cap_drop"},
		{"all three", &pluginv1alpha.CreateRequest{
			BackendOptions: map[string]string{"flavour": "big"}, CapAdd: []string{"NET_ADMIN"}, CapDrop: []string{"CHOWN"},
		}, "backend_options,cap_add,cap_drop"},
		// A refused Create still carries them: they do nothing either way.
		{"refused", &pluginv1alpha.CreateRequest{CapAdd: []string{"NET_ADMIN"}, Services: []*pluginv1alpha.ServiceContainer{{Name: "db"}}}, "cap_add"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newTestServer(t, &fakeCache{}, &fakeLauncher{})
			buf := &logBuffer{}
			srv.log = slog.New(slog.NewTextHandler(buf, nil))
			c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
			if _, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{LabelArg: testImage}); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(buf.String(), "level=WARN") {
				t.Fatalf("a Create carrying no ignored field warned:\n%s", buf)
			}
			tc.first.LabelArg = testImage
			resp, err := c.Create(testContext(t), tc.first)
			wantEnv := ""
			if err == nil {
				wantEnv = "env=" + resp.GetEnvironmentId() + " "
			}
			second := &pluginv1alpha.CreateRequest{
				LabelArg: testImage, BackendOptions: map[string]string{"k": "v"}, CapAdd: []string{"SYS_ADMIN"}, CapDrop: []string{"ALL"},
			}
			if _, err := c.Create(testContext(t), second); err != nil {
				t.Fatal(err)
			}
			var warnings []string
			for line := range strings.Lines(buf.String()) {
				if strings.Contains(line, "level=WARN") {
					warnings = append(warnings, line)
				}
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], "fields="+tc.want+"\n") {
				t.Fatalf("warnings %q; want one naming fields=%s", warnings, tc.want)
			}
			if gotEnv := strings.Contains(warnings[0], "env="); wantEnv == "" && gotEnv {
				t.Errorf("the warning of a refused Create names a job: %q", warnings[0])
			} else if wantEnv != "" && !strings.Contains(warnings[0], wantEnv) {
				t.Errorf("warning %q; want it to name %s", warnings[0], wantEnv)
			}
		})
	}
}

func TestCreateGivesEachJobItsOwnID(t *testing.T) {
	t.Parallel()
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, &fakeCache{}, &fakeLauncher{})))
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
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, &fakeCache{}, &fakeLauncher{})))
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
	srv := newTestServer(t, &fakeCache{}, &fakeLauncher{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, lis, srv) }()
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

// openFDs counts the process's open descriptors.
func openFDs(t *testing.T) int {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(fds)
}
