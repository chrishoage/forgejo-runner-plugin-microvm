// SPDX-License-Identifier: GPL-3.0-or-later

package vm

import (
	"fmt"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The libraries' names: the sonames of the minimum versions, libkrun's 1.x
// API and libkrunfw's 5 (VM-10). They load from the plugin's lib_dir, and
// only from there, or by soname through the system's dynamic loader.
const (
	libkrun   = "libkrun.so.1"
	libkrunfw = "libkrunfw.so.5"
)

// krun is the part of libkrun's 1.x API the VM child calls. Each call
// returns a negative errno on failure.
type krun struct {
	setLogLevel          func(level uint32) int32
	createCtx            func() int32
	setVMConfig          func(ctx uint32, cpus uint8, memMiB uint32) int32
	addVirtiofs3         func(ctx uint32, tag, path string, shmSize uint64, readOnly bool) int32
	addDisk              func(ctx uint32, blockID, path string, readOnly bool) int32
	addNetUnixgram       func(ctx uint32, path *byte, fd int32, mac *byte, features, flags uint32) int32
	disableImplicitVsock func(ctx uint32) int32
	addVsock             func(ctx uint32, tsiFeatures uint32) int32
	addVsockPort2        func(ctx uint32, port uint32, path string, listen bool) int32
	setConsoleOutput     func(ctx uint32, path string) int32
	setExec              func(ctx uint32, path string, argv, envp []string) int32
	startEnter           func(ctx uint32) int32
}

// load opens libkrun from dir, or by soname when dir is empty. libkrun
// opens libkrunfw by its soname, so from dir libkrunfw is opened first:
// libkrun's own lookup then finds it already loaded.
func load(dir string) (krun, error) {
	name := libkrun
	if dir != "" {
		if _, err := purego.Dlopen(filepath.Join(dir, libkrunfw), purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
			return krun{}, fmt.Errorf("%s: %w", filepath.Join(dir, libkrunfw), err)
		}
		name = filepath.Join(dir, libkrun)
	}
	h, err := purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return krun{}, fmt.Errorf("%s: %w", name, err)
	}
	var k krun
	var setExec func(ctx uint32, path string, argv, envp unsafe.Pointer) int32
	for sym, fn := range map[string]any{
		"krun_set_log_level":          &k.setLogLevel,
		"krun_create_ctx":             &k.createCtx,
		"krun_set_vm_config":          &k.setVMConfig,
		"krun_add_virtiofs3":          &k.addVirtiofs3,
		"krun_add_disk":               &k.addDisk,
		"krun_add_net_unixgram":       &k.addNetUnixgram,
		"krun_disable_implicit_vsock": &k.disableImplicitVsock,
		"krun_add_vsock":              &k.addVsock,
		"krun_add_vsock_port2":        &k.addVsockPort2,
		"krun_set_console_output":     &k.setConsoleOutput,
		"krun_set_exec":               &setExec,
		"krun_start_enter":            &k.startEnter,
	} {
		if _, err := purego.Dlsym(h, sym); err != nil {
			return krun{}, fmt.Errorf("%s: %w", name, err)
		}
		purego.RegisterLibFunc(fn, h, sym)
	}
	k.setExec = func(ctx uint32, path string, argv, envp []string) int32 {
		a, e := cStrings(argv), cStrings(envp)
		defer runtime.KeepAlive(a)
		defer runtime.KeepAlive(e)
		return setExec(ctx, path, unsafe.Pointer(&a[0]), unsafe.Pointer(&e[0])) //nolint:gosec // G103: a NULL-terminated char*[] for C; kept alive for the call
	}
	return k, nil
}

// cStrings returns ss as a NULL-terminated array of NUL-terminated strings.
func cStrings(ss []string) []*byte {
	out := make([]*byte, 0, len(ss)+1)
	for _, s := range ss {
		b := append([]byte(s), 0)
		out = append(out, &b[0])
	}
	return append(out, nil)
}

// callError is a libkrun call that returned a negative errno.
type callError struct {
	call  string
	errno syscall.Errno
}

func (e *callError) Error() string { return e.call + ": " + e.errno.Error() }

func (e *callError) Unwrap() error { return e.errno }

// check returns nil for a call's non-negative result, and its error
// otherwise.
func check(call string, r int32) error {
	if r < 0 {
		return &callError{call: call, errno: syscall.Errno(-r)}
	}
	return nil
}
