// SPDX-License-Identifier: GPL-3.0-or-later

package vm

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"google.golang.org/protobuf/encoding/protodelim"

	vmv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/job/internal/gen/runnermicrovm/vm/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/logging"
)

// fakeKrun records libkrun's calls, and fails the one named in fail.
func fakeKrun(calls *[]string, fail string) krun {
	rec := func(name string, args ...any) int32 {
		*calls = append(*calls, strings.TrimSpace(fmt.Sprintln(append([]any{name}, args...)...)))
		if name == fail {
			return -int32(syscall.EINVAL)
		}
		return 0
	}
	return krun{
		setLogLevel: func(l uint32) int32 { return rec("set_log_level", l) },
		createCtx:   func() int32 { _ = rec("create_ctx"); return 7 },
		setVMConfig: func(c uint32, n uint8, m uint32) int32 { return rec("set_vm_config", c, n, m) },
		addVirtiofs3: func(c uint32, tag, p string, shm uint64, ro bool) int32 {
			return rec("add_virtiofs3", c, tag, p, shm, ro)
		},
		addDisk: func(c uint32, id, p string, ro bool) int32 {
			return rec("add_disk", c, id, p, ro)
		},
		addNetUnixgram: func(c uint32, p *byte, fd int32, mac *byte, features, flags uint32) int32 {
			return rec("add_net_unixgram", c, p == nil, fd, net.HardwareAddr(unsafe.Slice(mac, 6)), features, flags)
		},
		disableImplicitVsock: func(c uint32) int32 { return rec("disable_implicit_vsock", c) },
		addVsock:             func(c, tsi uint32) int32 { return rec("add_vsock", c, tsi) },
		addVsockPort2: func(c, port uint32, p string, listen bool) int32 {
			return rec("add_vsock_port2", c, port, p, listen)
		},
		setConsoleOutput: func(c uint32, p string) int32 { return rec("set_console_output", c, p) },
		setExec: func(c uint32, p string, argv, envp []string) int32 {
			return rec("set_exec", c, p, argv, envp, "envp nil:", envp == nil)
		},
		startEnter: func(c uint32) int32 { return rec("start_enter", c) },
	}
}

func testSpec() *vmv1.Spec {
	return &vmv1.Spec{
		Cpus: 2, MemoryMib: 1024, RootDir: "/state/jobs/j/0/root", LayerDir: "/cache/jobs/j/0", LayerTag: "the-layers", Console: "/state/jobs/j/0/console",
		AgentSocket: "/runtime/j/0/agent.sock", AgentPort: 1024, ScratchDisk: "/state/jobs/j/0/scratch.ext4",
		GuestMac: "02:00:c6:12:00:02", LibkrunLogLevel: "warn",
	}
}

func TestBootConfiguresTheVM(t *testing.T) {
	t.Parallel()
	var calls []string
	if err := boot(fakeKrun(&calls, ""), testSpec()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"set_log_level 2",
		"create_ctx",
		"set_vm_config 7 2 1024",
		"add_virtiofs3 7 /dev/root /state/jobs/j/0/root 0 true",
		"add_virtiofs3 7 the-layers /cache/jobs/j/0 0 true",
		"add_disk 7 scratch /state/jobs/j/0/scratch.ext4 false",
		// No path, fd 4, the VM's MAC, no features and flags 0: no DHCP
		// client and no vfkit handshake.
		"add_net_unixgram 7 true 4 02:00:c6:12:00:02 0 0",
		"disable_implicit_vsock 7",
		"add_vsock 7 0",
		"add_vsock_port2 7 1024 agent.sock true",
		"set_console_output 7 /state/jobs/j/0/console",
		// The agent's environment is empty, and not nil: libkrun passes
		// the host's environment for a NULL one.
		"set_exec 7 /.runner-microvm/agent [] [] envp nil: false",
		"start_enter 7",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

// Each mount is a virtiofs share of its own, after the layer share, under
// its tag, read-only unless it is read-write, with no DAX window (STO-1,
// STO-2).
func TestBootSharesTheMounts(t *testing.T) {
	t.Parallel()
	var calls []string
	spec := testSpec()
	spec.Mounts = []*vmv1.Mount{{Source: "/srv/tools", Tag: "mount0"}, {Source: "/srv/data", Tag: "mount1", ReadWrite: true}}
	if err := boot(fakeKrun(&calls, ""), spec); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"add_virtiofs3 7 the-layers /cache/jobs/j/0 0 true",
		"add_virtiofs3 7 mount0 /srv/tools 0 true",
		"add_virtiofs3 7 mount1 /srv/data 0 false",
		"add_disk 7 scratch /state/jobs/j/0/scratch.ext4 false",
	}
	i := slices.Index(calls, want[0])
	if i < 0 || len(calls) < i+len(want) || !slices.Equal(calls[i:i+len(want)], want) {
		t.Errorf("calls:\n%s\nwant, in a row:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

// A mount's tag must be one libkrun takes: not empty, and at most 36 bytes,
// the buffer libkrun copies it into without a length check.
func TestBootRefusesABadMountTag(t *testing.T) {
	t.Parallel()
	for _, tag := range []string{"", strings.Repeat("t", 37)} {
		var calls []string
		spec := testSpec()
		spec.Mounts = []*vmv1.Mount{{Source: "/srv/tools", Tag: tag}}
		if err := boot(fakeKrun(&calls, ""), spec); err == nil || len(calls) != 0 {
			t.Errorf("tag %q: err %v, calls %v", tag, err, calls)
		}
	}
	var calls []string
	spec := testSpec()
	spec.Mounts = []*vmv1.Mount{{Source: "/srv/tools", Tag: strings.Repeat("t", 36)}}
	if err := boot(fakeKrun(&calls, ""), spec); err != nil {
		t.Errorf("a 36-byte tag: %v", err)
	}
}

// Each disk is added after the scratch disk, under its block id, by its
// inherited descriptor's path, fd 5 and up in the spec's order, read-only
// unless it is read-write (STO-1, STO-2).
func TestBootAttachesTheDisks(t *testing.T) {
	t.Parallel()
	var calls []string
	spec := testSpec()
	spec.Disks = []*vmv1.Disk{{Source: "/srv/store.ext4", BlockId: "disk0"}, {Source: "/srv/tools.erofs", BlockId: "disk1"}}
	if err := boot(fakeKrun(&calls, ""), spec); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"add_disk 7 scratch /state/jobs/j/0/scratch.ext4 false",
		"add_disk 7 disk0 /proc/self/fd/5 true",
		"add_disk 7 disk1 /proc/self/fd/6 true",
		"add_net_unixgram 7 true 4 02:00:c6:12:00:02 0 0",
	}
	i := slices.Index(calls, want[0])
	if i < 0 || len(calls) < i+len(want) || !slices.Equal(calls[i:i+len(want)], want) {
		t.Errorf("calls:\n%s\nwant, in a row:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

// A disk's block id must fit the guest's 20-byte serial, not be empty, and
// not be the scratch disk's.
func TestBootRefusesABadBlockID(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"", strings.Repeat("d", 21), "scratch"} {
		var calls []string
		spec := testSpec()
		spec.Disks = []*vmv1.Disk{{Source: "/srv/store.ext4", BlockId: id}}
		if err := boot(fakeKrun(&calls, ""), spec); err == nil || len(calls) != 0 {
			t.Errorf("block id %q: err %v, calls %v", id, err, calls)
		}
	}
	var calls []string
	spec := testSpec()
	spec.Disks = []*vmv1.Disk{{Source: "/srv/store.ext4", BlockId: strings.Repeat("d", 20)}}
	if err := boot(fakeKrun(&calls, ""), spec); err != nil {
		t.Errorf("a 20-byte block id: %v", err)
	}
}

// A failed call stops the boot there, naming the call.
func TestBootStopsAtAFailedCall(t *testing.T) {
	t.Parallel()
	var calls []string
	err := boot(fakeKrun(&calls, "add_virtiofs3"), testSpec())
	if err == nil || !strings.Contains(err.Error(), "krun_add_virtiofs3 /dev/root") || !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("err = %v, want the root share's EINVAL", err)
	}
	if slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "start_enter") }) {
		t.Error("the VM was started after a failed call")
	}
}

func TestBootRefusesABadMAC(t *testing.T) {
	t.Parallel()
	for _, mac := range []string{"", "nope", "02:00:c6:12:00:02:00:00"} {
		var calls []string
		spec := testSpec()
		spec.GuestMac = mac
		if err := boot(fakeKrun(&calls, ""), spec); err == nil || len(calls) != 0 {
			t.Errorf("MAC %q: err %v, calls %v", mac, err, calls)
		}
	}
}

// The layer share's tag comes from the spec, so the agent mounts what
// libkrun shares; an empty one would share the layers under no name.
func TestBootRefusesNoLayerTag(t *testing.T) {
	t.Parallel()
	var calls []string
	spec := testSpec()
	spec.LayerTag = ""
	if err := boot(fakeKrun(&calls, ""), spec); err == nil || len(calls) != 0 {
		t.Errorf("no layer tag: err %v, calls %v", err, calls)
	}
}

func TestBootRefusesCPUCounts(t *testing.T) {
	t.Parallel()
	for _, n := range []uint32{0, 256} {
		var calls []string
		spec := testSpec()
		spec.Cpus = n
		if err := boot(fakeKrun(&calls, ""), spec); err == nil {
			t.Errorf("%d CPUs: booted", n)
		}
		if len(calls) != 0 {
			t.Errorf("%d CPUs: called libkrun: %v", n, calls)
		}
	}
}

// libkrun's log level is the spec's, by libkrun's numbers; any other name
// fails before libkrun is called.
func TestBootSetsTheLibkrunLogLevel(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{"warn": "set_log_level 2", "info": "set_log_level 3", "debug": "set_log_level 4", "trace": "set_log_level 5"} {
		var calls []string
		spec := testSpec()
		spec.LibkrunLogLevel = name
		if err := boot(fakeKrun(&calls, ""), spec); err != nil || len(calls) == 0 || calls[0] != want {
			t.Errorf("%s: err %v, calls %v; want %q first", name, err, calls, want)
		}
	}
	for _, name := range []string{"", "error", "off", "DEBUG"} {
		var calls []string
		spec := testSpec()
		spec.LibkrunLogLevel = name
		if err := boot(fakeKrun(&calls, ""), spec); err == nil || len(calls) != 0 {
			t.Errorf("%q: err %v, calls %v; want an error and no call", name, err, calls)
		}
	}
}

func TestCStrings(t *testing.T) {
	t.Parallel()
	got := cStrings([]string{"A=1"})
	if len(got) != 2 || got[1] != nil {
		t.Fatalf("not NULL-terminated: %v", got)
	}
	if b := unsafe.Slice(got[0], 4); string(b) != "A=1\x00" {
		t.Errorf("string = %q", b)
	}
	if got := cStrings(nil); len(got) != 1 || got[0] != nil {
		t.Errorf("empty = %v", got)
	}
}

// Main sets the VM log's level to the spec's before anything else, and
// fails on any other level before it loads libkrun.
func TestMainSetsTheLogLevel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		level, want string
		wantLevel   slog.Level
	}{
		// testSpec's agent socket directory doesn't exist: Main fails
		// there once the level is set, before it loads libkrun.
		{"warn", "agent socket directory", slog.LevelWarn},
		{"debug", "agent socket directory", slog.LevelDebug},
		{"", "not debug, info, warn or error", slog.LevelInfo},
		{"trace", "not debug, info, warn or error", slog.LevelInfo},
	} {
		spec := testSpec()
		spec.LogLevel = tc.level
		var in, log bytes.Buffer
		if _, err := protodelim.MarshalTo(&in, spec); err != nil {
			t.Fatal(err)
		}
		var v slog.LevelVar
		if code := Main(&in, logging.New(&log, "test", &v), &v); code != 1 || !strings.Contains(log.String(), tc.want) || v.Level() != tc.wantLevel {
			t.Errorf("%q: exit %d, level %v, log:\n%s\nwant 1, %v and %q", tc.level, code, v.Level(), log.String(), tc.wantLevel, tc.want)
		}
	}
}
