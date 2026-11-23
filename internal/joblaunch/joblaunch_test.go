// SPDX-License-Identifier: GPL-3.0-or-later

package joblaunch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protodelim"

	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
)

// The test process is a subreaper, as the plugin is, so a grandchild left
// by a fake job process is reparented here and reaped by the launcher.
func TestMain(m *testing.M) {
	if err := SetSubreaper(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if builtDir != "" {
		_ = os.RemoveAll(builtDir)
	}
	os.Exit(code)
}

// fakeJobBinary writes a shell script standing in for the job binary. It
// reads the job spec to its end, then runs script. No test runs its own
// binary.
func fakeJobBinary(t *testing.T, script string) *jobv1.Spec {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner-microvm-job")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ncat >/dev/null\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &jobv1.Spec{Job: "job-1", JobBinary: path, Vms: []*jobv1.VM{{Name: "echo"}}}
}

func newLauncher(t *testing.T) *Launcher {
	t.Helper()
	l, err := New(slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	return l
}

func launch(t *testing.T, spec *jobv1.Spec) (*Launcher, *Process, string) {
	t.Helper()
	l := newLauncher(t)
	logPath := filepath.Join(t.TempDir(), "job.log")
	p, err := l.Launch(t.Context(), spec, logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := l.Kill(ctx, p); err != nil {
			t.Errorf("Kill: %v", err)
		}
	})
	return l, p, logPath
}

func waitExited(t *testing.T, p *Process) {
	t.Helper()
	select {
	case <-p.Exited:
	case <-time.After(30 * time.Second):
		t.Fatal("Exited did not close")
	}
}

// alive reports whether pid is a live process or an unreaped zombie.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func readPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && bytes.HasSuffix(b, []byte("\n")) {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s was never written", path)
	return 0
}

func encode(t *testing.T, events ...*jobv1.Event) string {
	t.Helper()
	var b bytes.Buffer
	for _, ev := range events {
		if _, err := protodelim.MarshalTo(&b, ev); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "events")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func started(pid int32) *jobv1.Event {
	return &jobv1.Event{Event: &jobv1.Event_VmStarted{VmStarted: &jobv1.VMStarted{Pid: pid}}}
}

func TestEventsAndLog(t *testing.T) {
	t.Parallel()
	events := encode(t, started(1), &jobv1.Event{Event: &jobv1.Event_VmExited{VmExited: &jobv1.VMExited{ExitCode: 3}}})
	_, p, logPath := launch(t, fakeJobBinary(t, "echo to-the-log >&2; cat "+events))
	var got []*jobv1.Event
	for ev := range p.Events {
		got = append(got, ev)
	}
	waitExited(t, p)
	if len(got) != 2 || got[0].GetVmStarted().GetPid() != 1 || got[1].GetVmExited().GetExitCode() != 3 {
		t.Errorf("events = %v", got)
	}
	if b, err := os.ReadFile(logPath); err != nil || string(b) != "to-the-log\n" {
		t.Errorf("job process log = %q, %v", b, err)
	}
}

// Exited closes, though nobody reads Events and the job process sends more
// than the protocol allows.
func TestExitedWithoutReadingEvents(t *testing.T) {
	t.Parallel()
	var many []*jobv1.Event
	for i := range 50 {
		many = append(many, started(int32(i)))
	}
	_, p, _ := launch(t, fakeJobBinary(t, "cat "+encode(t, many...)))
	if c := cap(p.Events); c != 4 {
		t.Errorf("Events holds %d, want 4 for one VM", c)
	}
	waitExited(t, p)
}

// A process the job process leaves in its group, as a VM child, is killed
// and reaped before Exited closes.
func TestExitedReapsTheGroup(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	_, p, _ := launch(t, fakeJobBinary(t, "sleep 1000 & echo $! >"+pidFile+"; exit 0"))
	child := readPid(t, pidFile)
	waitExited(t, p)
	if alive(child) {
		t.Errorf("the job process's child %d outlived Exited", child)
	}
}

// Kill ends a running job process and its group, and returns only once all
// of it is reaped.
func TestKill(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	l, p, _ := launch(t, fakeJobBinary(t, "sleep 1000 & echo $! >"+pidFile+"; wait"))
	child := readPid(t, pidFile)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := l.Kill(ctx, p); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Exited:
	default:
		t.Fatal("Kill returned before Exited closed")
	}
	if alive(child) || alive(p.pid) {
		t.Error("a process of the group outlived Kill")
	}
	// Again, once it is gone: no error, and no signal to a reused group id.
	if err := l.Kill(ctx, p); err != nil {
		t.Fatal(err)
	}
}

// Kill gives up with its context's error, not the job process's life.
func TestKillHonoursItsContext(t *testing.T) {
	t.Parallel()
	l := newLauncher(t)
	p := &Process{Exited: make(chan struct{}), pid: 1 << 30, exited: true}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := l.Kill(ctx, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("Kill = %v, want context.Canceled", err)
	}
}

func TestLaunchFails(t *testing.T) {
	t.Parallel()
	l := newLauncher(t)
	spec := &jobv1.Spec{JobBinary: filepath.Join(t.TempDir(), "missing")}
	if _, err := l.Launch(t.Context(), spec, filepath.Join(t.TempDir(), "job.log")); err == nil {
		t.Error("launched a missing binary")
	}
	if _, err := l.Launch(t.Context(), fakeJobBinary(t, "exit 0"), filepath.Join(t.TempDir(), "no", "dir", "job.log")); err == nil {
		t.Error("launched without a log file")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := l.Launch(ctx, fakeJobBinary(t, "exit 0"), filepath.Join(t.TempDir(), "job.log")); err == nil {
		t.Error("launched with a cancelled context")
	}
}

// A job process that exits before reading its spec fails the launch, and
// leaves nothing behind.
func TestLaunchFailsWhenTheSpecCantBeWritten(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "runner-microvm-job")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec 0<&-\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A spec bigger than a pipe's buffer, so the write can't complete
	// before the job process is gone.
	spec := &jobv1.Spec{Job: strings.Repeat("x", 1<<20), JobBinary: path}
	l := newLauncher(t)
	if _, err := l.Launch(t.Context(), spec, filepath.Join(t.TempDir(), "job.log")); err == nil {
		t.Fatal("launched, though the spec was never read")
	}
}
