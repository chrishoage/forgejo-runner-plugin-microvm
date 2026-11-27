// SPDX-License-Identifier: GPL-3.0-or-later

package joblaunch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/testbin"
)

// dieBound is how soon a job process must be gone once the plugin is.
const dieBound = 5 * time.Second

// pluginGone is the job process's log line when its liveness pipe ends.
const pluginGone = "the plugin is gone"

// builtDir holds the job binary the tests build; TestMain deletes it.
var builtDir string

// realJobBinary builds the job binary once and returns its path. These
// tests need the real job process's side of the liveness pipe, and no test
// runs its own binary.
var realJobBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "joblaunch-")
	if err != nil {
		return "", err
	}
	builtDir = dir
	bin := filepath.Join(dir, "runner-microvm-job")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-o", bin,
		"github.com/chrishoage/forgejo-runner-plugin-microvm/cmd/runner-microvm-job")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	cmd.WaitDelay = 10 * time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", &buildError{err: err, out: string(out)}
	}
	return bin, nil
})

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return "build the job binary: " + e.err.Error() + "\n" + e.out }

// stuckInMkfs returns a spec for the real job binary whose mkfs.ext4 writes
// its pid to the returned file and never exits, so the job process waits
// on it until it is killed.
func stuckInMkfs(t *testing.T) (*jobv1.Spec, string) {
	t.Helper()
	bin, err := realJobBinary()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "mkfs.pid")
	mkfs := filepath.Join(dir, "mkfs.ext4")
	testbin.Write(t, mkfs, "#!/bin/sh\necho $$ >"+pidFile+"\nexec sleep 1000\n")
	return &jobv1.Spec{
		Job:       "job-1",
		JobBinary: bin,
		MkfsExt4:  mkfs,
		LogLevel:  "info",
		Network:   &jobv1.Network{Subnet: "198.18.0.0/24", GatewayAddress: "198.18.0.1", GatewayMac: "02:00:c6:12:00:01", Mtu: 1500},
		Vms: []*jobv1.VM{{
			Name: "echo", Cpus: 1, MemoryMib: 512, GuestAddress: "198.18.0.2", GuestMac: "02:00:c6:12:00:02",
			VmLog: filepath.Join(dir, "vm.log"), Console: filepath.Join(dir, "console"),
			AgentSocket: filepath.Join(dir, "agent.sock"), AgentPort: 1024, RootDir: filepath.Join(dir, "root"),
			ScratchDisk: filepath.Join(dir, "scratch.ext4"), ScratchSizeBytes: 1 << 20,
		}},
	}, pidFile
}

// gone waits up to dieBound for p to exit, failing the test if it doesn't.
func gone(t *testing.T, p *Process, mkfs int) {
	t.Helper()
	start := time.Now()
	select {
	case <-p.Exited:
		t.Logf("the job process was gone %v later", time.Since(start).Round(time.Millisecond))
	case <-time.After(dieBound):
		t.Fatalf("the job process outlived the plugin by %v", dieBound)
	}
	if alive(mkfs) {
		t.Errorf("the job process's mkfs %d outlived it", mkfs)
	}
}

// A job process exits promptly once the liveness pipe's write end closes,
// as it does when the plugin dies, and takes its group with it.
func TestJobProcessDiesWithTheLivenessPipe(t *testing.T) {
	t.Parallel()
	spec, pidFile := stuckInMkfs(t)
	l, p, logPath := launch(t, spec)
	mkfs := readPid(t, pidFile)
	if err := l.liveW.Close(); err != nil {
		t.Fatal(err)
	}
	gone(t, p, mkfs)
	if b, _ := os.ReadFile(logPath); !strings.Contains(string(b), pluginGone) {
		t.Errorf("the job process's log does not say the plugin is gone:\n%s", b)
	}
}

// A job process is killed when the thread that forked it exits, though the
// liveness pipe is still open: Pdeathsig, the backstop for a job process too
// stuck to read the pipe.
func TestJobProcessDiesWithItsThread(t *testing.T) {
	t.Parallel()
	spec, pidFile := stuckInMkfs(t)
	l, p, logPath := launch(t, spec)
	mkfs := readPid(t, pidFile)
	if l.thread.tid == os.Getpid() {
		t.Fatal("the launcher forks on the main thread, which never exits")
	}
	l.thread.stop()
	gone(t, p, mkfs)
	if b, _ := os.ReadFile(logPath); strings.Contains(string(b), pluginGone) {
		t.Errorf("the liveness pipe ended, not only the thread:\n%s", b)
	}
	// A launcher whose thread is gone starts nothing.
	if _, err := l.Launch(t.Context(), spec, logPath); err == nil {
		t.Error("launched after the thread exited")
	}
}

// Close ends a job process still running as the plugin's death would, closes
// both ends of the liveness pipe and stops the forking thread, and a closed
// launcher launches nothing.
func TestCloseReleasesTheLauncher(t *testing.T) {
	t.Parallel()
	spec, pidFile := stuckInMkfs(t)
	l, p, logPath := launch(t, spec)
	mkfs := readPid(t, pidFile)
	l.Close()
	gone(t, p, mkfs)
	for _, f := range []*os.File{l.liveR, l.liveW} {
		if err := f.Close(); !errors.Is(err, os.ErrClosed) {
			t.Errorf("%s was left open: closing it again returned %v", f.Name(), err)
		}
	}
	select {
	case <-l.thread.done:
	default:
		t.Error("the forking thread still runs")
	}
	if _, err := l.Launch(t.Context(), spec, logPath); err == nil {
		t.Error("launched after Close")
	}
	l.Close()
}
