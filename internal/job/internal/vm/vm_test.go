// SPDX-License-Identifier: GPL-3.0-or-later

package vm

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	vmv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/job/internal/gen/runnermicrovm/vm/v1"
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
		disableImplicitVsock: func(c uint32) int32 { return rec("disable_implicit_vsock", c) },
		addVsock:             func(c, tsi uint32) int32 { return rec("add_vsock", c, tsi) },
		addVsockPort2: func(c, port uint32, p string, listen bool) int32 {
			return rec("add_vsock_port2", c, port, p, listen)
		},
		setConsoleOutput: func(c uint32, p string) int32 { return rec("set_console_output", c, p) },
		setExec: func(c uint32, p string, argv, envp []string) int32 {
			return rec("set_exec", c, p, argv, envp)
		},
		startEnter: func(c uint32) int32 { return rec("start_enter", c) },
	}
}

func testSpec() *vmv1.Spec {
	return &vmv1.Spec{
		Cpus: 2, MemoryMib: 1024, RootDir: "/state/jobs/j/0/root", LayerDir: "/cache/jobs/j/0", Console: "/state/jobs/j/0/console",
		AgentSocket: "/runtime/j/0/agent.sock", AgentPort: 1024, ScratchDisk: "/state/jobs/j/0/scratch.ext4",
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
		"add_virtiofs3 7 layers /cache/jobs/j/0 0 true",
		"add_disk 7 scratch /state/jobs/j/0/scratch.ext4 false",
		"disable_implicit_vsock 7",
		"add_vsock 7 0",
		"add_vsock_port2 7 1024 agent.sock true",
		"set_console_output 7 /state/jobs/j/0/console",
		"set_exec 7 /.runner-microvm-agent [] [PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin]",
		"start_enter 7",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
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
