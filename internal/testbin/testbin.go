// SPDX-License-Identifier: GPL-3.0-or-later

// Package testbin writes the executables tests run, such as a shell script
// standing in for the job binary. Only tests import it.
//
// A file a test writes itself and then executes can fail with ETXTBSY (text
// file busy): while the test process holds the file open for writing, a fork
// on another goroutine, for a parallel test, copies that fd into its child,
// which holds it until its own exec closes it (golang/go#22315). So the file
// is written by a child process, cp, that has exited before Write returns;
// no process then holds it open for writing, and an exec can't race one.
package testbin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// copyBound bounds the cp, which only copies a small file.
const copyBound = time.Minute

// cp is looked up before any test runs, so a test that sets PATH can still
// write.
var cp, errCp = exec.LookPath("cp")

// Write writes content to path, mode 0755, for the test to run at once.
func Write(t testing.TB, path, content string) {
	t.Helper()
	if errCp != nil {
		t.Fatalf("write %s: %v", path, errCp)
	}
	// The source is never executed, so a forked child holding it open
	// costs nothing.
	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), copyBound)
	defer cancel()
	cmd := exec.CommandContext(ctx, cp, "--", src, path) //nolint:gosec // G204: cp from PATH copies a test's own file to its own path.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write %s: cp: %v: %s", path, err, out)
	}
	if err := os.Chmod(path, 0o755); err != nil { //nolint:gosec // G302: the file is a program a test runs, maybe as another user.
		t.Fatal(err)
	}
}
