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
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protodelim"

	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
	vmv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/job/internal/gen/runnermicrovm/vm/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/logging"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/testbin"
)

// fakeJobBinary writes a shell script standing in for the job binary: run
// with `vm`, it is the VM child, doing script. No test runs its own binary.
func fakeJobBinary(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner-microvm-job")
	body := "#!/bin/sh\n[ \"$1\" = vm ] || exit 99\ncat >/dev/null\n" + script + "\n"
	testbin.Write(t, path, body)
	return path
}

// fakeMkfs writes a shell script standing in for mkfs.ext4, doing script.
func fakeMkfs(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mkfs.ext4")
	testbin.Write(t, path, "#!/bin/sh\n"+script+"\n")
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
		LogLevel:  "info",
		Network:   &jobv1.Network{Subnet: "198.18.0.0/24", GatewayAddress: "198.18.0.1", GatewayMac: "02:00:c6:12:00:01", Mtu: 1500},
		Vms: []*jobv1.VM{{
			Name: "echo", Cpus: 2, MemoryMib: 1024, GuestAddress: "198.18.0.2", GuestMac: "02:00:c6:12:00:02",
			VmLog: filepath.Join(dir, "vm.log"), Console: filepath.Join(dir, "console"),
			AgentSocket: filepath.Join(dir, "agent.sock"), AgentPort: 1024, RootDir: filepath.Join(dir, "root"),
			ScratchDisk: filepath.Join(dir, "scratch.ext4"), ScratchSizeBytes: 1 << 30,
		}},
	}
}

// livenessPipe returns a pipe standing in for the plugin's liveness pipe;
// both ends are closed on cleanup.
func livenessPipe(t *testing.T) (r, w *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	return r, w
}

// run runs the job process on spec and returns its error and events.
func runJob(t *testing.T, spec *jobv1.Spec) ([]*jobv1.Event, error) {
	t.Helper()
	live, _ := livenessPipe(t)
	th := startThread()
	defer th.stop()
	return runOn(t, spec, live, th)
}

// runOn runs the job process on spec, with live as its liveness pipe,
// forking on th, and returns its error and events.
func runOn(t *testing.T, spec *jobv1.Spec, live *os.File, th *thread) ([]*jobv1.Event, error) {
	t.Helper()
	var in, out bytes.Buffer
	if _, err := protodelim.MarshalTo(&in, spec); err != nil {
		t.Fatal(err)
	}
	err := run(t.Context(), &in, &out, live, th, slog.New(slog.DiscardHandler), new(slog.LevelVar))
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

// The job process log follows the spec's log level, set before the job
// process logs anything else.
func TestRunLogsAtTheSpecsLevel(t *testing.T) {
	t.Parallel()
	for level, debug := range map[string]bool{"debug": true, "info": false, "warn": false} {
		spec := testJobSpec(t, fakeJobBinary(t, "exit 0"))
		spec.LogLevel = level
		var in, out, log bytes.Buffer
		if _, err := protodelim.MarshalTo(&in, spec); err != nil {
			t.Fatal(err)
		}
		live, _ := livenessPipe(t)
		var v slog.LevelVar
		if err := Run(t.Context(), &in, &out, live, logging.New(&log, "test", &v), &v); err != nil {
			t.Fatal(err)
		}
		got := log.String()
		if strings.Contains(got, `level=DEBUG msg="read the job spec"`) != debug {
			t.Errorf("%s: the debug line logged: %t, want %t:\n%s", level, !debug, debug, got)
		}
		if strings.Contains(got, `level=INFO msg="VM exited"`) != (level != "warn") {
			t.Errorf("%s: the info line:\n%s", level, got)
		}
	}
}

// Each VM child's spec carries the job spec's library directory, libkrun
// log level and log level.
func TestRunPassesTheLibrariesOn(t *testing.T) {
	t.Parallel()
	got := filepath.Join(t.TempDir(), "vm-spec")
	bin := filepath.Join(t.TempDir(), "runner-microvm-job")
	testbin.Write(t, bin, "#!/bin/sh\n[ \"$1\" = vm ] || exit 99\ncat >"+got+"\n")
	spec := testJobSpec(t, bin)
	spec.LibDir, spec.LibkrunLogLevel, spec.LogLevel = "/opt/libkrun", "trace", "debug"
	if _, err := runJob(t, spec); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(got)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	child := &vmv1.Spec{}
	if err := protodelim.UnmarshalFrom(bufio.NewReader(f), child); err != nil {
		t.Fatal(err)
	}
	if child.GetLibDir() != "/opt/libkrun" || child.GetLibkrunLogLevel() != "trace" || child.GetLogLevel() != "debug" {
		t.Errorf("VM child spec: lib_dir %q, libkrun_log_level %q, log_level %q; want /opt/libkrun, trace and debug",
			child.GetLibDir(), child.GetLibkrunLogLevel(), child.GetLogLevel())
	}
}

func TestRunReportsTheVMsExit(t *testing.T) {
	t.Parallel()
	spec := testJobSpec(t, fakeJobBinary(t, "echo booting; exit 3"))
	// Longer than the tail, and cut in the middle of a character.
	console := strings.Repeat("é", 2600) + "kernel panic!"
	if err := os.WriteFile(spec.GetVms()[0].GetConsole(), []byte(console), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := runJob(t, spec)
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
	// The last 4 KiB, less the cut character's stray byte: a proto
	// string must be valid UTF-8.
	if want := console[len(console)-4095:]; x.GetConsoleTail() != want {
		t.Errorf("console tail = %q, want the last 4 KiB without the cut character", x.GetConsoleTail())
	}
}

func TestRunReportsASignal(t *testing.T) {
	t.Parallel()
	events, err := runJob(t, testJobSpec(t, fakeJobBinary(t, "kill -9 $$")))
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
	if _, err := runJob(t, spec); err != nil {
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
			events, err := runJob(t, tt.spec)
			if err == nil || !strings.Contains(err.Error(), "scratch disk: "+disk) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one naming the scratch disk and %q", err, tt.want)
			}
			events = afterNetworkUp(t, events)
			// The cause comes first, as Start's error shows it.
			if len(events) != 1 || !strings.HasPrefix(events[0].GetFailed().GetReason(), "scratch disk: "+disk+": ") {
				t.Fatalf("events = %v, want one Failed beginning with the scratch disk", events)
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

// The VM child's spec carries each mount's source, tag and read-write flag,
// in the job spec's order (STO-1, STO-2).
func TestRunPassesTheMounts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	saved := filepath.Join(dir, "vm-child-spec")
	bin := filepath.Join(dir, "runner-microvm-job")
	testbin.Write(t, bin, "#!/bin/sh\n[ \"$1\" = vm ] || exit 99\ncat > "+saved+"\n")
	tools, data := filepath.Join(dir, "tools"), filepath.Join(dir, "data")
	for _, d := range []string{tools, data} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	spec := testJobSpec(t, bin)
	spec.Vms[0].Mounts = []*jobv1.Mount{{Source: tools, Tag: "mount0"}, {Source: data, Tag: "mount1"}}
	if _, err := runJob(t, spec); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(saved)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	child := &vmv1.Spec{}
	if err := protodelim.UnmarshalFrom(bufio.NewReader(f), child); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range child.GetMounts() {
		got = append(got, fmt.Sprintf("%s %s rw=%v", m.GetSource(), m.GetTag(), m.GetReadWrite()))
	}
	if want := []string{tools + " mount0 rw=false", data + " mount1 rw=false"}; !slices.Equal(got, want) {
		t.Errorf("the VM child's mounts are %q, want %q", got, want)
	}
}

// A mount source that is gone, or is no longer a directory, by Start fails
// the job before the scratch disk is made and the VM child starts, naming
// the source (PROTO-10).
func TestRunFailsOnAMountSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		source, want string
	}{
		"missing":         {filepath.Join(dir, "missing"), "does not exist"},
		"not a directory": {file, "is not a directory"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			started := filepath.Join(t.TempDir(), "started")
			spec := testJobSpec(t, fakeJobBinary(t, "touch "+started))
			spec.Vms[0].Mounts = []*jobv1.Mount{{Source: dir, Tag: "mount0"}, {Source: tt.source, Tag: "mount1"}}
			events, err := runJob(t, spec)
			prefix := "mount " + tt.source + ": " + tt.want
			if err == nil || !strings.Contains(err.Error(), prefix) {
				t.Fatalf("err = %v, want one beginning %q", err, prefix)
			}
			events = afterNetworkUp(t, events)
			if len(events) != 1 || !strings.HasPrefix(events[0].GetFailed().GetReason(), prefix) || events[0].GetFailed().GetBug() {
				t.Fatalf("events = %v, want one Failed beginning %q", events, prefix)
			}
			for _, p := range []string{started, spec.GetVms()[0].GetScratchDisk()} {
				if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s: %v, want nothing there", p, err)
				}
			}
		})
	}
}

// The VM child's spec carries each disk's source, block id and read-write
// flag, in the job spec's order, and the VM child inherits each disk's
// file as fd 5 and up, in the same order, read-only (STO-1, STO-4).
func TestRunPassesTheDisks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	saved := filepath.Join(dir, "vm-child-spec")
	bin := filepath.Join(dir, "runner-microvm-job")
	script := "#!/bin/sh\n[ \"$1\" = vm ] || exit 99\ncat > " + saved + "\n" +
		"cat <&5 > " + dir + "/fd5\ncat <&6 > " + dir + "/fd6\n" +
		"if echo x >&5; then echo writable > " + dir + "/fd5-writable; fi\n"
	testbin.Write(t, bin, script)
	store, tools := filepath.Join(dir, "store.ext4"), filepath.Join(dir, "tools.erofs")
	for _, p := range []string{store, tools} {
		if err := os.WriteFile(p, []byte(filepath.Base(p)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	spec := testJobSpec(t, bin)
	spec.Vms[0].Disks = []*jobv1.Disk{{Source: store, BlockId: "disk0"}, {Source: tools, BlockId: "disk1"}}
	if _, err := runJob(t, spec); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(saved)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	child := &vmv1.Spec{}
	if err := protodelim.UnmarshalFrom(bufio.NewReader(f), child); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range child.GetDisks() {
		got = append(got, fmt.Sprintf("%s %s rw=%v", d.GetSource(), d.GetBlockId(), d.GetReadWrite()))
	}
	if want := []string{store + " disk0 rw=false", tools + " disk1 rw=false"}; !slices.Equal(got, want) {
		t.Errorf("the VM child's disks are %q, want %q", got, want)
	}
	for fd, want := range map[string]string{"fd5": "store.ext4", "fd6": "tools.erofs"} {
		if b, err := os.ReadFile(filepath.Join(dir, fd)); err != nil || string(b) != want {
			t.Errorf("the VM child's %s read %q, %v, want %q", fd, b, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "fd5-writable")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the VM child could write a read-only disk's fd 5: %v", err)
	}
}

// A disk source that is gone, or is not a regular file, by Start fails the
// job before the scratch disk is made and the VM child starts, naming the
// source; a FIFO there fails at once instead of blocking the open
// (PROTO-10).
func TestRunFailsOnADiskSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good, fifo := filepath.Join(dir, "good.img"), filepath.Join(dir, "fifo")
	if err := os.WriteFile(good, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		source, want string
	}{
		"missing":   {filepath.Join(dir, "missing"), "does not exist"},
		"directory": {dir, "is not a regular file"},
		"FIFO":      {fifo, "is not a regular file"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			started := filepath.Join(t.TempDir(), "started")
			spec := testJobSpec(t, fakeJobBinary(t, "touch "+started))
			spec.Vms[0].Disks = []*jobv1.Disk{{Source: good, BlockId: "disk0"}, {Source: tt.source, BlockId: "disk1"}}
			events, err := runJob(t, spec)
			prefix := "disk " + tt.source + ": " + tt.want
			if err == nil || !strings.Contains(err.Error(), prefix) {
				t.Fatalf("err = %v, want one beginning %q", err, prefix)
			}
			events = afterNetworkUp(t, events)
			if len(events) != 1 || !strings.HasPrefix(events[0].GetFailed().GetReason(), prefix) || events[0].GetFailed().GetBug() {
				t.Fatalf("events = %v, want one Failed beginning %q", events, prefix)
			}
			for _, p := range []string{started, spec.GetVms()[0].GetScratchDisk()} {
				if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s: %v, want nothing there", p, err)
				}
			}
		})
	}
}

func TestRunFails(t *testing.T) {
	t.Parallel()
	missing := testJobSpec(t, filepath.Join(t.TempDir(), "no-such-binary"))
	noLog := testJobSpec(t, fakeJobBinary(t, "exit 0"))
	noLog.Vms[0].VmLog = filepath.Join(t.TempDir(), "no", "such", "dir", "vm.log")
	noLevel := testJobSpec(t, fakeJobBinary(t, "exit 0"))
	noLevel.LogLevel = ""
	badLevel := testJobSpec(t, fakeJobBinary(t, "exit 0"))
	badLevel.LogLevel = "trace"
	for name, tt := range map[string]struct {
		spec      *jobv1.Spec
		want      string
		networkUp bool
		// Only the plugin's bug can cause it: Start reports Internal.
		bug bool
	}{
		"no VM child":   {missing, "start the VM child", true, false},
		"no VM log":     {noLog, "VM log", true, false},
		"no VMs at all": {&jobv1.Spec{}, "0 VMs", false, true},
		"no log level":  {noLevel, `log level ""`, false, true},
		"a bad level":   {badLevel, `log level "trace"`, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			events, err := runJob(t, tt.spec)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one naming %q", err, tt.want)
			}
			if tt.networkUp {
				events = afterNetworkUp(t, events)
			}
			if len(events) != 1 || !strings.Contains(events[0].GetFailed().GetReason(), tt.want) {
				t.Fatalf("events = %v, want one Failed naming %q", events, tt.want)
			}
			if got := events[0].GetFailed().GetBug(); got != tt.bug {
				t.Errorf("Failed's bug = %v, want %v", got, tt.bug)
			}
		})
	}
}

// The VM child gets the liveness pipe as fd 3 and its VM's end of the frame
// socket as fd 4.
func TestRunPassesTheLivenessPipeAndTheFrameSocket(t *testing.T) {
	t.Parallel()
	live, _ := livenessPipe(t)
	var st syscall.Stat_t
	if err := syscall.Fstat(int(live.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`[ -S /proc/self/fd/4 ] && [ "$(readlink /proc/self/fd/3)" = "pipe:[%d]" ] || exit 7`, st.Ino)
	th := startThread()
	defer th.stop()
	events, err := runOn(t, testJobSpec(t, fakeJobBinary(t, script)), live, th)
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
		events, err := runJob(t, spec)
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
	live, _ := livenessPipe(t)
	if err := Run(t.Context(), strings.NewReader("\xff\xff\xff\xff\xff"), &out, live, slog.New(slog.DiscardHandler), new(slog.LevelVar)); err == nil {
		t.Fatal("a garbled spec ran")
	}
	ev := &jobv1.Event{}
	if err := protodelim.UnmarshalFrom(bufio.NewReader(&out), ev); err != nil || !ev.GetFailed().GetBug() {
		t.Fatalf("event = %v, %v; want Failed, as the plugin's bug", ev, err)
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
	testbin.Write(t, bin, script)
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
		events, _ := runJob(t, spec)
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
