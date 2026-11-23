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

// TestEchoWorkflow is the protocol test: it runs e2e/workflows/echo.yml
// through forgejo-runner's own job executor (act/runner), which calls the
// real plugin through the runner's plugin client, and the plugin boots the
// image in e2e/image as a real VM. No Forgejo, no runner daemon.
func TestEchoWorkflow(t *testing.T) {
	log, err := runWorkflow(t, workflow, map[string]string{"microvm": readImage(t)})
	if err != nil {
		t.Fatalf("the workflow failed: %v", err)
	}
	if !strings.Contains(log, stepOut) {
		t.Fatalf("the job log does not show the step's output %q", stepOut)
	}
}

// runWorkflow runs the workflow file through act/runner against a fresh
// plugin, with each label in images mapped to its image, and returns the
// job log and the executor's error.
func runWorkflow(t *testing.T, workflow string, images map[string]string) (string, error) {
	t.Helper()
	sock := startPlugin(t, buildPlugin(t))
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
		Plugins:              map[string]runner.PluginConfig{"runner-microvm": {Address: "unix://" + sock}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	log := &syncBuffer{}
	ctx = runner.WithJobLoggerFactory(ctx, jobLogger{log})
	err = r.NewPlanExecutor(plan)(ctx)
	t.Logf("job log:\n%s", log.String())
	return log.String(), err
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

// buildPlugin builds runner-microvm with its payload, as it ships. `just
// test-kvm` builds the payload first.
func buildPlugin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "runner-microvm")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-tags", "payload", "-trimpath", "-buildvcs=false",
		"-o", bin, module+"/cmd/runner-microvm")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	cmd.WaitDelay = 10 * time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the plugin (run `just payload` first): %v\n%s", err, out)
	}
	return bin
}

// startPlugin runs the plugin's serve by path, with RUNNER_MICROVM_CONFIG
// naming a config file that puts its directories in a fresh directory, and
// returns its listen socket once it exists. On
// cleanup it stops the plugin with SIGTERM, which removes every job; a
// plugin still running after that, or at the timeout, is killed with each
// job process's group (killWithJobs).
func startPlugin(t *testing.T, bin string) string {
	t.Helper()
	// Short, so the runner's client can connect to the socket by its path.
	base, err := os.MkdirTemp("", "rm-")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(base, "runtime", "runner-microvm.sock")
	cfg := filepath.Join(base, "config.yaml")
	content := fmt.Sprintf("listen: unix://%s\nstate_dir: %s\ncache_dir: %s\nruntime_dir: %s\n",
		sock, filepath.Join(base, "state"), filepath.Join(base, "cache"), filepath.Join(base, "runtime"))
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		_ = os.RemoveAll(base)
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	var log syncBuffer
	cmd := exec.CommandContext(ctx, bin, "serve")
	cmd.Env = append(os.Environ(), "RUNNER_MICROVM_CONFIG="+cfg)
	cmd.Stdout, cmd.Stderr = &log, &log
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
			return sock
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
