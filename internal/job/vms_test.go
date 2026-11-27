// SPDX-License-Identifier: GPL-3.0-or-later

package job

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
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
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/testbin"
)

// vmsBound is how long a test waits for the job process to return.
const vmsBound = 20 * time.Second

// fakeVMs writes a shell script standing in for the job binary whose VM
// child does scripts[i] for VM i: it reads its spec and tells its VM by the
// console path testVMsSpec gives it. No test runs its own binary.
func fakeVMs(t *testing.T, scripts ...string) string {
	t.Helper()
	var body strings.Builder
	body.WriteString("#!/bin/sh\n[ \"$1\" = vm ] || exit 99\nspec=$(tr -d '\\000')\ncase \"$spec\" in\n")
	for i, s := range scripts {
		fmt.Fprintf(&body, "*/vm%d/console*)\n%s\n;;\n", i, s)
	}
	body.WriteString("esac\nexit 97\n")
	path := filepath.Join(t.TempDir(), "runner-microvm-job")
	testbin.Write(t, path, body.String())
	return path
}

// testVMsSpec returns a spec of n VMs, VM i named vm<i>, with its files in
// a directory vm<i>/ of its own, its scratch disk formatted by a fake
// mkfs.ext4 that does nothing.
func testVMsSpec(t *testing.T, jobBinary string, n int) *jobv1.Spec {
	t.Helper()
	spec := testJobSpec(t, jobBinary)
	base := t.TempDir()
	spec.Vms = nil
	for i := range n {
		dir := filepath.Join(base, "vm"+strconv.Itoa(i))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		spec.Vms = append(spec.Vms, &jobv1.VM{
			Name: "vm" + strconv.Itoa(i), Cpus: 1, MemoryMib: 512,
			GuestAddress: fmt.Sprintf("198.18.0.%d", 2+i), GuestMac: fmt.Sprintf("02:00:c6:12:00:%02x", 2+i),
			VmLog: filepath.Join(dir, "vm.log"), Console: filepath.Join(dir, "console"),
			AgentSocket: filepath.Join(dir, "agent.sock"), AgentPort: 1024, RootDir: filepath.Join(dir, "root"),
			ScratchDisk: filepath.Join(dir, "scratch.ext4"), ScratchSizeBytes: 1 << 30,
		})
	}
	return spec
}

// runBounded runs the job process on spec, as runJob does, failing t
// unless it returns within vmsBound.
func runBounded(t *testing.T, spec *jobv1.Spec) ([]*jobv1.Event, error) {
	t.Helper()
	return runEach(t, spec, func(*jobv1.Event) {})
}

// runEach is runBounded handing each event to each as it arrives, on t's
// goroutine, before it reads the next one.
func runEach(t *testing.T, spec *jobv1.Spec, each func(*jobv1.Event)) ([]*jobv1.Event, error) {
	t.Helper()
	var in bytes.Buffer
	if _, err := protodelim.MarshalTo(&in, spec); err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Should t end first, the job process's next send fails, which ends it.
	t.Cleanup(func() { _ = r.Close() })
	if err := r.SetReadDeadline(time.Now().Add(vmsBound)); err != nil {
		t.Fatal(err)
	}
	live, _ := livenessPipe(t)
	th := startThread()
	done := make(chan error, 1)
	go func() {
		err := run(t.Context(), &in, w, live, th, slog.New(slog.DiscardHandler), new(slog.LevelVar))
		th.stop()
		_ = w.Close()
		done <- err
	}()
	var events []*jobv1.Event
	br := bufio.NewReader(r)
	for {
		ev := &jobv1.Event{}
		err := protodelim.UnmarshalFrom(br, ev)
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("the job process still runs %v later", vmsBound)
		}
		if err != nil {
			t.Fatalf("reading events: %v", err)
		}
		events = append(events, ev)
		each(ev)
	}
	// The job process closes its end of the pipe once it has returned.
	return events, <-done
}

// pidFile is a file in a fresh directory a fake VM child writes its pid to.
func pidFile(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name+".pid")
}

// gone fails t unless the process whose pid is in path no longer exists: it
// was killed and reaped.
func gone(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("VM child %d still exists: %v", pid, err)
	}
}

// shellWaitFor is shell that waits, up to 10 s, until cond holds, and exits 98
// if it never does.
func shellWaitFor(cond string) string {
	return "i=0; until " + cond + "; do i=$((i+1)); [ $i -lt 1000 ] || exit 98; sleep 0.01; done\n"
}

// The job process accepts 1 to 253 VMs, what the job network's /24 gives
// addresses to; any other count is the plugin's bug, before the network
// starts.
func TestRunRefusesAVMCount(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 254} {
		spec := testVMsSpec(t, fakeVMs(t, "exit 0"), 1)
		vm := spec.Vms[0]
		spec.Vms = nil
		for range n {
			spec.Vms = append(spec.Vms, vm)
		}
		events, err := runJob(t, spec)
		want := fmt.Sprintf("%d VMs", n)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%d VMs: err = %v, want one naming %q", n, err, want)
		}
		if len(events) != 1 || !events[0].GetFailed().GetBug() || !strings.Contains(events[0].GetFailed().GetReason(), want) {
			t.Errorf("%d VMs: events = %v, want one Failed as the plugin's bug, naming %q", n, events, want)
		}
	}
}

// With two VMs the job process starts both, reports each one's events by
// its index, and runs on after VM 1 exits: VM 0, which exits only once the
// test has seen VM 1's VMExited, ends with its own exit code, not killed.
func TestRunKeepsRunningAfterAServiceExits(t *testing.T) {
	t.Parallel()
	release := filepath.Join(t.TempDir(), "release")
	bin := fakeVMs(t,
		shellWaitFor("[ -e "+release+" ]")+"echo vm0 >&2; exit 3",
		"echo vm1; exit 5",
	)
	vm0Exited := false
	events, err := runEach(t, testVMsSpec(t, bin, 2), func(ev *jobv1.Event) {
		switch x := ev.GetVmExited(); {
		case x == nil:
		case x.GetVm() == 0:
			vm0Exited = true
		case x.GetVm() == 1:
			if vm0Exited {
				t.Error("VM 0's VMExited came before VM 1's")
			}
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Error(err)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	events = afterNetworkUp(t, events)
	if len(events) != 4 {
		t.Fatalf("events = %v, want each VM's VMStarted and VMExited", events)
	}
	for i, ev := range events[:2] {
		if s := ev.GetVmStarted(); s == nil || s.GetVm() != uint32(i) || s.GetPid() <= 0 {
			t.Errorf("event %d = %v, want VM %d's VMStarted with its pid", i, ev, i)
		}
	}
	for i, want := range []struct {
		vm   uint32
		code int32
		log  string
	}{{1, 5, "vm1\n"}, {0, 3, "vm0\n"}} {
		x := events[2+i].GetVmExited()
		if x == nil || x.GetVm() != want.vm || x.GetExitCode() != want.code || x.GetLogTail() != want.log {
			t.Errorf("event %d = %v, want VM %d's VMExited with %d and log %q", 2+i, events[2+i], want.vm, want.code, want.log)
		}
	}
}

// The job's container exiting ends the job: the job process kills VM 1,
// reports its VMExited once it is reaped, and returns.
func TestRunKillsTheServicesWhenTheJobsContainerExits(t *testing.T) {
	t.Parallel()
	vm1 := pidFile(t, "vm1")
	bin := fakeVMs(t,
		shellWaitFor("[ -s "+vm1+" ]")+"exit 0",
		"echo $$ > "+vm1+"; exec sleep 1000",
	)
	events, err := runBounded(t, testVMsSpec(t, bin, 2))
	if err != nil {
		t.Fatal(err)
	}
	events = afterNetworkUp(t, events)
	if len(events) != 4 || events[0].GetVmStarted().GetVm() != 0 || events[1].GetVmStarted().GetVm() != 1 {
		t.Fatalf("events = %v, want both VMs' VMStarted, then their VMExited", events)
	}
	if x := events[2].GetVmExited(); x == nil || x.GetVm() != 0 || x.GetExitCode() != 0 {
		t.Errorf("third event = %v, want VM 0's VMExited with 0", events[2])
	}
	if x := events[3].GetVmExited(); x == nil || x.GetVm() != 1 || x.GetExitCode() != 128+9 {
		t.Errorf("fourth event = %v, want VM 1's VMExited with 137, killed", events[3])
	}
	gone(t, vm1)
}

// A VM that can't be prepared, here by a mount gone, fails the job naming
// it, before any VM child starts.
func TestRunPreparesEveryVMBeforeStartingAny(t *testing.T) {
	t.Parallel()
	started := filepath.Join(t.TempDir(), "started")
	spec := testVMsSpec(t, fakeVMs(t, "touch "+started, "touch "+started), 2)
	missing := filepath.Join(t.TempDir(), "missing")
	spec.Vms[1].Mounts = []*jobv1.Mount{{Source: missing, Tag: "mount0"}}
	events, err := runBounded(t, spec)
	prefix := "mount " + missing + ": does not exist"
	if err == nil || !strings.Contains(err.Error(), prefix) {
		t.Fatalf("err = %v, want one naming %q", err, prefix)
	}
	events = afterNetworkUp(t, events)
	if len(events) != 1 || !strings.HasPrefix(events[0].GetFailed().GetReason(), prefix) {
		t.Fatalf("events = %v, want one Failed beginning %q", events, prefix)
	}
	if _, err := os.Stat(started); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a VM child started: %v", err)
	}
}

// A VM child that can't start fails the job naming its VM, once the VM
// children already started are killed and reaped; their VMExited is never
// sent, so Failed is the plugin's first word on why.
func TestRunKillsTheStartedVMsWhenOneCantStart(t *testing.T) {
	t.Parallel()
	vm0 := pidFile(t, "vm0")
	spec := testVMsSpec(t, fakeVMs(t, "echo $$ > "+vm0+"; exec sleep 1000", "exit 0"), 2)
	spec.Vms[1].VmLog = filepath.Join(t.TempDir(), "no", "such", "dir", "vm.log")
	events, err := runBounded(t, spec)
	if err == nil || !strings.Contains(err.Error(), "VM vm1: VM log") {
		t.Fatalf("err = %v, want one naming VM vm1's VM log", err)
	}
	events = afterNetworkUp(t, events)
	if len(events) != 2 || events[0].GetVmStarted().GetVm() != 0 ||
		!strings.HasPrefix(events[1].GetFailed().GetReason(), "VM vm1: VM log") || events[1].GetFailed().GetBug() {
		t.Fatalf("events = %v, want VM 0's VMStarted, then Failed naming VM vm1", events)
	}
	// The pid file may not be written yet when the kill comes.
	if _, err := os.Stat(vm0); err == nil {
		gone(t, vm0)
	}
}

// failingWriter accepts n writes, then fails every one after.
type failingWriter struct {
	n   int
	buf bytes.Buffer
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.n == 0 {
		return 0, io.ErrClosedPipe
	}
	w.n--
	return w.buf.Write(p)
}

// A control channel nobody reads any more ends the job: once a send fails,
// the job process kills every VM child, waits for them and returns.
func TestRunKillsEveryVMWhenNobodyReads(t *testing.T) {
	t.Parallel()
	// The send that fails is VM 0's VMStarted, then VM 1's.
	for _, writes := range []int{1, 2} {
		t.Run(fmt.Sprintf("after %d events", writes), func(t *testing.T) {
			t.Parallel()
			pids := []string{pidFile(t, "vm0"), pidFile(t, "vm1")}
			spec := testVMsSpec(t, fakeVMs(t, "echo $$ > "+pids[0]+"; exec sleep 1000", "echo $$ > "+pids[1]+"; exec sleep 1000"), 2)
			var in bytes.Buffer
			if _, err := protodelim.MarshalTo(&in, spec); err != nil {
				t.Fatal(err)
			}
			live, _ := livenessPipe(t)
			th := startThread()
			defer th.stop()
			done := make(chan error, 1)
			go func() {
				done <- run(t.Context(), &in, &failingWriter{n: writes}, live, th, slog.New(slog.DiscardHandler), new(slog.LevelVar))
			}()
			select {
			case err := <-done:
				if !errors.Is(err, io.ErrClosedPipe) {
					t.Errorf("err = %v, want the control channel's", err)
				}
			case <-time.After(vmsBound):
				t.Fatalf("the job process still runs %v later", vmsBound)
			}
			for _, p := range pids {
				if _, err := os.Stat(p); err == nil {
					gone(t, p)
				}
			}
		})
	}
}
