// SPDX-License-Identifier: GPL-3.0-or-later

package cmd_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The built plugin's check exits 0 on a valid config file and 1 on an
// invalid one, printing the error; with no subcommand it prints its usage
// and exits 2. check needs no payload, so the plugin is built without one.
func TestBuiltPluginCheck(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := filepath.Join(dir, "runner-microvm")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-o", bin, module+"/cmd/runner-microvm")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	build.WaitDelay = 10 * time.Second
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the plugin: %v\n%s", err, out)
	}
	valid := filepath.Join(dir, "valid.yaml")
	invalid := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(valid, []byte("state_dir: "+dir+"/state\nmax_memory_mib: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalid, []byte("profiles:\n  big:\n    cpus: 8\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		args       []string
		file       string
		code       int
		out, inErr string
	}{
		{[]string{"check"}, valid, 0, "config " + valid + ": valid\n", ""},
		{[]string{"check"}, invalid, 1, "", "config " + invalid + `: line 3: profile "big": cpus: unknown key`},
		{nil, valid, 2, "", "usage: runner-microvm <command>"},
	} {
		code, stdout, stderr := runBin(t, bin, tc.file, tc.args...)
		if code != tc.code || stdout != tc.out || !strings.Contains(stderr, tc.inErr) {
			t.Errorf("%q with %s: exit %d, stdout %q, stderr %q; want %d, %q and %q",
				tc.args, tc.file, code, stdout, stderr, tc.code, tc.out, tc.inErr)
		}
	}
}

// runBin runs bin by path with RUNNER_MICROVM_CONFIG=file and a timeout, in
// a process group of its own that is killed when it ends, and returns its
// exit code, stdout and stderr.
func runBin(t *testing.T, bin, file string, args ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "RUNNER_MICROVM_CONFIG="+file)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("run %s: %v", bin, err)
	}
	return cmd.ProcessState.ExitCode(), stdout.String(), stderr.String()
}
