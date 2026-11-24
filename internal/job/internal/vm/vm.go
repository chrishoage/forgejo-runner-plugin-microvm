// SPDX-License-Identifier: GPL-3.0-or-later

// Package vm is the VM child: it loads libkrun with purego, configures the VM
// from its spec and inherited descriptors, and starts it. It is the only code
// that loads libkrun (docs/adr/0004-libkrun-via-purego.md).
package vm

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"google.golang.org/protobuf/encoding/protodelim"

	vmv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/job/internal/gen/runnermicrovm/vm/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/logging"
)

// AgentPath is where the agent is in the VM's boot root, and what libkrun's
// init, which stays PID 1, starts as its workload. The plugin links the
// agent there (internal/plugin's agentPath); every booted VM's test fails if
// they part.
const AgentPath = "/.runner-microvm/agent"

// agentEnv is the agent's whole environment: empty, and never nil, since
// libkrun passes the host's environment into the guest for a NULL one. The
// agent looks nothing up in PATH, and every step gets its environment from
// its Exec.
var agentEnv = []string{}

// rootTag is the virtiofs tag libkrun boots from (KRUN_FS_ROOT_TAG).
const rootTag = "/dev/root"

// scratchBlockID is the scratch disk's block id in libkrun.
const scratchBlockID = "scratch"

// maxTag is the longest virtiofs tag libkrun takes: it copies a tag into a
// 36-byte buffer with no length check, and panics when the VM starts on a
// longer one.
const maxTag = 36

// maxBlockID is the longest disk block id the VM child takes: the guest
// sees the id as the disk's virtio-blk serial, which is 20 bytes, so a
// longer one would reach the guest cut short.
const maxBlockID = 20

// firstDiskFD is the VM child's inherited descriptor for its first disk's
// file; disk i is firstDiskFD+i.
const firstDiskFD = 5

// krunLogLevels are libkrun's log levels by the spec's names: libkrun's
// own numbers (KRUN_LOG_LEVEL_*), on the VM child's stderr. None is below
// warnings, so libkrun's errors always reach the VM log.
var krunLogLevels = map[string]uint32{"warn": 2, "info": 3, "debug": 4, "trace": 5}

// netFD is the VM child's inherited descriptor for its VM's end of the job
// network's frame socket.
const netFD = 4

// The VM's network device: no virtio-net features, and flags 0, so neither
// vfkit's handshake nor libkrun init's DHCP client: the agent addresses the
// guest from its boot config.
const (
	netFeatures = 0
	netFlags    = 0
)

// Main reads the VM child spec from in and boots the VM. It returns only on
// failure: libkrun ends the process with the guest's exit code when the VM
// stops. log writes the VM log, at level, which Main sets to the spec's log
// level before it logs anything else.
func Main(in io.Reader, log *slog.Logger, level *slog.LevelVar) int {
	spec := &vmv1.Spec{}
	if err := (protodelim.UnmarshalOptions{MaxSize: 1 << 20}).UnmarshalFrom(bufio.NewReader(in), spec); err != nil {
		log.Error("read the VM child spec", "err", err)
		return 1
	}
	if err := logging.SetLevel(level, spec.GetLogLevel()); err != nil {
		log.Error("the VM child spec", "err", err)
		return 1
	}
	// The agent socket's path can outgrow bind's 107 bytes, so libkrun
	// binds it relative to its directory (boot).
	if err := os.Chdir(filepath.Dir(spec.GetAgentSocket())); err != nil {
		log.Error("agent socket directory", "err", err)
		return 1
	}
	lib, files, err := load(spec.GetLibDir())
	if err != nil {
		log.Error("load libkrun", "err", err)
		return 1
	}
	log.Debug("loaded libkrun and libkrunfw", "libkrun", files.Libkrun, "libkrunfw", files.Libkrunfw,
		"libkrun_log_level", spec.GetLibkrunLogLevel())
	// libkrun's calls are made from one thread, which krun_start_enter
	// never returns from.
	runtime.LockOSThread()
	if err := boot(lib, spec); err != nil {
		log.Error("boot", "err", err)
		return 1
	}
	return 0
}

// Libs loads libkrun and libkrunfw as Main does, from dir or, when it is
// empty, by soname, requiring every libkrun function Main calls, and prints
// the files the loader picked to out, one line each:
//
//	libkrun.so.1=<file>
//	libkrunfw.so.5=<file>
//
// It is `runner-microvm-job libs [<dir>]`, which the plugin runs once at
// startup, so that a missing library or function stops the plugin instead
// of failing every Start. It returns the exit code: 1 after printing the
// error to errOut.
func Libs(dir string, out, errOut io.Writer) int {
	if dir != "" && !filepath.IsAbs(dir) {
		_, _ = fmt.Fprintf(errOut, "lib_dir %s is not an absolute path\n", dir)
		return 1
	}
	_, files, err := load(dir)
	if err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return 1
	}
	if _, err := fmt.Fprintf(out, "%s=%s\n%s=%s\n", libkrun, files.Libkrun, libkrunfw, files.Libkrunfw); err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return 1
	}
	return 0
}

// boot configures one VM through lib and enters it. The working directory
// is the agent socket's.
func boot(lib krun, spec *vmv1.Spec) error {
	cpus := spec.GetCpus()
	if cpus == 0 || cpus > 255 {
		return fmt.Errorf("%d CPUs: libkrun takes 1 to 255", cpus)
	}
	vcpus := uint8(cpus)
	if spec.GetLayerTag() == "" {
		return fmt.Errorf("no layer tag")
	}
	for _, m := range spec.GetMounts() {
		if m.GetTag() == "" || len(m.GetTag()) > maxTag {
			return fmt.Errorf("mount %s: tag %q: libkrun takes 1 to %d bytes", m.GetSource(), m.GetTag(), maxTag)
		}
	}
	for _, d := range spec.GetDisks() {
		switch id := d.GetBlockId(); {
		case id == "" || len(id) > maxBlockID:
			return fmt.Errorf("disk %s: block id %q: the guest's serial holds 1 to %d bytes", d.GetSource(), id, maxBlockID)
		case id == scratchBlockID:
			return fmt.Errorf("disk %s: block id %q is the scratch disk's", d.GetSource(), id)
		}
	}
	mac, err := net.ParseMAC(spec.GetGuestMac())
	if err != nil || len(mac) != 6 {
		return fmt.Errorf("guest MAC %q: not an Ethernet address", spec.GetGuestMac())
	}
	level, ok := krunLogLevels[spec.GetLibkrunLogLevel()]
	if !ok {
		return fmt.Errorf("libkrun log level %q: not warn, info, debug or trace", spec.GetLibkrunLogLevel())
	}
	if err := check("krun_set_log_level", lib.setLogLevel(level)); err != nil {
		return err
	}
	r := lib.createCtx()
	if err := check("krun_create_ctx", r); err != nil {
		return err
	}
	ctx := uint32(r) //nolint:gosec // G115: checked non-negative above
	type call struct {
		name string
		call func() int32
	}
	// Each mount is a share of its own, read-only on the host's side
	// unless it is read-write, so the guest can't write a read-only one
	// even after remounting it read-write (STO-2). No DAX window.
	var mounts []call
	for _, m := range spec.GetMounts() {
		mounts = append(mounts, call{"krun_add_virtiofs3 " + m.GetTag(), func() int32 {
			return lib.addVirtiofs3(ctx, m.GetTag(), m.GetSource(), 0, !m.GetReadWrite())
		}})
	}
	// Each disk is added after the scratch disk, by its inherited
	// descriptor's path, so the file libkrun opens in krun_start_enter is
	// the very file the job process opened, not whatever is at its source
	// by then; read-only unless it is read-write, so the guest can't write
	// it even after remounting it read-write (STO-2). The guest finds it by
	// its block id, which it sees as its serial.
	var disks []call
	for i, d := range spec.GetDisks() {
		fdPath := "/proc/self/fd/" + strconv.Itoa(firstDiskFD+i)
		disks = append(disks, call{"krun_add_disk " + d.GetBlockId(), func() int32 {
			return lib.addDisk(ctx, d.GetBlockId(), fdPath, !d.GetReadWrite())
		}})
	}
	calls := []call{
		{"krun_set_vm_config", func() int32 { return lib.setVMConfig(ctx, vcpus, spec.GetMemoryMib()) }},
		// The boot root is the VM's root, and the layer link directory
		// a share of its own, both read-only on the host's side, so the
		// guest can't write them even after remounting them read-write
		// (IMG-9). No DAX window.
		{"krun_add_virtiofs3 " + rootTag, func() int32 { return lib.addVirtiofs3(ctx, rootTag, spec.GetRootDir(), 0, true) }},
		{"krun_add_virtiofs3 " + spec.GetLayerTag(), func() int32 { return lib.addVirtiofs3(ctx, spec.GetLayerTag(), spec.GetLayerDir(), 0, true) }},
	}
	calls = append(calls, mounts...)
	calls = append(calls, []call{
		// The scratch disk is the first block device, /dev/vda,
		// read-write (VM-5).
		{"krun_add_disk", func() int32 { return lib.addDisk(ctx, scratchBlockID, spec.GetScratchDisk(), false) }},
	}...)
	calls = append(calls, disks...)
	calls = append(calls, []call{
		// The VM's network device, on its end of the frame socket the job
		// network serves, one Ethernet frame per datagram, with no path:
		// the socket is already connected.
		{"krun_add_net_unixgram", func() int32 { return lib.addNetUnixgram(ctx, nil, netFD, &mac[0], netFeatures, netFlags) }},
		// No implicit vsock and no transparent socket impersonation: with
		// TSI on, guest sockets become host sockets, loopback included,
		// bypassing the job network (NET-4, NET-6). The only vsock port is
		// the agent's.
		{"krun_disable_implicit_vsock", func() int32 { return lib.disableImplicitVsock(ctx) }},
		{"krun_add_vsock", func() int32 { return lib.addVsock(ctx, 0) }},
		// listen: the host side connects, to the agent listening in the
		// guest.
		{"krun_add_vsock_port2", func() int32 {
			return lib.addVsockPort2(ctx, spec.GetAgentPort(), filepath.Base(spec.GetAgentSocket()), true)
		}},
		{"krun_set_console_output", func() int32 { return lib.setConsoleOutput(ctx, spec.GetConsole()) }},
		{"krun_set_exec", func() int32 { return lib.setExec(ctx, AgentPath, nil, agentEnv) }},
		{"krun_start_enter", func() int32 { return lib.startEnter(ctx) }},
	}...)
	for _, s := range calls {
		if err := check(s.name, s.call()); err != nil {
			return err
		}
	}
	return nil
}
