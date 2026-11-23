// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// buildRunnerClient builds cmd/internal/runnerclient, which makes a job's
// calls through forgejo-runner's own plugin client, and returns its path.
// The runner's generated plugin protocol and the plugin's own register the
// same proto file, so the runner's client can't run in this test's process.
func buildRunnerClient(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "runnerclient")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-o", bin,
		"github.com/chrishoage/forgejo-runner-plugin-microvm/cmd/internal/runnerclient")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	cmd.WaitDelay = 10 * time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the runner's client: %v\n%s", err, out)
	}
	return bin
}

// serveByPath serves srv until the test ends, on a socket whose path fits
// connect's 107 bytes, since the runner's client dials it by path, and
// returns the path.
func serveByPath(t *testing.T, srv *Server) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "plugin.sock")
	lis, closeLis, err := listen(t.Context(), sock)
	if err != nil {
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
	})
	return sock
}

// runRunnerClient runs the runner's client bin by path, with args, in a
// process group of its own that is killed when it ends or after 3 minutes,
// and fails t unless it reports that every call succeeded.
func runRunnerClient(t *testing.T, bin string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if err != nil || out.String() != "ok\n" {
		t.Fatalf("the runner's client %q: %v\n%s", args, err, out.String())
	}
}

// A 20 MiB message from forgejo-runner's own plugin client, a Create
// carrying a 20 MiB backend option, is accepted: the plugin's limit is the
// client's own, 32 MiB.
func TestARunnerMessageOf20MiBIsAccepted(t *testing.T) {
	t.Parallel()
	bin := buildRunnerClient(t)
	sock := serveByPath(t, newTestServer(t, &fakeCache{}, &fakeLauncher{}))
	runRunnerClient(t, bin, "-option-bytes", strconv.Itoa(20<<20), "-label", testImage, sock)
}
