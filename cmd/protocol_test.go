// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"code.forgejo.org/forgejo/runner/v13/act/model"
	"code.forgejo.org/forgejo/runner/v13/act/runner"
	"github.com/sirupsen/logrus"
)

// The workflow both the harness and this test run, and what its step
// prints.
const (
	workflow = "../e2e/workflows/echo.yml"
	stepOut  = "hello from runner-microvm e2e"
)

// The profile the plugin under test defines besides the implicit default,
// and its shape.
const (
	bigProfile    = "big"
	bigCPUs       = 3
	bigMemoryMiB  = 1536
	bigScratchGiB = 4
)

// testProfiles is the profiles section of the plugin's config file.
var testProfiles = fmt.Sprintf("profiles:\n  %s:\n    cpus: %d\n    memory_mib: %d\n    scratch_gib: %d\n",
	bigProfile, bigCPUs, bigMemoryMiB, bigScratchGiB)

// TestEchoWorkflow is the protocol test: it runs e2e/workflows/echo.yml
// through forgejo-runner's own job executor (act/runner), which calls the
// real plugin through the runner's plugin client, and the plugin boots the
// image in e2e/image as a real VM. No Forgejo, no runner daemon. Its label
// names no profile, so its VM has the built-in shape.
func TestEchoWorkflow(t *testing.T) {
	log, err := runWorkflow(t, workflow, map[string]string{"microvm": readImage(t)})
	if err != nil {
		t.Fatalf("the workflow failed: %v", err)
	}
	if !strings.Contains(log, stepOut) {
		t.Fatalf("the job log does not show the step's output %q", stepOut)
	}
	wantShape(t, log, 2, 1024, 10)
}

// TestProfileWorkflow runs a workflow whose label names the big profile:
// its VM has that profile's CPU count, memory and scratch size (VM-4, VM-5).
// Then it changes the profile's CPU count in the plugin's config file and
// sends the plugin SIGHUP: the next job on the profile has the new count
// (CFG-7).
func TestProfileWorkflow(t *testing.T) {
	wf := writeWorkflow(t, "  big:\n    runs-on: microvm-big\n    steps:\n      - run: |\n          "+shapeStep+"\n")
	images := map[string]string{"microvm-big": readImage(t) + ";profile=" + bigProfile}
	p := startPlugin(t, buildPlugin(t))
	log, err := runWorkflowOn(t, p, wf, images)
	if err != nil {
		t.Fatalf("the workflow failed: %v", err)
	}
	wantShape(t, log, bigCPUs, bigMemoryMiB, bigScratchGiB)

	const reloadedCPUs = bigCPUs + 1
	p.writeConfig(t, fmt.Sprintf("profiles:\n  %s:\n    cpus: %d\n    memory_mib: %d\n    scratch_gib: %d\n",
		bigProfile, reloadedCPUs, bigMemoryMiB, bigScratchGiB))
	p.reload(t)
	log, err = runWorkflowOn(t, p, wf, images)
	if err != nil {
		t.Fatalf("the workflow failed after the reload: %v", err)
	}
	wantShape(t, log, reloadedCPUs, bigMemoryMiB, bigScratchGiB)
}

// shapeStep is the step, in e2e/workflows/echo.yml too, that prints the VM's
// CPU count, memory and scratch disk size, each computed by the step's
// shell, so the log's copy of the script never matches shapeLine.
const shapeStep = `echo "shape: cpus=$(nproc) memtotal_kib=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo) vda_sectors=$(cat /sys/block/vda/size)"`

var shapeLine = regexp.MustCompile(`shape: cpus=(\d+) memtotal_kib=(\d+) vda_sectors=(\d+)`)

// wantShape fails t unless log shows the shape step's line with cpus CPUs,
// memory within the kernel's overhead of memoryMiB, and a scratch disk of
// scratchGiB.
func wantShape(t *testing.T, log string, cpus, memoryMiB, scratchGiB uint64) {
	t.Helper()
	m := shapeLine.FindStringSubmatch(log)
	if m == nil {
		t.Fatal("the job log does not show the shape step's line")
	}
	got := make([]uint64, 3)
	for i := range got {
		n, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		got[i] = n
	}
	if got[0] != cpus {
		t.Errorf("nproc = %d, want %d", got[0], cpus)
	}
	// MemTotal is the memory less what the kernel reserves for itself:
	// within 15% below it, never above.
	if limit := memoryMiB << 10; got[1] > limit || got[1] < limit*85/100 {
		t.Errorf("MemTotal = %d KiB, want within 15%% below %d KiB", got[1], limit)
	}
	if want := scratchGiB << 30 / 512; got[2] != want {
		t.Errorf("/sys/block/vda/size = %d sectors, want %d", got[2], want)
	}
}

// runWorkflow runs the workflow file through act/runner against a fresh
// plugin, with each label in images mapped to its image, and returns the
// job log and the executor's error.
func runWorkflow(t *testing.T, workflow string, images map[string]string) (string, error) {
	t.Helper()
	return runWorkflowOn(t, startPlugin(t, buildPlugin(t)), workflow, images)
}

// runWorkflowOn is runWorkflow against the running plugin p.
func runWorkflowOn(t *testing.T, p *plugin, workflow string, images map[string]string) (string, error) {
	t.Helper()
	log := &syncBuffer{}
	run := planExecutor(t, p, workflow, images, log)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	err := run(ctx)
	t.Logf("job log:\n%s", log.String())
	return log.String(), err
}

// planExecutor returns act/runner's executor for the workflow file against
// the running plugin p, with each label in images mapped to its image,
// writing the job log to log. It may run in any goroutine.
func planExecutor(t *testing.T, p *plugin, workflow string, images map[string]string, log *syncBuffer) func(context.Context) error {
	t.Helper()
	planner, err := model.NewWorkflowPlanner(workflow, true, false)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.PlanEvent("workflow_dispatch")
	if err != nil {
		t.Fatal(err)
	}
	platforms := map[string]string{}
	for label, image := range images {
		platforms[label] = "runner-microvm://" + image
	}
	level := logrus.InfoLevel
	r, err := runner.New(&runner.Config{
		JobLoggerLevel:       &level,
		Workdir:              gitRepo(t),
		EventName:            "workflow_dispatch",
		LogOutput:            true,
		ContainerMaxLifetime: 10 * time.Minute,
		Platforms:            platforms,
		Plugins:              map[string]runner.PluginConfig{"runner-microvm": {Address: "unix://" + p.sock}},
	})
	if err != nil {
		t.Fatal(err)
	}
	executor := r.NewPlanExecutor(plan)
	return func(ctx context.Context) error {
		return executor(runner.WithJobLoggerFactory(ctx, jobLogger{log}))
	}
}

// readImage returns the image e2e/image pins.
func readImage(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../e2e/image")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// buildPlugin builds runner-microvm with its payload, as it ships, plus the
// given -ldflags, which only a test that needs a seam passes. `just
// test-kvm` builds the payload first.
func buildPlugin(t *testing.T, ldflags ...string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "runner-microvm")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	args := []string{"build", "-tags", "payload", "-trimpath", "-buildvcs=false"}
	if len(ldflags) > 0 {
		args = append(args, "-ldflags", strings.Join(ldflags, " "))
	}
	cmd := exec.CommandContext(ctx, "go", append(args, "-o", bin, module+"/cmd/runner-microvm")...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	cmd.WaitDelay = 10 * time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the plugin (run `just payload` first): %v\n%s", err, out)
	}
	return bin
}

// plugin is a running plugin under test.
type plugin struct {
	// sock is its listen socket.
	sock string
	// config is its config file, and dirs the config file's directory
	// keys, which every version of the file keeps.
	config, dirs string
	// state is its state directory.
	state string
	pid   int
	log   *syncBuffer
}

// writeConfig replaces p's config file with its directory keys and
// profiles, in one step, as an editor's save through a rename does.
func (p *plugin) writeConfig(t *testing.T, profiles string) {
	t.Helper()
	tmp := p.config + ".tmp"
	if err := os.WriteFile(tmp, []byte(p.dirs+profiles), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p.config); err != nil {
		t.Fatal(err)
	}
}

// reloaded is the plugin's log line for a successful reload.
var reloaded = regexp.MustCompile(`msg="reloaded the config file"`)

// reload sends p SIGHUP and waits until its log shows the reload.
func (p *plugin) reload(t *testing.T) {
	t.Helper()
	before := len(reloaded.FindAllString(p.log.String(), -1))
	if err := syscall.Kill(p.pid, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for len(reloaded.FindAllString(p.log.String(), -1)) == before {
		if time.Now().After(deadline) {
			t.Fatalf("the plugin did not reload on SIGHUP:\n%s", p.log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startPlugin runs the plugin's serve by path, with RUNNER_MICROVM_CONFIG
// naming a config file that puts its directories in a fresh directory and
// defines testProfiles: startPluginWith with testProfiles.
func startPlugin(t *testing.T, bin string) *plugin {
	t.Helper()
	return startPluginWith(t, bin, testProfiles)
}

// startPluginWith runs the plugin's serve by path, with
// RUNNER_MICROVM_CONFIG naming a config file that puts its directories in a
// fresh directory and holds profiles, the profiles section, and returns it
// once its listen socket exists. On
// cleanup it stops the plugin with SIGTERM, which removes every job; a
// plugin still running after that, or at the timeout, is killed with each
// job process's group (killWithJobs).
func startPluginWith(t *testing.T, bin, profiles string) *plugin {
	t.Helper()
	// Short, so the runner's client can connect to the socket by its path.
	base, err := os.MkdirTemp("", "rm-")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(base, "runtime", "runner-microvm.sock")
	cfg := filepath.Join(base, "config.yaml")
	dirs := fmt.Sprintf("listen: unix://%s\nstate_dir: %s\ncache_dir: %s\nruntime_dir: %s\n",
		sock, filepath.Join(base, "state"), filepath.Join(base, "cache"), filepath.Join(base, "runtime"))
	if err := os.WriteFile(cfg, []byte(dirs+profiles), 0o600); err != nil {
		_ = os.RemoveAll(base)
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	log := &syncBuffer{}
	cmd := exec.CommandContext(ctx, bin, "serve")
	cmd.Env = append(os.Environ(), "RUNNER_MICROVM_CONFIG="+cfg)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { killWithJobs(cmd.Process.Pid); return nil }
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	done := make(chan struct{})
	go func() { defer close(done); exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the plugin did not stop on SIGTERM")
		}
		cancel()
		<-done
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Logf("plugin log:\n%s", log.String())
		if err := removeAll(base); err != nil {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		if fi, err := os.Stat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return &plugin{sock: sock, config: cfg, dirs: dirs, state: filepath.Join(base, "state"), pid: cmd.Process.Pid, log: log}
		}
		select {
		case <-done:
			t.Fatalf("the plugin exited at startup: %v\n%s", <-exited, log.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no listen socket at %s\n%s", sock, log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// removeAll deletes dir, giving back the write bit an image's read-only
// directories took.
func removeAll(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// gitRepo returns a fresh, empty git repository, so act/runner's git
// lookups in its work directory don't fail noisily.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "init", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

// jobLogger gives act/runner a job logger writing to a buffer.
type jobLogger struct{ w *syncBuffer }

func (l jobLogger) WithJobLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(l.w)
	log.SetFormatter(&logrus.TextFormatter{DisableColors: true})
	return log
}

// syncBuffer is a buffer several goroutines may write.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
