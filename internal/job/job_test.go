// SPDX-License-Identifier: GPL-3.0-or-later

package job

import (
	"bufio"
	"bytes"
	"errors"
	"io"
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

// fakeJobBinary writes a shell script standing in for the job binary: run
// with `vm`, it is the VM child, doing script. No test runs its own binary.
func fakeJobBinary(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner-microvm-job")
	body := "#!/bin/sh\n[ \"$1\" = vm ] || exit 99\ncat >/dev/null\n" + script + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testJobSpec(t *testing.T, jobBinary string) *jobv1.Spec {
	t.Helper()
	dir := t.TempDir()
	return &jobv1.Spec{
		Job:       "job-1",
		JobBinary: jobBinary,
		Vms: []*jobv1.VM{{
			Name: "echo", Cpus: 2, MemoryMib: 1024,
			VmLog: filepath.Join(dir, "vm.log"), Console: filepath.Join(dir, "console"),
			AgentSocket: filepath.Join(dir, "agent.sock"), AgentPort: 1024, RootDir: filepath.Join(dir, "root"),
		}},
	}
}

// run runs the job process on spec and returns its error and events.
func run(t *testing.T, spec *jobv1.Spec) ([]*jobv1.Event, error) {
	t.Helper()
	var in, out bytes.Buffer
	if _, err := protodelim.MarshalTo(&in, spec); err != nil {
		t.Fatal(err)
	}
	err := Run(t.Context(), &in, &out, slog.New(slog.DiscardHandler))
	var events []*jobv1.Event
	r := bufio.NewReader(&out)
	for {
		ev := &jobv1.Event{}
		if e := protodelim.UnmarshalFrom(r, ev); errors.Is(e, io.EOF) {
			break
		} else if e != nil {
			t.Fatalf("reading events: %v", e)
		}
		events = append(events, ev)
	}
	return events, err
}

func TestRunReportsTheVMsExit(t *testing.T) {
	t.Parallel()
	spec := testJobSpec(t, fakeJobBinary(t, "echo booting; exit 3"))
	console := strings.Repeat("x", 500) + "kernel panic"
	if err := os.WriteFile(spec.GetVms()[0].GetConsole(), []byte(console), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := run(t, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %v, want VMStarted and VMExited", events)
	}
	if s := events[0].GetVmStarted(); s == nil || s.GetVm() != 0 || s.GetPid() <= 0 {
		t.Errorf("first event = %v, want VM 0's VMStarted with its pid", events[0])
	}
	x := events[1].GetVmExited()
	if x == nil || x.GetVm() != 0 || x.GetExitCode() != 3 {
		t.Fatalf("second event = %v, want VM 0's VMExited with exit code 3", events[1])
	}
	if x.GetLogTail() != "booting\n" {
		t.Errorf("log tail = %q", x.GetLogTail())
	}
	if want := console[len(console)-300:]; x.GetConsoleTail() != want {
		t.Errorf("console tail = %q, want the last 300 bytes", x.GetConsoleTail())
	}
}

func TestRunReportsASignal(t *testing.T) {
	t.Parallel()
	events, err := run(t, testJobSpec(t, fakeJobBinary(t, "kill -9 $$")))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].GetVmExited().GetExitCode() != 128+9 {
		t.Fatalf("events = %v, want VMExited with 137", events)
	}
}

func TestRunFails(t *testing.T) {
	t.Parallel()
	missing := testJobSpec(t, filepath.Join(t.TempDir(), "no-such-binary"))
	twoVMs := testJobSpec(t, fakeJobBinary(t, "exit 0"))
	twoVMs.Vms = append(twoVMs.Vms, twoVMs.Vms[0])
	noLog := testJobSpec(t, fakeJobBinary(t, "exit 0"))
	noLog.Vms[0].VmLog = filepath.Join(t.TempDir(), "no", "such", "dir", "vm.log")
	for name, tt := range map[string]struct {
		spec *jobv1.Spec
		want string
	}{
		"no VM child":   {missing, "start the VM child"},
		"two VMs":       {twoVMs, "2 VMs"},
		"no VM log":     {noLog, "VM log"},
		"no VMs at all": {&jobv1.Spec{}, "0 VMs"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			events, err := run(t, tt.spec)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one naming %q", err, tt.want)
			}
			if len(events) != 1 || !strings.Contains(events[0].GetFailed().GetReason(), tt.want) {
				t.Fatalf("events = %v, want one Failed naming %q", events, tt.want)
			}
		})
	}
}

func TestRunRefusesAGarbledSpec(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := Run(t.Context(), strings.NewReader("\xff\xff\xff\xff\xff"), &out, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("a garbled spec ran")
	}
	ev := &jobv1.Event{}
	if err := protodelim.UnmarshalFrom(bufio.NewReader(&out), ev); err != nil || ev.GetFailed() == nil {
		t.Fatalf("event = %v, %v; want Failed", ev, err)
	}
}

// A VM child that never reads its spec, and leaves a process holding its
// stdin, can't keep the job process waiting once it exits: the spec, larger
// than a pipe holds, comes from a file, not through a pipe.
func TestRunDoesNotWaitOnTheSpecsReader(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	bin := filepath.Join(t.TempDir(), "runner-microvm-job")
	// With job control off, sh gives a background command /dev/null as
	// stdin unless it is redirected explicitly.
	script := "#!/bin/sh\nexec 3<&0\nsleep 1000 <&3 3<&- &\necho $! >" + pidFile + "\nexit 3\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	spec := testJobSpec(t, bin)
	spec.LibDir = strings.Repeat("l", 256<<10)
	done := make(chan []*jobv1.Event, 1)
	go func() {
		events, _ := run(t, spec)
		done <- events
	}()
	select {
	case events := <-done:
		if len(events) != 2 || events[1].GetVmExited().GetExitCode() != 3 {
			t.Errorf("events = %v, want VMExited with 3", events)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the job process still waits on a VM child that exited")
	}
}
