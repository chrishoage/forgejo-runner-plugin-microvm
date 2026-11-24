// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"code.forgejo.org/forgejo/runner/v13/act/model"
	"code.forgejo.org/forgejo/runner/v13/act/runner"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
)

// The workflows both the harness and this test run, and what each one's
// first step prints.
const (
	workflows = "../e2e/workflows"
	stepOut   = "hello from runner-microvm e2e"
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

// TestWorkflows is the protocol test: it runs every workflow in
// e2e/workflows/, one subtest each, through forgejo-runner's own job
// executor (act/runner), which calls the real plugin through the runner's
// plugin client, and the plugin boots the image in e2e/image as real VMs.
// No Forgejo, no runner daemon. The test maps the harness's microvm and
// microvm-big labels; a workflow with a job on any other label, such as the
// Network epic's on microvm-node, which need the harness's Forgejo or the
// internet, or the Mounts and disks epic's on microvm-storage, which need
// the harness's fixtures, is skipped, naming the label. Each workflow must succeed
// and show its first step's output, and workflowChecks adds a workflow's
// own checks.
func TestWorkflows(t *testing.T) {
	t.Parallel()
	image := readImage(t)
	labels := map[string]string{"microvm": image, "microvm-big": image + ";profile=" + bigProfile}
	files, err := filepath.Glob(filepath.Join(workflows, "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no workflows in %s", workflows)
	}
	p := startPlugin(t, buildPlugin(t))
	// lib_dir names the dev shell's directory, and the libraries load from
	// there.
	wantLibsLoaded(t, p, devLibDir(t))
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".yml")
		t.Run(name, func(t *testing.T) {
			if other := unmappedLabels(t, file, labels); len(other) > 0 {
				t.Skipf("runs on %s, which only the harness has", strings.Join(other, ", "))
			}
			log, err := runWorkflowOn(t, p, file, labels)
			if err != nil {
				t.Fatalf("the workflow failed: %v", err)
			}
			if !strings.Contains(log, stepOut) {
				t.Fatalf("the job log does not show the first step's output %q", stepOut)
			}
			if check := workflowChecks[name]; check != nil {
				check(t, log, image)
			}
		})
	}
}

// workflowInputs are the workflow_dispatch inputs the test dispatches a
// workflow with, by the workflow's file name; any other input takes the
// workflow's own default, as the harness's dispatch does. hold's default
// holds its VM for the minute the harness's reload scenario needs, which
// this test doesn't.
var workflowInputs = map[string]map[string]any{
	"hold": {"seconds": "1"},
}

// workflowChecks are the checks of a workflow's job log beyond its success
// and its first step's output, by the workflow's file name.
var workflowChecks = map[string]func(t *testing.T, log, image string){
	// Its label names no profile, so its VM has the built-in shape. The job
	// log shows Start's three progress lines, in order (PROTO-9).
	"echo": func(t *testing.T, log, image string) {
		wantShape(t, log, 2, 1024, 10)
		// The job log quotes each line, its newline too.
		const at = `\[runner-microvm \d+\.\d\ds\] `
		progress := regexp.MustCompile(`(?s)` + at + `image ` + regexp.QuoteMeta(image) + ` ready \(sha256:[0-9a-f]{64}, \d+ layers\) in \d+\.\d\ds.*` +
			at + `VM \S+ \(2 CPUs, 1024 MiB\) ready in \d+\.\d\ds.*` + at + `started in \d+\.\d\ds`)
		if !progress.MatchString(log) {
			t.Errorf("the job log does not show Start's three progress lines, in order:\n%s", log)
		}
	},
	// The runner fetched the actions from the job's repository, not from
	// the work directory, and ran the JavaScript one on the image's node.
	"actions": func(t *testing.T, log, _ string) {
		for _, want := range []*regexp.Regexp{
			regexp.MustCompile(`git clone 'http://127\.0\.0\.1:\d+/` + repoOwner + `/actions'`),
			regexp.MustCompile(`js action: hello, microvm, from node v24\.`),
			regexp.MustCompile(`composite action: hello, microvm`),
			regexp.MustCompile(`js action post: the state came back`),
		} {
			if !want.MatchString(log) {
				t.Errorf("the job log does not match %s", want)
			}
		}
	},
	"big":  func(t *testing.T, log, _ string) { wantShape(t, log, bigCPUs, bigMemoryMiB, bigScratchGiB) },
	"hold": func(t *testing.T, log, _ string) { wantShape(t, log, bigCPUs, bigMemoryMiB, bigScratchGiB) },
}

// unmappedLabels returns the labels the workflow file's jobs run on that
// labels lacks, sorted.
func unmappedLabels(t *testing.T, file string, labels map[string]string) []string {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	wf, err := model.ReadWorkflow(f, false)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	var other []string
	for _, job := range wf.Jobs {
		for _, label := range job.RunsOn() {
			if _, ok := labels[label]; !ok && !slices.Contains(other, label) {
				other = append(other, label)
			}
		}
	}
	slices.Sort(other)
	return other
}

// TestProfileWorkflow runs a workflow whose label names the big profile:
// its VM has that profile's CPU count, memory and scratch size (VM-4, VM-5).
// Then it changes the profile's CPU count in the plugin's config file and
// sends the plugin SIGHUP: the next job on the profile has the new count
// (CFG-7).
func TestProfileWorkflow(t *testing.T) {
	t.Parallel()
	profileAndReload(t, startPlugin(t, buildPlugin(t)))
}

// TestWorkflowsAsRoot is the protocol test's root case (VM-9): with serve
// run as root under `sudo -n`, the echo workflow, then TestProfileWorkflow's
// profile check and reload, on one plugin. As root, libkrun's virtiofs
// server switches to the guest's uid and gid for each request, for every
// VM's boot root and layer share, which the unprivileged run never does.
// Without passwordless sudo it skips, saying so, unless CI is set.
func TestWorkflowsAsRoot(t *testing.T) {
	t.Parallel()
	needRoot(t)
	image := readImage(t)
	p := startRootPluginWith(t, buildPlugin(t), testProfiles)
	// sudo's child is the plugin.
	procs := descendants(p.pid)
	if len(procs) == 0 {
		t.Fatal("sudo has no child")
	}
	if uid := strings.Fields(procStatus(t, procs[0].pid)["Uid"]); len(uid) < 2 || uid[1] != "0" {
		t.Fatalf("the plugin, %d (%s), runs as uid %v, not root", procs[0].pid, procs[0].comm, uid)
	}
	t.Logf("the plugin, %d (%s), runs as root", procs[0].pid, procs[0].comm)
	t.Run("echo", func(t *testing.T) {
		log, err := runWorkflowOn(t, p, filepath.Join(workflows, "echo.yml"), map[string]string{"microvm": image})
		if err != nil {
			t.Fatalf("the workflow failed: %v", err)
		}
		if !strings.Contains(log, stepOut) {
			t.Fatalf("the job log does not show the first step's output %q", stepOut)
		}
		workflowChecks["echo"](t, log, image)
	})
	t.Run("profile", func(t *testing.T) { profileAndReload(t, p) })
}

// profileAndReload runs a workflow on the big profile on p and checks its
// VM's shape, then raises the profile's CPU count in p's config file,
// reloads p, and checks that the next job on the profile has the new count.
func profileAndReload(t *testing.T, p *plugin) {
	t.Helper()
	wf := writeWorkflow(t, "  big:\n    runs-on: microvm-big\n    steps:\n      - run: |\n          "+shapeStep+"\n")
	images := map[string]string{"microvm-big": readImage(t) + ";profile=" + bigProfile}
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

// shapeStep is the step, in e2e/workflows/echo.yml, big.yml and hold.yml
// too, that prints the VM's CPU count, memory and scratch disk size, each
// computed by the step's shell, so the log's copy of the script never
// matches shapeLine.
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
	t.Cleanup(func() { logIfFailed(t, "job log", log) })
	err := run(ctx)
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
	name := strings.TrimSuffix(filepath.Base(workflow), ".yml")
	repo := jobRepo(t, name)
	event := map[string]any{}
	if inputs := workflowInputs[name]; inputs != nil {
		event["inputs"] = inputs
	}
	// The same event, in the expression context and in event.json.
	eventJSON, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	level := logrus.InfoLevel
	r, err := runner.New(&runner.Config{
		JobLoggerLevel:       &level,
		EventJSON:            string(eventJSON),
		Workdir:              repo.dir,
		EventName:            "workflow_dispatch",
		LogOutput:            true,
		ContainerMaxLifetime: 10 * time.Minute,
		Platforms:            platforms,
		Plugins:              map[string]runner.PluginConfig{"runner-microvm": {Address: "unix://" + p.sock}},
		// As the runner daemon passes a task's: the job's repository at its
		// commit, on the instance at GitHubInstance, which github.server_url
		// becomes.
		GitHubInstance: repo.serverURL,
		PresetGitHubContext: &model.GithubContext{
			Event:           event,
			EventName:       "workflow_dispatch",
			RunID:           "1",
			RunNumber:       "1",
			RunAttempt:      "1",
			Actor:           repoOwner,
			Repository:      repo.name,
			RepositoryOwner: repoOwner,
			Sha:             repo.sha,
			Ref:             "refs/heads/main",
			RefName:         "main",
			RefType:         "branch",
		},
		ActionCacheDir: t.TempDir(),
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
// given -ldflags, which only a test that needs a seam passes, and returns
// its path. It builds once per set of -ldflags (goBuild). `just test-kvm`
// builds the payload first.
func buildPlugin(t *testing.T, ldflags ...string) string {
	t.Helper()
	args := []string{"-tags", "payload"}
	if len(ldflags) > 0 {
		args = append(args, "-ldflags", strings.Join(ldflags, " "))
	}
	bin, err := goBuild(module+"/cmd/runner-microvm", args...)
	if err != nil {
		t.Fatalf("build the plugin (run `just payload` first): %v", err)
	}
	return bin
}

// built holds the binaries goBuild built, each in a directory of its own
// under dir, which TestMain deletes.
var built struct {
	mu   sync.Mutex
	dir  string
	bins map[string]func() (string, error)
}

func TestMain(m *testing.M) {
	code := m.Run()
	if built.dir != "" {
		_ = os.RemoveAll(built.dir)
	}
	os.Exit(code)
}

// goBuild builds the package pkg, static, with the extra `go build` args,
// and returns the binary's path. Each package and args is built once per
// test binary, and every test asking for it gets the same file, so no test
// may change it; a failed build fails every test asking for it, with its
// output. It may run in any goroutine.
func goBuild(pkg string, args ...string) (string, error) {
	key := strings.Join(append([]string{pkg}, args...), "\x00")
	built.mu.Lock()
	if built.bins == nil {
		built.bins = map[string]func() (string, error){}
	}
	build, ok := built.bins[key]
	if !ok {
		n := len(built.bins)
		build = sync.OnceValues(func() (string, error) {
			built.mu.Lock()
			if built.dir == "" {
				dir, err := os.MkdirTemp("", "cmd-built-")
				if err != nil {
					built.mu.Unlock()
					return "", err
				}
				built.dir = dir
			}
			dir := filepath.Join(built.dir, strconv.Itoa(n))
			built.mu.Unlock()
			if err := os.Mkdir(dir, 0o755); err != nil {
				return "", err
			}
			bin := filepath.Join(dir, filepath.Base(pkg))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			argv := append(append([]string{"build", "-trimpath", "-buildvcs=false"}, args...), "-o", bin, pkg)
			cmd := exec.CommandContext(ctx, "go", argv...)
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			cmd.WaitDelay = 10 * time.Second
			if out, err := cmd.CombinedOutput(); err != nil {
				return "", fmt.Errorf("go %s: %w\n%s", strings.Join(argv, " "), err, out)
			}
			return bin, nil
		})
		built.bins[key] = build
	}
	built.mu.Unlock()
	return build()
}

// plugin is a running plugin under test.
type plugin struct {
	// sock is its listen socket.
	sock string
	// config is its config file, and dirs the config file's directory
	// keys and other plugin-level keys, which every version of the file
	// keeps.
	config, dirs string
	// state is its state directory.
	state string
	pid   int
	log   *syncBuffer
	// base holds its config file and directories, bin is its binary, and
	// setup its config file's settings and profiles and its environment,
	// for restart.
	base, bin string
	setup     pluginSetup
	// exited closes once its process has exited; the process is reaped
	// only at cleanup, so exitStatus can read its zombie's status.
	exited <-chan struct{}
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

// devLibDir is the dev shell's directory of libkrun and libkrunfw, which
// only the tests and `just check-libs` read: the plugin reads its lib_dir
// key, and no variable but RUNNER_MICROVM_CONFIG.
func devLibDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("RUNNER_MICROVM_LIBDIR")
	if dir == "" {
		t.Fatal("RUNNER_MICROVM_LIBDIR is not set: run the tests inside the dev shell")
	}
	return dir
}

// startPluginWith runs the plugin's serve by path, with
// RUNNER_MICROVM_CONFIG naming a config file that puts its directories in a
// fresh directory, sets lib_dir to the dev shell's libraries (devLibDir),
// and holds profiles, the profiles section, and returns it once its listen
// socket exists: startPluginSetup with that setup.
func startPluginWith(t *testing.T, bin, profiles string) *plugin {
	t.Helper()
	return startPluginSetup(t, bin, libDirSetup(t, profiles))
}

// libDirSetup is the setup of a plugin with lib_dir set to the dev shell's
// libraries (devLibDir) and profiles, the profiles section.
func libDirSetup(t *testing.T, profiles string) pluginSetup {
	t.Helper()
	return pluginSetup{settings: "lib_dir: " + devLibDir(t) + "\n", profiles: profiles}
}

// pluginSetup is the config file and environment a test's plugin starts
// with.
type pluginSetup struct {
	// settings are the plugin-level keys besides the directories, and
	// profiles the profiles section; every version of the file keeps
	// settings.
	settings, profiles string
	// env is added to the test's environment.
	env []string
}

// startPluginSetup runs the plugin's serve by path, as spawnPlugin does,
// and returns it once its listen socket exists.
func startPluginSetup(t *testing.T, bin string, setup pluginSetup) *plugin {
	t.Helper()
	p := spawnPlugin(t, bin, setup, false)
	p.waitForSocket(t, false)
	return p
}

// startRootPluginWith is startPluginWith with serve run as root, under
// `sudo -n`, which needRoot checks first. Its environment is only what the
// plugin reads: PATH, for mkfs.ext4, HOME and RUNNER_MICROVM_CONFIG. The
// test can connect to its socket, and on cleanup the plugin, its jobs and
// its directories go through sudo too.
func startRootPluginWith(t *testing.T, bin, profiles string) *plugin {
	t.Helper()
	needRoot(t)
	p := spawnPlugin(t, bin, libDirSetup(t, profiles), true)
	p.waitForSocket(t, true)
	return p
}

// needRoot skips t, saying why, when `sudo -n` can't run a command as
// root. With CI set it fails t instead, so CI never skips a root test.
func needRoot(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sudo", "-n", "true").CombinedOutput()
	if err == nil {
		return
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("CI is set, and `sudo -n true` failed: %v\n%s", err, out)
	}
	t.Skipf("SKIPPED: this test runs the plugin as root, and `sudo -n true` failed: %v\n%s", err, out)
}

// asRoot runs argv as root with `sudo -n`, and returns its error and
// output.
func asRoot(argv ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sudo", append([]string{"-n"}, argv...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sudo %v: %w\n%s", argv, err, out)
	}
	return nil
}

// killAsRoot kills, as root, the process group pid leads and every process
// below pid, each with its process group, a job process leading one of its
// own.
func killAsRoot(pid int) {
	args := []string{"kill", "-KILL", "--", "-" + strconv.Itoa(pid)}
	for _, q := range descendants(pid) {
		if pgid, err := syscall.Getpgid(q.pid); err == nil {
			args = append(args, "-"+strconv.Itoa(pgid))
		}
		args = append(args, strconv.Itoa(q.pid))
	}
	// Some of them may be gone already, which kill reports.
	_ = asRoot(args...)
}

// spawnPlugin runs the plugin's serve by path, as root when root, with
// RUNNER_MICROVM_CONFIG naming a config file that puts its directories in a
// fresh directory and holds setup's settings and profiles, and setup's env,
// and returns at once. On cleanup it stops the plugin with SIGTERM, which
// drains it: the cleanups registered after this one, which run before it,
// have removed every job, so it exits at once. A plugin still running 30 s
// later, or at the timeout, is killed with each job process's group
// (killWithJobs).
//
// Its cleanup kills only processes it can name for sure: it never reaps
// the plugin's process (or sudo's) before it is done killing. It waits for
// the exit with waitid's WNOWAIT, which leaves a zombie, so neither the pid
// nor the process group the command leads can be reused meanwhile. A
// plugin still running after SIGTERM is killed with its descendants' groups
// while it is alive and they are still its own. Once it has exited, its
// group, which a root plugin shares with sudo, is killed too, which reaches
// a root plugin that outlived sudo; only then is it reaped.
func spawnPlugin(t *testing.T, bin string, setup pluginSetup, root bool) *plugin {
	t.Helper()
	// Short, so the runner's client can connect to the socket by its path.
	base, err := os.MkdirTemp("", "rm-")
	if err != nil {
		t.Fatal(err)
	}
	return spawnPluginIn(t, base, bin, setup, root)
}

// restart runs the plugin p ran again, unprivileged, on the same config
// file and directories, once p's own process has died: spawnPluginIn on
// p's base, returning once its listen socket exists. The plugin binds its
// socket over the one p left.
func (p *plugin) restart(t *testing.T) *plugin {
	t.Helper()
	q := spawnPluginIn(t, p.base, p.bin, p.setup, false)
	q.waitForSocket(t, false)
	return q
}

// spawnPluginIn is spawnPlugin with its config file and directories in
// base, which its cleanup deletes.
func spawnPluginIn(t *testing.T, base, bin string, setup pluginSetup, root bool) *plugin {
	t.Helper()
	remove := func() error { return removeAll(base) }
	kill := killWithJobs
	killGroup := func(pgid int) { _ = syscall.Kill(-pgid, syscall.SIGKILL) }
	if root {
		remove = func() error { return asRoot("rm", "-rf", "--", base) }
		kill = killAsRoot
		// The group may be empty already, which kill reports.
		killGroup = func(pgid int) { _ = asRoot("kill", "-KILL", "--", "-"+strconv.Itoa(pgid)) }
	}
	runtime := filepath.Join(base, "runtime")
	sock := filepath.Join(runtime, "runner-microvm.sock")
	cfg := filepath.Join(base, "config.yaml")
	dirs := fmt.Sprintf("listen: unix://%s\nstate_dir: %s\ncache_dir: %s\nruntime_dir: %s\n",
		sock, filepath.Join(base, "state"), filepath.Join(base, "cache"), runtime) + setup.settings
	if err := os.WriteFile(cfg, []byte(dirs+setup.profiles), 0o600); err != nil {
		_ = os.RemoveAll(base)
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	log := &syncBuffer{}
	cmd := exec.CommandContext(ctx, bin, "serve")
	cmd.Env = append(append(os.Environ(), setup.env...), "RUNNER_MICROVM_CONFIG="+cfg)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if root {
		// The test's own runtime directory, so it can reach the socket
		// in it.
		if err := os.Mkdir(runtime, 0o755); err != nil {
			cancel()
			_ = os.RemoveAll(base)
			t.Fatal(err)
		}
		argv := append([]string{"-n", "env", "-i", "PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, setup.env...)
		cmd = exec.CommandContext(ctx, "sudo", append(argv, "RUNNER_MICROVM_CONFIG="+cfg, bin, "serve")...)
		// A session of its own has no terminal, so sudo starts no pty
		// and runs the plugin in its own process group, the one sudo
		// leads, rather than in a new session.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	cmd.Stdout, cmd.Stderr = log, log
	// reaping is set, under mu, just before the process is reaped, and
	// every kill by pid checks it under mu first.
	var mu sync.Mutex
	reaping := false
	killAlive := func() {
		mu.Lock()
		defer mu.Unlock()
		if !reaping {
			kill(cmd.Process.Pid)
		}
	}
	cmd.Cancel = func() error { killAlive(); return nil }
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		_ = remove()
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		var info unix.Siginfo
		for {
			err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
			if !errors.Is(err, unix.EINTR) {
				return
			}
		}
	}()
	t.Cleanup(func() {
		// sudo passes SIGTERM on to the plugin.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(30 * time.Second):
			t.Error("the plugin did not stop on SIGTERM")
			killAlive()
			<-exited
		}
		// A zombie still holds its group's id.
		killGroup(pid)
		mu.Lock()
		reaping = true
		mu.Unlock()
		if err := cmd.Wait(); err != nil {
			t.Logf("the plugin exited: %v", err)
		}
		cancel()
		if err := remove(); err != nil {
			t.Error(err)
		}
		logIfFailed(t, "plugin log", log)
	})
	return &plugin{
		sock: sock, config: cfg, dirs: dirs, state: filepath.Join(base, "state"), pid: pid, log: log,
		base: base, bin: bin, setup: setup, exited: exited,
	}
}

// waitForSocket waits until the plugin p listens on its socket, failing t
// if p exits first or 30 s pass. A root plugin's socket is made 0666 first.
//
// A socket a dead plugin left may be at p.sock, which p binds over, and p's
// can get the inode number of the one deleted, so the wait is for health to
// answer SERVING, which only a live plugin does. A root plugin's base is
// fresh: the first socket there is its own.
func (p *plugin) waitForSocket(t *testing.T, root bool) {
	t.Helper()
	chmodded := !root
	deadline := time.Now().Add(30 * time.Second)
	for {
		if fi, err := os.Lstat(p.sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			if !chmodded {
				// It is root's, mode 0660 (internal/plugin's listen),
				// and the test isn't root.
				if err := asRoot("chmod", "0666", p.sock); err != nil {
					t.Fatal(err)
				}
				chmodded = true
			}
			if serving(p.sock) {
				break
			}
		}
		select {
		case <-p.exited:
			t.Fatalf("the plugin exited at startup (%v):\n%s", p.exitStatus(t), p.log.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no plugin serving at %s\n%s", p.sock, p.log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// serving reports whether the plugin listening at sock answers health's
// Check with SERVING within a second. Each call dials afresh, so no
// client's reconnect backoff delays the next.
func serving(sock string) bool {
	conn, err := grpclib.NewClient("unix://"+sock, grpclib.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := healthv1.NewHealthClient(conn).Check(ctx, &healthv1.HealthCheckRequest{})
	return err == nil && resp.GetStatus() == healthv1.HealthCheckResponse_SERVING
}

// logMatches reports whether the plugin p's log matches re within 10 s. p's
// stderr reaches p.log through os/exec's copying goroutine, so a line the
// plugin logged before a call returned can arrive after it.
func (p *plugin) logMatches(re *regexp.Regexp) bool {
	deadline := time.Now().Add(10 * time.Second)
	for !re.MatchString(p.log.String()) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// exitStatus returns the status p's process exited with, once p.exited has
// closed: its zombie's exit_code, field 52 of /proc/<pid>/stat.
func (p *plugin) exitStatus(t *testing.T) syscall.WaitStatus {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(p.pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	// The fields after comm, which may hold spaces and parentheses,
	// start after its last ')', with field 3.
	s := string(b)
	rest := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(rest) < 50 {
		t.Fatalf("/proc/%d/stat has no exit_code: %q", p.pid, s)
	}
	code, err := strconv.Atoi(rest[49])
	if err != nil {
		t.Fatal(err)
	}
	return syscall.WaitStatus(code)
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

// actionsDir is the actions the workflows use, which the job's repository
// holds at actions/.
const actionsDir = "../e2e/actions"

// repoOwner owns each job's repository, as the harness's admin user does.
const repoOwner = "e2e"

// repository is a job's repository, served over HTTP on loopback.
type repository struct {
	// dir is its work tree, act/runner's work directory.
	dir string
	// serverURL is the HTTP server's root, and name the repository's path
	// under it, owner/name, so serverURL/name is its clone URL.
	serverURL, name string
	// sha is its one commit.
	sha string
}

// jobRepo returns a fresh git repository named for the workflow, as the
// harness's run creates one on Forgejo: one commit on main holding
// e2e/actions/ at actions/. It is served over HTTP on loopback by git
// http-backend, so the runner fetches an action from it as it does from
// Forgejo, with no network, and the repository is act/runner's work
// directory too, whose git lookups then find a commit.
func jobRepo(t *testing.T, workflow string) repository {
	t.Helper()
	top := t.TempDir()
	name := repoOwner + "/" + workflow
	dir := filepath.Join(top, filepath.FromSlash(name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// CopyFS keeps the executable bits, which git records.
	if err := os.CopyFS(filepath.Join(dir, "actions"), os.DirFS(actionsDir)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	gitCmd := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "user.name=e2e", "-c", "user.email=e2e@example.invalid"}, args...)...)
		out, err := cmd.Output()
		if err != nil {
			var stderr []byte
			if ee, ok := errors.AsType[*exec.ExitError](err); ok {
				stderr = ee.Stderr
			}
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr)
		}
		return strings.TrimSpace(string(out))
	}
	gitCmd("init", "-q", "-b", "main")
	gitCmd("add", "-A")
	gitCmd("commit", "-q", "--no-gpg-sign", "-m", "test: the actions")
	sha := gitCmd("rev-parse", "HEAD")
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&cgi.Handler{
		Path: git,
		Args: []string{"http-backend"},
		// Fetching a commit by its id, as the runner fetches an action at a
		// sha, asks for an object no ref advertises.
		Env: []string{
			"GIT_PROJECT_ROOT=" + top, "GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=uploadpack.allowAnySHA1InWant", "GIT_CONFIG_VALUE_0=true",
		},
		InheritEnv: []string{"PATH", "HOME"},
	})
	t.Cleanup(srv.Close)
	return repository{dir: dir, serverURL: srv.URL, name: name, sha: sha}
}

// jobLogger gives act/runner a job logger writing to a buffer.
type jobLogger struct{ w *syncBuffer }

func (l jobLogger) WithJobLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(l.w)
	log.SetFormatter(&logrus.TextFormatter{DisableColors: true})
	return log
}

// logIfFailed logs the named log, all of it, if t has failed by now. A
// passing test's logs stay out of -v's output, which would otherwise bury
// each test's result under every job and plugin log. Call it from a
// cleanup, after the cleanup's own checks.
func logIfFailed(t *testing.T, name string, log *syncBuffer) {
	t.Helper()
	if t.Failed() {
		t.Logf("%s:\n%s", name, log.String())
	}
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
