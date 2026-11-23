// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
)

// create creates a job on c and returns its environment id.
func create(t *testing.T, c pluginv1alpha.BackendPluginClient) string {
	t.Helper()
	resp, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{LabelArg: testImage})
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetEnvironmentId()
}

// start runs Start on id to its end, and returns its StartComplete.
func start(ctx context.Context, c pluginv1alpha.BackendPluginClient, id string) (*pluginv1alpha.StartComplete, error) {
	stream, err := c.Start(ctx, &pluginv1alpha.StartRequest{EnvironmentId: id})
	if err != nil {
		return nil, err
	}
	var done *pluginv1alpha.StartComplete
	for {
		out, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return done, nil
		}
		if err != nil {
			return nil, err
		}
		if sc := out.GetStartComplete(); sc != nil {
			done = sc
		}
	}
}

func TestStartBootsTheVM(t *testing.T) {
	t.Parallel()
	cache, launcher := &fakeCache{}, &fakeLauncher{}
	srv := newTestServer(t, cache, launcher)
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	id := create(t, c)
	done, err := start(testContext(t), c, id)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"PATH": "/usr/bin:/bin", "A": "b=c"}; !maps.Equal(done.GetImageEnv(), want) {
		t.Errorf("image env = %v, want %v", done.GetImageEnv(), want)
	}
	state := filepath.Join(srv.cfg.Dirs.State, "jobs", id)
	root := filepath.Join(state, "0", "root")
	if req := cache.acquired[0]; req.Job != id || req.VM != 0 || req.Ref != testImage {
		t.Errorf("Acquire(%+v)", req)
	}
	// The boot root: mount points, a hard link to the agent, and the boot
	// config.
	if got := names(t, root); strings.Join(got, " ") != ".runner-microvm-agent .runner-microvm-boot dev proc sys" {
		t.Errorf("the boot root holds %v", got)
	}
	agent, err := os.Stat(srv.cfg.Agent)
	if err != nil {
		t.Fatal(err)
	}
	if linked, err := os.Stat(filepath.Join(root, ".runner-microvm-agent")); err != nil || !os.SameFile(agent, linked) {
		t.Errorf("the boot root's agent is not a hard link to %s: %v", srv.cfg.Agent, err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".runner-microvm-boot"))
	if err != nil {
		t.Fatal(err)
	}
	boot := &agentv1.BootConfig{}
	if err := proto.Unmarshal(b, boot); err != nil {
		t.Fatal(err)
	}
	wantBoot := &agentv1.BootConfig{
		LayerDir: "/dev/.runner-microvm/layers", Layers: []string{"0.erofs", "1.erofs"}, ScratchDevice: "/dev/vda",
		Network: &agentv1.Network{Address: "198.18.0.2/24", Gateway: "198.18.0.1", Mtu: 1500},
		Image:   &agentv1.Image{Env: []string{"PATH=/usr/bin:/bin", "A=b=c"}},
	}
	if !proto.Equal(boot, wantBoot) {
		t.Errorf("boot config = %v, want %v", boot, wantBoot)
	}
	spec := launcher.specs[0]
	if spec.GetJob() != id || spec.GetJobBinary() != "/fake/runner-microvm-job" || spec.GetMkfsExt4() != "/fake/mkfs.ext4" ||
		len(spec.GetVms()) != 1 {
		t.Fatalf("job spec = %v", spec)
	}
	wantNet := &jobv1.Network{Subnet: "198.18.0.0/24", GatewayAddress: "198.18.0.1", GatewayMac: "02:00:c6:12:00:01", Mtu: 1500}
	if !proto.Equal(spec.GetNetwork(), wantNet) {
		t.Errorf("job network = %v, want %v", spec.GetNetwork(), wantNet)
	}
	vm := spec.GetVms()[0]
	if vm.GetRootDir() != root || vm.GetAgentPort() != 1024 || vm.GetCpus() != 2 || vm.GetMemoryMib() != 1024 ||
		vm.GetAgentSocket() != filepath.Join(srv.cfg.Dirs.Runtime, id, "0", "agent.sock") ||
		vm.GetConsole() != filepath.Join(state, "0", "console") || vm.GetVmLog() != filepath.Join(state, "0", "vm.log") ||
		vm.GetScratchDisk() != filepath.Join(state, "0", "scratch.ext4") || vm.GetScratchSizeBytes() != 10<<30 ||
		vm.GetLayerDir() != "/cache/jobs/"+id+"/0" || !proto.Equal(vm.GetBoot(), wantBoot) ||
		vm.GetGuestAddress() != "198.18.0.2" || vm.GetGuestMac() != "02:00:c6:12:00:02" {
		t.Errorf("VM = %v", vm)
	}
	if launcher.logs[0] != filepath.Join(state, "job.log") {
		t.Errorf("job process log = %s", launcher.logs[0])
	}

	// Started once only.
	_, err = start(testContext(t), c, id)
	wantCode(t, err, codes.FailedPrecondition)

	// Remove ends the job process and deletes the job's files.
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatal(err)
	}
	if launcher.alive() != 0 {
		t.Error("the job process outlived Remove")
	}
	for _, d := range []string{state, filepath.Join(srv.cfg.Dirs.Runtime, id)} {
		if _, err := os.Lstat(d); !os.IsNotExist(err) {
			t.Errorf("%s outlived Remove: %v", d, err)
		}
	}
	if len(cache.released) != 1 || cache.released[0] != id {
		t.Errorf("released %v", cache.released)
	}
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

func TestStartUnknownJob(t *testing.T) {
	t.Parallel()
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, &fakeCache{}, &fakeLauncher{})))
	_, err := start(testContext(t), c, "job-00000000")
	wantCode(t, err, codes.NotFound)
}

// Every failed Start leaves no job process behind, and its error names the
// cause (PROTO-10, VM-8).
func TestStartFails(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		cache    *fakeCache
		launcher *fakeLauncher
		want     string
	}{
		"pull":              {&fakeCache{err: errors.New("manifest unknown")}, &fakeLauncher{}, "image " + testImage + ": manifest unknown"},
		"launch":            {&fakeCache{}, &fakeLauncher{err: errors.New("exec format error")}, "job process: exec format error"},
		"VM exits":          {&fakeCache{}, &fakeLauncher{does: exitsDuringBoot}, "exited during boot with code 1; console: Kernel panic"},
		"agent never ready": {&fakeCache{}, &fakeLauncher{does: hangs}, "the agent did not answer within"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newTestServer(t, tt.cache, tt.launcher)
			srv.cfg.ReadyTimeout = 200 * time.Millisecond
			c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
			id := create(t, c)
			_, err := start(testContext(t), c, id)
			wantCode(t, err, codes.Unavailable)
			if !strings.Contains(status.Convert(err).Message(), tt.want) {
				t.Errorf("error %q does not name %q", status.Convert(err).Message(), tt.want)
			}
			if tt.launcher.alive() != 0 {
				t.Error("a failed Start left its job process running")
			}
			if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
				t.Fatalf("Remove after a failed Start: %v", err)
			}
		})
	}
}

// A job network that fails to start fails Start with FailedPrecondition,
// naming the network, and leaves no job process behind.
func TestStartFailsOnTheNetwork(t *testing.T) {
	t.Parallel()
	launcher := &fakeLauncher{does: networkFails}
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, &fakeCache{}, launcher)))
	_, err := start(testContext(t), c, create(t, c))
	wantCode(t, err, codes.FailedPrecondition)
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "network: cannot reserve gateway IP") {
		t.Errorf("error %q does not name the network", msg)
	}
	if launcher.alive() != 0 {
		t.Error("a failed Start left its job process running")
	}
}

// Remove succeeds while a Start is still pulling, and the Start ends
// (PROTO-13).
func TestRemoveDuringStart(t *testing.T) {
	t.Parallel()
	cache := &fakeCache{block: true, entered: make(chan struct{})}
	launcher := &fakeLauncher{}
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, cache, launcher)))
	id := create(t, c)
	started := make(chan error, 1)
	go func() {
		_, err := start(testContext(t), c, id)
		started <- err
	}()
	<-cache.entered
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatal(err)
	}
	if err := <-started; err == nil {
		t.Fatal("Start succeeded on a removed job")
	}
	if len(launcher.specs) != 0 {
		t.Error("a job process was launched for a removed job")
	}
}

// A cancelled Start kills the job process before it returns.
func TestCancelledStartKillsTheJobProcess(t *testing.T) {
	t.Parallel()
	launcher := &fakeLauncher{does: hangs}
	srv := newTestServer(t, &fakeCache{}, launcher)
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	id := create(t, c)
	ctx, cancel := context.WithTimeout(testContext(t), 200*time.Millisecond)
	defer cancel()
	_, err := start(ctx, c, id)
	if err == nil {
		t.Fatal("a cancelled Start succeeded")
	}
	// The server notices the cancel by itself; Remove waits for it.
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatal(err)
	}
	if launcher.alive() != 0 {
		t.Error("the job process outlived a cancelled Start and Remove")
	}
	if launcher.kills != 1 {
		t.Errorf("%d kills, want 1", launcher.kills)
	}
}

// Close, as at shutdown, ends every job's process.
func TestCloseEndsEveryJob(t *testing.T) {
	t.Parallel()
	launcher := &fakeLauncher{}
	srv := newTestServer(t, &fakeCache{}, launcher)
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	for range 3 {
		if _, err := start(testContext(t), c, create(t, c)); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := launcher.alive(); n != 0 {
		t.Errorf("%d job processes outlived Close", n)
	}
}

func TestRemoveTreeReadOnly(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "job")
	if err := os.MkdirAll(filepath.Join(dir, "ro", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ro", "deeper", "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(dir, "ro", "deeper"), filepath.Join(dir, "ro")} {
		if err := os.Chmod(d, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeTree(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("%s is left: %v", dir, err)
	}
	if err := removeTree(dir); err != nil {
		t.Errorf("removeTree of a missing directory: %v", err)
	}
}

// A Start after a failed Start is refused, naming the first failure: the
// failed Start left the VM's files half made.
func TestStartAfterAFailedStart(t *testing.T) {
	t.Parallel()
	launcher := &fakeLauncher{err: errors.New("exec format error")}
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, &fakeCache{}, launcher)))
	id := create(t, c)
	_, err := start(testContext(t), c, id)
	wantCode(t, err, codes.Unavailable)
	_, err = start(testContext(t), c, id)
	wantCode(t, err, codes.FailedPrecondition)
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "failed to start earlier") || !strings.Contains(msg, "exec format error") {
		t.Errorf("error %q does not name the earlier failure", msg)
	}
	if n := len(launcher.specs); n != 1 {
		t.Errorf("%d launches, want 1", n)
	}
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatal(err)
	}
}

// await returns as soon as its context ends, while its op still blocks,
// and the op ends on its own later.
func TestAwaitReturnsWhenCancelled(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		release := make(chan struct{})
		returned := make(chan error, 1)
		go func() { returned <- await(ctx, func() error { <-release; return nil }) }()
		synctest.Wait()
		select {
		case err := <-returned:
			t.Fatalf("await returned %v while its op blocks", err)
		default:
		}
		cancel()
		wantCode(t, <-returned, codes.Canceled)
		// As gRPC ends the stream once the handler returns.
		close(release)
	})
}

// A relative library directory is made absolute, because the VM child
// changes its working directory before it loads libkrun.
func TestLibDirFromEnv(t *testing.T) {
	t.Parallel()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for env, want := range map[string]string{
		"":         "",
		"/opt/lib": "/opt/lib",
		"lib/krun": filepath.Join(wd, "lib/krun"),
	} {
		got, err := libDirFromEnv(func(k string) string {
			if k == LibDirEnv {
				return env
			}
			return ""
		})
		if err != nil || got != want {
			t.Errorf("%s=%q: %q, %v; want %q", LibDirEnv, env, got, err, want)
		}
	}
}

// Startup deletes everything in <state>/bin/ but the job binary and agent it
// just wrote: stale hashes and a crashed write's temp files.
func TestPruneDirKeepsOnlyTheCurrentFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, n := range []string{"runner-microvm-job-new", "runner-microvm-agent-new", "runner-microvm-agent-old", ".tmp-123"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneDir(dir, filepath.Join(dir, "runner-microvm-job-new"), filepath.Join(dir, "runner-microvm-agent-new")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(t, dir), " "); got != "runner-microvm-agent-new runner-microvm-job-new" {
		t.Errorf("bin/ holds %s", got)
	}
}
