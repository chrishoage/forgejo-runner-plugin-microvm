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

// versionVar is the variable `just release-assets` sets the version in,
// with -ldflags -X.
const versionVar = module + "/internal/plugin.version"

// buildPluginWithoutPayload builds runner-microvm into dir without its
// payload, which neither check nor version needs, passing ldflags on to the
// linker, and returns its path.
func buildPluginWithoutPayload(t *testing.T, dir, ldflags string) string {
	t.Helper()
	bin := filepath.Join(dir, "runner-microvm")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-ldflags="+ldflags,
		"-o", bin, module+"/cmd/runner-microvm")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	build.WaitDelay = 10 * time.Second
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the plugin: %v\n%s", err, out)
	}
	return bin
}

// The built plugin's check exits 0 on a valid config file and 1 on an
// invalid one, printing the error; version prints dev, since the build
// doesn't set it; with no subcommand it prints its usage and exits 2.
func TestBuiltPluginCheck(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := buildPluginWithoutPayload(t, dir, "")
	valid := filepath.Join(dir, "valid.yaml")
	invalid := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(valid, []byte("state_dir: "+dir+"/state\nmax_memory_mib: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalid, []byte("profiles:\n  big:\n    cpus: 256\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		args       []string
		file       string
		code       int
		out, inErr string
	}{
		{[]string{"check"}, valid, 0, "config " + valid + ": valid\n", ""},
		{[]string{"check"}, invalid, 1, "", "config " + invalid + `: line 3: profile "big": cpus: must be from 1 to 255, not 256`},
		{[]string{"version"}, valid, 0, "dev\n", ""},
		{nil, valid, 2, "", "usage: runner-microvm <command>"},
	} {
		code, stdout, stderr := runBin(t, bin, tc.file, tc.args...)
		if code != tc.code || stdout != tc.out || !strings.Contains(stderr, tc.inErr) {
			t.Errorf("%q with %s: exit %d, stdout %q, stderr %q; want %d, %q and %q",
				tc.args, tc.file, code, stdout, stderr, tc.code, tc.out, tc.inErr)
		}
	}
}

// A build that sets the version variable with -ldflags -X, as `just
// release-assets` does, prints that version, so renaming the variable
// can't silently leave every release saying dev.
func TestBuiltPluginVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := buildPluginWithoutPayload(t, dir, "-X "+versionVar+"=1.2.3")
	code, stdout, stderr := runBin(t, bin, filepath.Join(dir, "missing.yaml"), "version")
	if code != 0 || stdout != "1.2.3\n" || stderr != "" {
		t.Errorf("exit %d, stdout %q, stderr %q; want 0 and \"1.2.3\\n\"", code, stdout, stderr)
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
