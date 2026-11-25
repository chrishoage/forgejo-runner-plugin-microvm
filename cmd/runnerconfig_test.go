// SPDX-License-Identifier: GPL-3.0-or-later

package cmd_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	// runnerExample is the operator guide's forgejo-runner config file
	// (docs/operating.md), from this package's directory.
	runnerExample = "../docs/examples/forgejo-runner.yaml"
	// runnerModule is forgejo-runner's main package, a tool in go.mod, so
	// it builds at the version the protocol test runs against.
	runnerModule = "code.forgejo.org/forgejo/runner/v13"
	// declarePath is the runner's first call to Forgejo, made only once its
	// config file is loaded and checked.
	declarePath = "/api/actions/runner.v1.RunnerService/Declare"
)

// forgejo-runner's own config loader is internal to its module, so the test
// runs the runner: `daemon`, connected to a Forgejo that fails every call,
// loads the example as it would in production, checks that every label's
// scheme is a plugin the file configures, and then calls Declare, which
// fails. Reaching Declare is the proof the file loaded. The runner is built
// once, for both subtests: a build takes tens of seconds.
func TestRunnerExample(t *testing.T) {
	t.Parallel()
	bin := buildRunner(t, t.TempDir())

	t.Run("loads", func(t *testing.T) {
		t.Parallel()
		code, out, calls := runRunnerDaemon(t, bin, t.TempDir(), runnerExample)
		if code == 0 || strings.Contains(out, "invalid configuration") || !slices.Contains(calls, declarePath) {
			t.Fatalf("forgejo-runner with %s: exit %d, calls %q, want a non-zero exit at Declare (%s) once the file loaded; its output:\n%s",
				runnerExample, code, calls, declarePath, out)
		}
	})

	// The same run fails at loading, before any call, when no plugin is
	// configured under the labels' scheme: the runner's loader checks the
	// labels against plugins, so the subtest above proves they match.
	t.Run("needs its plugin", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		data, err := os.ReadFile(runnerExample)
		if err != nil {
			t.Fatal(err)
		}
		const key = "\n  runner-microvm:\n"
		if strings.Count(string(data), key) != 1 {
			t.Fatalf("%s has no single plugins key %q", runnerExample, strings.TrimSpace(key))
		}
		renamed := filepath.Join(dir, "renamed.yaml")
		if err := os.WriteFile(renamed, []byte(strings.Replace(string(data), key, "\n  other-plugin:\n", 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, calls := runRunnerDaemon(t, bin, dir, renamed)
		if code == 0 || !strings.Contains(out, `uses unknown scheme "runner-microvm"`) || len(calls) != 0 {
			t.Fatalf("forgejo-runner with the plugins key renamed: exit %d, calls %q, want a non-zero exit naming the unknown scheme and no call; its output:\n%s",
				code, calls, out)
		}
	})
}

// buildRunner builds forgejo-runner into dir and returns its path.
func buildRunner(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "forgejo-runner")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, runnerModule)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	build.WaitDelay = 10 * time.Second
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build forgejo-runner: %v\n%s", err, out)
	}
	return bin
}

// runRunnerDaemon runs `bin --config file daemon`, connected through its flags
// to a Forgejo that answers every call with 503, with dir as its working
// directory and HOME. It runs by path with a timeout, in a process group of
// its own that is killed when it ends, and returns its exit code, its
// output and the paths it called.
func runRunnerDaemon(t *testing.T, bin, dir, file string) (int, string, []string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	forgejo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		http.Error(w, "the test's Forgejo answers nothing", http.StatusServiceUnavailable)
	}))
	defer forgejo.Close()
	// The runner runs in dir, so a relative path would name a file there.
	file, err := filepath.Abs(file)
	if err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("0123456789abcdef0123456789abcdef01234567\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--config", file, "daemon",
		"--url", forgejo.URL+"/", "--uuid", "c9e50be9-a7c3-4aee-ba35-624c4ff8c519", "--token-url", "file:"+token)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	err = cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("run %s: %v", bin, err)
	}
	mu.Lock()
	defer mu.Unlock()
	return cmd.ProcessState.ExitCode(), out.String(), slices.Clone(calls)
}
