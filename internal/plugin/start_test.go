// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	// Only to read the boot config as the agent does: the plugin itself
	// never imports the agent.
	guestagent "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/agent"
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
	resp, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{Name: "FORGEJO-ACTIONS-TASK-7_JOB-Build", LabelArg: testImage})
	if err != nil {
		t.Fatal(err)
	}
	id := resp.GetEnvironmentId()
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
	// The boot root: mount points, and in .runner-microvm a hard link to
	// the agent and the boot config.
	if got := names(t, root); strings.Join(got, " ") != ".runner-microvm dev proc sys" {
		t.Errorf("the boot root holds %v", got)
	}
	if got := names(t, filepath.Join(root, ".runner-microvm")); strings.Join(got, " ") != "agent boot-config.binpb" {
		t.Errorf("the boot root's .runner-microvm holds %v", got)
	}
	agent, err := os.Stat(srv.cfg.Agent)
	if err != nil {
		t.Fatal(err)
	}
	if linked, err := os.Stat(filepath.Join(root, ".runner-microvm", "agent")); err != nil || !os.SameFile(agent, linked) {
		t.Errorf("the boot root's agent is not a hard link to %s: %v", srv.cfg.Agent, err)
	}
	// The agent reads the file the plugin wrote, at the path it reads, and
	// gets back the message the plugin built, its vsock port included.
	bootFile := filepath.Join(root, guestagent.BootConfigPath)
	if fi, err := os.Stat(bootFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the boot config at %s: %v, %v", guestagent.BootConfigPath, fi, err)
	}
	boot, err := guestagent.ReadBootConfig(bootFile)
	if err != nil {
		t.Fatal(err)
	}
	// The job's name made DNS-safe is the VM's hostname, and the hosts
	// list names it at its address; there is no search domain to carry.
	wantBoot := &agentv1.BootConfig{
		LayerDir: "/dev/.runner-microvm/layers", LayerTag: "layers", Layers: []string{"0.erofs", "1.erofs"}, ScratchDevice: "/dev/vda",
		Network:  &agentv1.Network{Address: "198.18.0.2/24", Gateway: "198.18.0.1", Mtu: 1500},
		Hostname: "forgejo-actions-task-7-job-build",
		Hosts:    []*agentv1.Host{{Name: "forgejo-actions-task-7-job-build", Address: "198.18.0.2"}},
		// The image's defaults for Exec, and the default PATH Create
		// returns to the runner.
		Image: &agentv1.Image{
			Env: []string{"PATH=/usr/bin:/bin", "A=b=c"}, User: "builder:staff", WorkingDir: "/src",
			DefaultPath: "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		},
		VsockPort: 1024,
		GuestDirs: []*agentv1.GuestDir{
			{Path: "/var/run/act", Mode: 0o777}, {Path: "/opt/hostedtoolcache", Mode: 0o777}, {Path: "/tmp", Mode: 0o1777},
		},
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
	if vm.GetName() != "forgejo-actions-task-7-job-build" || vm.GetRootDir() != root || vm.GetAgentPort() != 1024 || vm.GetCpus() != 2 || vm.GetMemoryMib() != 1024 ||
		vm.GetAgentSocket() != filepath.Join(srv.cfg.Dirs.Runtime, id, "0", "agent.sock") ||
		vm.GetConsole() != filepath.Join(state, "0", "console") || vm.GetVmLog() != filepath.Join(state, "0", "vm.log") ||
		vm.GetScratchDisk() != filepath.Join(state, "0", "scratch.ext4") || vm.GetScratchSizeBytes() != 10<<30 ||
		vm.GetLayerDir() != filepath.Join(srv.cfg.Dirs.Cache, "jobs", id, "0") || vm.GetLayerTag() != "layers" || !proto.Equal(vm.GetBoot(), wantBoot) ||
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

// A job whose name maps to nothing is `job` on the job network.
func TestStartNamesAnUnnamedJobJob(t *testing.T) {
	t.Parallel()
	cache, launcher := &fakeCache{}, &fakeLauncher{}
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, cache, launcher)))
	resp, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{Name: "_.!", LabelArg: testImage})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := start(testContext(t), c, resp.GetEnvironmentId()); err != nil {
		t.Fatal(err)
	}
	vm := launcher.specs[0].GetVms()[0]
	if vm.GetName() != "job" || vm.GetBoot().GetHostname() != "job" ||
		!proto.Equal(vm.GetBoot().GetHosts()[0], &agentv1.Host{Name: "job", Address: "198.18.0.2"}) {
		t.Errorf("VM %q, hostname %q, hosts %v", vm.GetName(), vm.GetBoot().GetHostname(), vm.GetBoot().GetHosts())
	}
}

func TestStartUnknownJob(t *testing.T) {
	t.Parallel()
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, &fakeCache{}, &fakeLauncher{})))
	_, err := start(testContext(t), c, "job-00000000")
	wantCode(t, err, codes.NotFound)
}

// What a failed Start's error carries of a VM that wrote bootLog and
// bootConsole: under their headings, the last 20 lines of each, at most
// their last 4 KiB, with their newlines (PROTO-10, VM-8). The VM log's 4
// KiB start inside its sixth line, which is cut, and its last 20 lines are
// all of those; the console's 4 KiB hold all 25 of its lines, of which the
// last 20 are kept.
var bootOutput = "\nVM log:\n" + bootLog[len(bootLog)-4096:len(bootLog)-1] +
	"\nconsole:\n" + bootConsole[strings.Index(bootConsole, "console line 6\n"):len(bootConsole)-1]

// Every failed Start fails with FailedPrecondition, since the runner reads
// Unavailable as the plugin being gone, and its error names the cause
// first, with the VM's last output once the VM got as far as booting
// (PROTO-10, VM-8). It leaves no job process behind, and the job is still
// removed.
func TestStartFails(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		cache    *fakeCache
		launcher *fakeLauncher
		want     func(srv *Server, id string) string
	}{
		"pull": {&fakeCache{err: errors.New("manifest unknown")}, &fakeLauncher{}, func(*Server, string) string {
			return "image " + testImage + ": manifest unknown"
		}},
		"launch": {&fakeCache{}, &fakeLauncher{err: errors.New("exec format error")}, func(*Server, string) string {
			return "job process: exec format error"
		}},
		"network": {&fakeCache{}, &fakeLauncher{does: networkFails}, func(*Server, string) string {
			return "network: cannot reserve gateway IP"
		}},
		"scratch disk": {&fakeCache{}, &fakeLauncher{does: scratchFails}, func(srv *Server, id string) string {
			return "scratch disk: " + filepath.Join(srv.cfg.Dirs.State, "jobs", id, "0", "scratch.ext4") + ": no space left on device"
		}},
		"VM exits": {&fakeCache{}, &fakeLauncher{does: exitsDuringBoot}, func(*Server, string) string {
			return "VM job exited during boot (1)" + bootOutput
		}},
		"agent never ready": {&fakeCache{}, &fakeLauncher{does: hangs}, func(*Server, string) string {
			return "VM job: the agent did not answer within 200ms" + bootOutput
		}},
		"job process exits": {&fakeCache{}, &fakeLauncher{does: exitsSilently}, func(*Server, string) string {
			return "VM job: its job process exited during boot" + bootOutput
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newTestServer(t, tt.cache, tt.launcher)
			srv.cfg.ReadyTimeout = 200 * time.Millisecond
			c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
			id := create(t, c)
			_, err := start(testContext(t), c, id)
			if want := tt.want(srv, id); status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != want {
				t.Errorf("Start = %v; want FailedPrecondition: %q", err, want)
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

// A job process that fails on the job spec, which only the plugin's bug can
// cause, fails Start with Internal, not FailedPrecondition, naming the spec.
func TestStartFailsOnThePluginsBug(t *testing.T) {
	t.Parallel()
	launcher := &fakeLauncher{does: specUnreadable}
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, &fakeCache{}, launcher)))
	id := create(t, c)
	_, err := start(testContext(t), c, id)
	if want := "read the job spec: unexpected EOF"; status.Code(err) != codes.Internal || status.Convert(err).Message() != want {
		t.Errorf("Start = %v; want Internal: %q", err, want)
	}
	if launcher.alive() != 0 {
		t.Error("a failed Start left its job process running")
	}
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatalf("Remove after a failed Start: %v", err)
	}
}

// A Start whose stream the runner cancels, or whose deadline passes, fails
// with its context's code, while pulling and while booting, and leaves no
// job process behind. (A Start that Remove ends fails with Canceled:
// TestRemoveWhileACancelledCallUnwinds.)
func TestCancelledStartFailsWithItsContextsCode(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		ctx  func(context.Context) (context.Context, context.CancelFunc)
		want codes.Code
	}{
		"cancelled": {context.WithCancel, codes.Canceled},
		"deadline": {func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, time.Second)
		}, codes.DeadlineExceeded},
	} {
		for stage, cache := range map[string]*fakeCache{
			"pulling": {block: true, entered: make(chan string, 1)},
			"booting": {},
		} {
			t.Run(name+" "+stage, func(t *testing.T) {
				t.Parallel()
				launcher := &fakeLauncher{does: hangs}
				srv := newTestServer(t, cache, launcher)
				id := createOn(t, srv, testImage)
				ctx, cancel := tt.ctx(testContext(t))
				defer cancel()
				stream := &startStream{ctx: ctx}
				started := make(chan error, 1)
				go func() { started <- srv.Start(&pluginv1alpha.StartRequest{EnvironmentId: id}, stream) }()
				if cache.block {
					within(t, cache.entered, "Start reaching the image cache")
				} else {
					waitFor(t, "the job process's launch", func() bool { return launcher.launched() == 1 })
				}
				if tt.want == codes.Canceled {
					cancel()
				}
				if err := within(t, started, "Start's return"); status.Code(err) != tt.want {
					t.Errorf("Start = %v, want %v", err, tt.want)
				}
				if launcher.alive() != 0 {
					t.Error("the cancelled Start left its job process running")
				}
			})
		}
	}
}

// waitFor waits up to 10 s for cond, failing t if it never holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// vmOutput keeps, of a VM's log and console, the last 20 lines of the last
// 4 KiB of each, with their newlines and without a trailing one, less any
// invalid UTF-8 and carriage returns, and leaves out an empty one's
// heading.
func TestVMOutput(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct{ log, console, want string }{
		"both":           {"a\nb\n", "c\n", "\nVM log:\na\nb\nconsole:\nc"},
		"neither":        {"", "\n", ""},
		"only the log":   {"a", "", "\nVM log:\na"},
		"only console":   {"", "panic\n\n", "\nconsole:\npanic"},
		"CRLF":           {"", "a\r\nb\r\n", "\nconsole:\na\nb"},
		"invalid UTF-8":  {"", "\x80bad \xffbytes", "\nconsole:\nbad bytes"},
		"21 lines":       {"", strings.Repeat("x\n", 20) + "y\n", "\nconsole:\n" + strings.Repeat("x\n", 19) + "y"},
		"over 4 KiB":     {strings.Repeat("z", 5000), "", "\nVM log:\n" + strings.Repeat("z", 4096)},
		"20 blank lines": {"", "a\n" + strings.Repeat("\n", 20) + "b", "\nconsole:\n" + strings.Repeat("\n", 19) + "b"},
		"exactly 4 KiB":  {"", strings.Repeat("q", 4095) + "\n", "\nconsole:\n" + strings.Repeat("q", 4095)},
		"only newlines":  {"\n\n\n", "", ""},
	} {
		if got := vmOutput(tt.log, tt.console); got != tt.want {
			t.Errorf("%s: vmOutput = %q, want %q", name, got, tt.want)
		}
	}
}

// tail reads the last 4 KiB of a file; a missing file has none.
func TestTail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	long := filepath.Join(dir, "long")
	content := strings.Repeat("0123456789", 500)
	if err := os.WriteFile(long, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tail(long); got != content[len(content)-4096:] {
		t.Errorf("tail of 5000 bytes = %d bytes, want the last 4096", len(got))
	}
	if got := tail(filepath.Join(dir, "missing")); got != "" {
		t.Errorf("tail of a missing file = %q", got)
	}
}

// Remove succeeds while a Start is still pulling, and the Start ends
// (PROTO-13).
func TestRemoveDuringStart(t *testing.T) {
	t.Parallel()
	cache := &fakeCache{block: true, entered: make(chan string, 1)}
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
	wantCode(t, err, codes.FailedPrecondition)
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

// An image environment entry without `=` names no variable, and is left
// out of StartComplete's image environment, as Exec leaves it out.
func TestEnvMapDropsEntriesWithoutEquals(t *testing.T) {
	t.Parallel()
	got := envMap([]string{"PATH=/bin", "LONELY", "EMPTY=", "A=b=c"})
	if want := map[string]string{"PATH": "/bin", "EMPTY": "", "A": "b=c"}; !maps.Equal(got, want) {
		t.Errorf("envMap = %v, want %v", got, want)
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

// A successful Start sends three progress lines on stdout before
// StartComplete, in order: the image, the VM, the total, each prefixed with
// the seconds since Start (PROTO-9).
func TestStartSendsItsProgress(t *testing.T) {
	t.Parallel()
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, &fakeCache{}, &fakeLauncher{})))
	stream, err := c.Start(testContext(t), &pluginv1alpha.StartRequest{EnvironmentId: create(t, c)})
	if err != nil {
		t.Fatal(err)
	}
	var outs []*pluginv1alpha.StartOutput
	for {
		out, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		outs = append(outs, out)
	}
	const at = `^\[runner-microvm \d+\.\d\ds\] `
	want := []*regexp.Regexp{
		regexp.MustCompile(at + `image ` + regexp.QuoteMeta(testImage) + ` ready \(sha256:0, 2 layers\) in \d+\.\d\ds\n$`),
		regexp.MustCompile(at + `VM job \(2 CPUs, 1024 MiB\) ready in \d+\.\d\ds\n$`),
		regexp.MustCompile(at + `started in \d+\.\d\ds\n$`),
	}
	if len(outs) != len(want)+1 || outs[len(want)].GetStartComplete() == nil {
		t.Fatalf("Start sent %v, want three progress lines, then StartComplete", outs)
	}
	for i, re := range want {
		d := outs[i].GetData()
		if d.GetStream() != pluginv1alpha.DataChunk_STDOUT || !re.Match(d.GetData()) {
			t.Errorf("output %d = %v %q, want stdout matching %s", i, d.GetStream(), d.GetData(), re)
		}
	}
}

// progressStream is the runner's side of a Start, whose Send of a progress
// line is data's, and of StartComplete is complete's, when set, and records
// it.
type progressStream struct {
	grpc.ServerStream
	ctx      context.Context
	data     func() error
	complete func() error

	mu        sync.Mutex
	sending   bool
	completed bool
}

func (s *progressStream) Context() context.Context { return s.ctx }

func (s *progressStream) Send(out *pluginv1alpha.StartOutput) error {
	s.mu.Lock()
	if s.sending {
		s.mu.Unlock()
		panic("Send called while another Send still runs")
	}
	s.sending = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.sending = false; s.mu.Unlock() }()
	if out.GetStartComplete() != nil {
		if s.complete != nil {
			if err := s.complete(); err != nil {
				return err
			}
		}
		s.mu.Lock()
		s.completed = true
		s.mu.Unlock()
		return nil
	}
	return s.data()
}

// A progress line the runner can't take doesn't fail Start: only
// StartComplete must arrive (PROTO-9).
func TestAFailedProgressLineDoesNotFailStart(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeCache{}, &fakeLauncher{})
	stream := &progressStream{ctx: testContext(t), data: func() error { return errors.New("the stream is full") }}
	if err := srv.Start(&pluginv1alpha.StartRequest{EnvironmentId: createOn(t, srv, testImage)}, stream); err != nil {
		t.Fatalf("Start = %v, want success", err)
	}
	if !stream.completed {
		t.Error("Start never sent StartComplete")
	}
}

// A Start whose runner never takes a progress line still ends at once when
// it is cancelled, with Canceled, and leaves no job process behind; it
// sends nothing more while that line's Send may still run.
func TestAStalledProgressLineEndsWithItsStart(t *testing.T) {
	t.Parallel()
	launcher := &fakeLauncher{}
	srv := newTestServer(t, &fakeCache{}, launcher)
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	// As gRPC ends the stream once the handler returns.
	t.Cleanup(func() { close(release) })
	stream := &progressStream{ctx: ctx, data: func() error {
		entered <- struct{}{}
		<-release
		return errors.New("the stream ended")
	}}
	id := createOn(t, srv, testImage)
	started := make(chan error, 1)
	go func() { started <- srv.Start(&pluginv1alpha.StartRequest{EnvironmentId: id}, stream) }()
	within(t, entered, "the first progress line")
	cancel()
	if err := within(t, started, "Start's return"); status.Code(err) != codes.Canceled {
		t.Errorf("Start = %v, want Canceled", err)
	}
	if launcher.alive() != 0 {
		t.Error("the cancelled Start left its job process running")
	}
}

// A Start cancelled once its VM is ready, while it sends its last progress
// line or StartComplete, fails with Canceled as any cancelled Start does: it
// kills the job process, and a second Start names the failure.
func TestStartCancelledOnceItsVMIsReady(t *testing.T) {
	t.Parallel()
	for name, last := range map[string]bool{"the started line": false, "StartComplete": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			launcher := &fakeLauncher{}
			srv := newTestServer(t, &fakeCache{}, launcher)
			ctx, cancel := context.WithCancel(testContext(t))
			defer cancel()
			entered := make(chan struct{}, 1)
			// The send blocks until the Start's context ends.
			block := func() error {
				entered <- struct{}{}
				<-ctx.Done()
				return status.FromContextError(ctx.Err()).Err()
			}
			lines := 0
			stream := &progressStream{ctx: ctx, data: func() error {
				lines++
				if !last && lines == 3 {
					return block()
				}
				return nil
			}}
			if last {
				stream.complete = block
			}
			id := createOn(t, srv, testImage)
			started := make(chan error, 1)
			go func() { started <- srv.Start(&pluginv1alpha.StartRequest{EnvironmentId: id}, stream) }()
			within(t, entered, "the blocked send")
			cancel()
			if err := within(t, started, "Start's return"); status.Code(err) != codes.Canceled {
				t.Errorf("Start = %v, want Canceled", err)
			}
			if launcher.alive() != 0 {
				t.Error("the cancelled Start left its job process running")
			}
			err := srv.Start(&pluginv1alpha.StartRequest{EnvironmentId: id}, &progressStream{ctx: testContext(t), data: func() error { return nil }})
			if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "failed to start earlier") {
				t.Errorf("a second Start = %v, want FailedPrecondition naming the earlier failure", err)
			}
		})
	}
}

// A Start whose StartComplete send the stream's end cut off fails with
// Canceled even while its own context, derived from the stream's, does not
// yet report its end: Go ends a parent context's Done before it cancels the
// contexts derived from it, so the send can fail first.
func TestStartCutOffBeforeItsContextEnds(t *testing.T) {
	t.Parallel()
	launcher := &fakeLauncher{}
	srv := newTestServer(t, &fakeCache{}, launcher)
	ctx := &endingContext{Context: testContext(t)}
	stream := &progressStream{ctx: ctx, data: func() error { return nil }, complete: func() error {
		ctx.ended.Store(true)
		return status.Error(codes.Canceled, "the stream ended")
	}}
	id := createOn(t, srv, testImage)
	if err := srv.Start(&pluginv1alpha.StartRequest{EnvironmentId: id}, stream); status.Code(err) != codes.Canceled {
		t.Errorf("Start = %v, want Canceled", err)
	}
	if launcher.alive() != 0 {
		t.Error("the cut-off Start left its job process running")
	}
}

// endingContext is a stream's context that reports its end through Err once
// ended is set, before the contexts derived from it know: its Done never
// closes.
type endingContext struct {
	context.Context
	ended atomic.Bool
}

func (c *endingContext) Err() error {
	if c.ended.Load() {
		return context.Canceled
	}
	return c.Context.Err()
}

// A Start whose StartComplete the runner takes, and which it cancels at
// once, as its client does once it reads StartComplete, has succeeded: the
// Send returned without error, so its job process keeps running.
func TestStartCancelledAsStartCompleteArrives(t *testing.T) {
	t.Parallel()
	launcher := &fakeLauncher{}
	srv := newTestServer(t, &fakeCache{}, launcher)
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	stream := &progressStream{ctx: ctx, data: func() error { return nil }, complete: func() error {
		cancel()
		return nil
	}}
	id := createOn(t, srv, testImage)
	if err := srv.Start(&pluginv1alpha.StartRequest{EnvironmentId: id}, stream); err != nil {
		t.Fatalf("Start = %v, want it to succeed", err)
	}
	if launcher.alive() != 1 {
		t.Error("the Start whose StartComplete was sent ended its job process")
	}
	if _, err := srv.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatal(err)
	}
}

// awaitDelivery returns the Send's own result once the stream's context
// ends during it, even when the Send returns only after the call's context
// has ended: a Send that succeeded wins over a cancel at the same moment,
// and one the stream's end cut off fails.
func TestAwaitDeliveryTakesTheSendsResult(t *testing.T) {
	t.Parallel()
	for name, sendErr := range map[string]error{"sent": nil, "cut off": status.Error(codes.Canceled, "stream ended")} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				streamCtx, cancel := context.WithCancel(t.Context())
				ctx, stop := context.WithCancel(streamCtx)
				defer stop()
				err := awaitDelivery(ctx, streamCtx, func() error {
					cancel()
					// Returns only once every other goroutine in the
					// bubble blocks: after the caller saw ctx end.
					time.Sleep(time.Second)
					return sendErr
				})
				if !errors.Is(err, sendErr) || (sendErr == nil) != (err == nil) {
					t.Errorf("awaitDelivery = %v, want %v", err, sendErr)
				}
			})
		})
	}
}

// awaitDelivery sends nothing once ctx has ended, and, while the stream is
// still open, returns ctx's status as soon as ctx ends during a Send that
// still blocks, as await does: Remove or the VM's death ended the call,
// and the runner may never read again.
func TestAwaitDeliveryEndsWithItsContext(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		sent := false
		err := awaitDelivery(ctx, t.Context(), func() error { sent = true; return nil })
		wantCode(t, err, codes.Canceled)
		if sent {
			t.Error("awaitDelivery sent once its context had ended")
		}

		ctx, cancel = context.WithCancel(t.Context())
		release := make(chan struct{})
		returned := make(chan error, 1)
		go func() { returned <- awaitDelivery(ctx, t.Context(), func() error { <-release; return nil }) }()
		synctest.Wait()
		cancel()
		wantCode(t, <-returned, codes.Canceled)
		// As gRPC ends the stream once the handler returns.
		close(release)
	})
}
