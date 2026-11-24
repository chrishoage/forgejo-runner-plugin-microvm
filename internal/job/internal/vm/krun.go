// SPDX-License-Identifier: GPL-3.0-or-later

package vm

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
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

// Files are the files the dynamic loader picked for libkrun and libkrunfw,
// as it names them: from lib_dir, the directory's own entries, symlinks
// unresolved.
type Files struct {
	Libkrun, Libkrunfw string
}

// fwSymbol is the libkrunfw function libkrun calls for the guest kernel: a
// file without it is not libkrunfw.
const fwSymbol = "krunfw_get_kernel"

// load opens libkrunfw and libkrun from dir, and only from there, or by
// soname through the system's dynamic loader when dir is empty, and
// requires every libkrun function the VM child calls. It returns the
// libraries' files too.
//
// libkrun needs libkrunfw by its soname, so from dir libkrunfw is opened
// first: libkrun's own need is then met by the one already loaded, never
// by a search of the system. By soname, libkrun is opened first, so that
// libkrunfw is the one libkrun's need picked, which a search from libkrun
// finds through libkrun's own run path; opening libkrunfw by soname then
// returns that one.
func load(dir string) (krun, Files, error) {
	var hk, hfw uintptr
	var err error
	if dir != "" {
		if hfw, err = open(filepath.Join(dir, libkrunfw)); err != nil {
			return krun{}, Files{}, fmt.Errorf("%s in lib_dir %s: %w", libkrunfw, dir, err)
		}
		if hk, err = open(filepath.Join(dir, libkrun)); err != nil {
			return krun{}, Files{}, fmt.Errorf("%s in lib_dir %s: %w", libkrun, dir, err)
		}
		// libkrun's libkrunfw is the one loaded by soname: it must be
		// dir's, or libkrun took another from the system.
		if h, err := purego.Dlopen(libkrunfw, purego.RTLD_NOW|rtldNoload); err != nil || h != hfw {
			return krun{}, Files{}, fmt.Errorf("%s in lib_dir %s is not the %s that %s loads: is its soname %s?", libkrunfw, dir, libkrunfw, libkrun, libkrunfw)
		}
	} else {
		if hk, err = open(libkrun); err != nil {
			return krun{}, Files{}, fmt.Errorf("%s by soname: %w", libkrun, err)
		}
		if hfw, err = open(libkrunfw); err != nil {
			return krun{}, Files{}, fmt.Errorf("%s by soname: %w", libkrunfw, err)
		}
	}
	var files Files
	if files.Libkrun, err = loadedFile(hk); err != nil {
		return krun{}, Files{}, fmt.Errorf("%s: %w", libkrun, err)
	}
	if files.Libkrunfw, err = loadedFile(hfw); err != nil {
		return krun{}, Files{}, fmt.Errorf("%s: %w", libkrunfw, err)
	}
	if _, err := purego.Dlsym(hfw, fwSymbol); err != nil {
		return krun{}, Files{}, fmt.Errorf("%s (%s) is not libkrunfw: it lacks %s: %w", libkrunfw, files.Libkrunfw, fwSymbol, err)
	}
	var k krun
	var setExec func(ctx uint32, path string, argv, envp unsafe.Pointer) int32
	funcs := map[string]any{
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
	}
	// Every missing function is named, in order, not only the first.
	var missing []string
	for _, sym := range slices.Sorted(maps.Keys(funcs)) {
		if _, err := purego.Dlsym(hk, sym); err != nil {
			missing = append(missing, sym)
		}
	}
	if len(missing) > 0 {
		return krun{}, Files{}, fmt.Errorf("%s (%s) lacks %s: runner-microvm needs libkrun's 1.x API, built with block and network support",
			libkrun, files.Libkrun, strings.Join(missing, ", "))
	}
	for sym, fn := range funcs {
		purego.RegisterLibFunc(fn, hk, sym)
	}
	k.setExec = func(ctx uint32, path string, argv, envp []string) int32 {
		a, e := cStrings(argv), cStrings(envp)
		defer runtime.KeepAlive(a)
		defer runtime.KeepAlive(e)
		return setExec(ctx, path, unsafe.Pointer(&a[0]), unsafe.Pointer(&e[0])) //nolint:gosec // G103: a NULL-terminated char*[] for C; kept alive for the call
	}
	return k, files, nil
}

// rtldNoload makes dlopen return a library already loaded, and load none
// (RTLD_NOLOAD, <dlfcn.h>).
const rtldNoload = 0x4

// open opens the library name, a path or a soname, binding every symbol at
// once, so a library that can't be linked fails here, not at a call.
func open(name string) (uintptr, error) {
	return purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}

// rtldDILinkmap is dlinfo's request for the library's struct link_map
// (RTLD_DI_LINKMAP, <dlfcn.h>).
const rtldDILinkmap = 2

// linkMap is the start of glibc's and musl's struct link_map, whose layout
// <link.h> makes public: the load address, then the file's name.
type linkMap struct {
	_    uintptr
	name *byte
}

// loadedFile returns the file the dynamic loader loaded for the library
// handle h, as the loader names it (dlinfo's link map).
func loadedFile(h uintptr) (string, error) {
	sym, err := purego.Dlsym(purego.RTLD_DEFAULT, "dlinfo")
	if err != nil {
		return "", fmt.Errorf("find dlinfo: %w", err)
	}
	var dlinfo func(handle uintptr, request int32, info unsafe.Pointer) int32
	purego.RegisterFunc(&dlinfo, sym)
	var lm *linkMap
	if dlinfo(h, rtldDILinkmap, unsafe.Pointer(&lm)) != 0 || lm == nil || lm.name == nil { //nolint:gosec // G103: dlinfo's out parameter, a struct link_map *
		return "", errors.New("dlinfo found no link map")
	}
	return unix.BytePtrToString(lm.name), nil
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
