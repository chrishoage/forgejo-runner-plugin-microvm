// SPDX-License-Identifier: GPL-3.0-or-later

package job

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
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

// fakeMkfs writes a shell script standing in for mkfs.ext4, doing script.
func fakeMkfs(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mkfs.ext4")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// testJobSpec returns a spec of one VM whose scratch disk a fake mkfs.ext4
// formats, doing nothing.
func testJobSpec(t *testing.T, jobBinary string) *jobv1.Spec {
	t.Helper()
	dir := t.TempDir()
	return &jobv1.Spec{
		Job:       "job-1",
		JobBinary: jobBinary,
		MkfsExt4:  fakeMkfs(t, "exit 0"),
		Network:   &jobv1.Network{Subnet: "198.18.0.0/24", GatewayAddress: "198.18.0.1", GatewayMac: "02:00:c6:12:00:01", Mtu: 1500},
		Vms: []*jobv1.VM{{
			Name: "echo", Cpus: 2, MemoryMib: 1024, GuestAddress: "198.18.0.2", GuestMac: "02:00:c6:12:00:02",
			VmLog: filepath.Join(dir, "vm.log"), Console: filepath.Join(dir, "console"),
			AgentSocket: filepath.Join(dir, "agent.sock"), AgentPort: 1024, RootDir: filepath.Join(dir, "root"),
			ScratchDisk: filepath.Join(dir, "scratch.ext4"), ScratchSizeBytes: 1 << 30,
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

// afterNetworkUp returns events after the first, which must be NetworkUp:
// the job network is up before any VM starts.
func afterNetworkUp(t *testing.T, events []*jobv1.Event) []*jobv1.Event {
	t.Helper()
	if len(events) == 0 || events[0].GetNetworkUp() == nil {
		t.Fatalf("events = %v, want NetworkUp first", events)
	}
	return events[1:]
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
	events = afterNetworkUp(t, events)
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
	events = afterNetworkUp(t, events)
	if len(events) != 2 || events[1].GetVmExited().GetExitCode() != 128+9 {
		t.Fatalf("events = %v, want VMExited with 137", events)
	}
}

// The scratch disk is a fresh, sparse ext4 file of the VM's scratch size,
// formatted with the host's mkfs.ext4, before the VM child starts (VM-5).
func TestRunCreatesTheScratchDisk(t *testing.T) {
	t.Parallel()
	mkfs, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		t.Fatalf("the dev shell carries mkfs.ext4: %v", err)
	}
	spec := testJobSpec(t, fakeJobBinary(t, "exit 0"))
	spec.MkfsExt4 = mkfs
	v := spec.GetVms()[0]
	v.ScratchSizeBytes = 10 << 30
	if _, err := run(t, spec); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(v.GetScratchDisk())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 10<<30 || fi.Mode().Perm() != 0o600 {
		t.Errorf("scratch disk: size %d, mode %v; want 10 GiB, 0600", fi.Size(), fi.Mode().Perm())
	}
	// Sparse: only the filesystem's metadata is allocated.
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Blocks*512 > 64<<20 {
		t.Errorf("scratch disk allocates %d bytes, want it sparse", st.Blocks*512)
	}
	// ext4's superblock starts at 1024, its magic 0xEF53 at offset 56.
	f, err := os.Open(v.GetScratchDisk())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	magic := make([]byte, 2)
	if _, err := f.ReadAt(magic, 1024+56); err != nil {
		t.Fatal(err)
	}
	if magic[0] != 0x53 || magic[1] != 0xef {
		t.Errorf("scratch disk magic = %x, want ext4's 53ef", magic)
	}
}

// A scratch disk that can't be made fails the job, naming it, and leaves no
// file behind; a file already there is never reused.
func TestRunFailsOnTheScratchDisk(t *testing.T) {
	t.Parallel()
	failing := testJobSpec(t, fakeJobBinary(t, "exit 0"))
	failing.MkfsExt4 = fakeMkfs(t, "echo 'bad superblock' >&2; exit 1")
	noMkfs := testJobSpec(t, fakeJobBinary(t, "exit 0"))
	noMkfs.MkfsExt4 = filepath.Join(t.TempDir(), "mkfs.ext4")
	stale := testJobSpec(t, fakeJobBinary(t, "exit 0"))
	if err := os.WriteFile(stale.GetVms()[0].GetScratchDisk(), []byte("old job"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		spec      *jobv1.Spec
		want      string
		leftAlone bool
	}{
		"mkfs fails":   {spec: failing, want: "bad superblock"},
		"no mkfs":      {spec: noMkfs, want: "mkfs.ext4"},
		"a stale file": {spec: stale, want: "exists", leftAlone: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			disk := tt.spec.GetVms()[0].GetScratchDisk()
			events, err := run(t, tt.spec)
			if err == nil || !strings.Contains(err.Error(), "scratch disk "+disk) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one naming the scratch disk and %q", err, tt.want)
			}
			events = afterNetworkUp(t, events)
			if len(events) != 1 || !strings.Contains(events[0].GetFailed().GetReason(), "scratch disk") {
				t.Fatalf("events = %v, want one Failed naming the scratch disk", events)
			}
			_, err = os.Stat(disk)
			if tt.leftAlone && err != nil {
				t.Errorf("the file already there was deleted: %v", err)
			}
			if !tt.leftAlone && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a failed scratch disk is left behind: %v", err)
			}
		})
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
		spec      *jobv1.Spec
		want      string
		networkUp bool
	}{
		"no VM child":   {missing, "start the VM child", true},
		"two VMs":       {twoVMs, "2 VMs", false},
		"no VM log":     {noLog, "VM log", true},
		"no VMs at all": {&jobv1.Spec{}, "0 VMs", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			events, err := run(t, tt.spec)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one naming %q", err, tt.want)
			}
			if tt.networkUp {
				events = afterNetworkUp(t, events)
			}
			if len(events) != 1 || !strings.Contains(events[0].GetFailed().GetReason(), tt.want) {
				t.Fatalf("events = %v, want one Failed naming %q", events, tt.want)
			}
		})
	}
}

// The VM child gets its VM's end of the frame socket as fd 4, and nothing
// as fd 3, the liveness pipe's.
func TestRunPassesTheFrameSocket(t *testing.T) {
	t.Parallel()
	events, err := run(t, testJobSpec(t, fakeJobBinary(t, "[ -S /proc/self/fd/4 ] && [ ! -e /proc/self/fd/3 ] || exit 7")))
	if err != nil {
		t.Fatal(err)
	}
	events = afterNetworkUp(t, events)
	if len(events) != 2 || events[1].GetVmExited().GetExitCode() != 0 {
		t.Fatalf("events = %v, want VMExited with 0", events)
	}
}

// A job network that can't start fails the job with a reason that begins
// `network: `, before any VM child starts.
func TestRunFailsOnTheNetwork(t *testing.T) {
	t.Parallel()
	started := filepath.Join(t.TempDir(), "started")
	for name, change := range map[string]func(*jobv1.Spec){
		"no network":               func(s *jobv1.Spec) { s.Network = nil },
		"a gateway off the subnet": func(s *jobv1.Spec) { s.Network.GatewayAddress = "10.0.0.1" },
		"a bad VM MAC":             func(s *jobv1.Spec) { s.Vms[0].GuestMac = "nope" },
		"the gateway's address":    func(s *jobv1.Spec) { s.Vms[0].GuestAddress = "198.18.0.1" },
	} {
		spec := testJobSpec(t, fakeJobBinary(t, "touch "+started))
		change(spec)
		events, err := run(t, spec)
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if len(events) != 1 || !strings.HasPrefix(events[0].GetFailed().GetReason(), "network: ") {
			t.Errorf("%s: events = %v, want one Failed beginning with network: ", name, events)
		}
	}
	if _, err := os.Stat(started); !errors.Is(err, os.ErrNotExist) {
		t.Error("a VM child started without its network")
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
		events = afterNetworkUp(t, events)
		if len(events) != 2 || events[1].GetVmExited().GetExitCode() != 3 {
			t.Errorf("events = %v, want VMExited with 3", events)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the job process still waits on a VM child that exited")
	}
}
