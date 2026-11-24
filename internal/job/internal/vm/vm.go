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

	"google.golang.org/protobuf/encoding/protodelim"

	vmv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/job/internal/gen/runnermicrovm/vm/v1"
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

// libkrun's log level: warnings and errors, on the VM child's stderr.
const krunLogWarn = 2

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
// stops.
func Main(in io.Reader, log *slog.Logger) int {
	spec := &vmv1.Spec{}
	if err := (protodelim.UnmarshalOptions{MaxSize: 1 << 20}).UnmarshalFrom(bufio.NewReader(in), spec); err != nil {
		log.Error("read the VM child spec", "err", err)
		return 1
	}
	// The agent socket's path can outgrow bind's 107 bytes, so libkrun
	// binds it relative to its directory (boot).
	if err := os.Chdir(filepath.Dir(spec.GetAgentSocket())); err != nil {
		log.Error("agent socket directory", "err", err)
		return 1
	}
	lib, err := load(spec.GetLibDir())
	if err != nil {
		log.Error("load libkrun", "err", err)
		return 1
	}
	// libkrun's calls are made from one thread, which krun_start_enter
	// never returns from.
	runtime.LockOSThread()
	if err := boot(lib, spec); err != nil {
		log.Error("boot", "err", err)
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
	mac, err := net.ParseMAC(spec.GetGuestMac())
	if err != nil || len(mac) != 6 {
		return fmt.Errorf("guest MAC %q: not an Ethernet address", spec.GetGuestMac())
	}
	if err := check("krun_set_log_level", lib.setLogLevel(krunLogWarn)); err != nil {
		return err
	}
	r := lib.createCtx()
	if err := check("krun_create_ctx", r); err != nil {
		return err
	}
	ctx := uint32(r) //nolint:gosec // G115: checked non-negative above
	for _, s := range []struct {
		name string
		call func() int32
	}{
		{"krun_set_vm_config", func() int32 { return lib.setVMConfig(ctx, vcpus, spec.GetMemoryMib()) }},
		// The boot root is the VM's root, and the layer link directory
		// a share of its own, both read-only on the host's side, so the
		// guest can't write them even after remounting them read-write
		// (IMG-9). No DAX window.
		{"krun_add_virtiofs3 " + rootTag, func() int32 { return lib.addVirtiofs3(ctx, rootTag, spec.GetRootDir(), 0, true) }},
		{"krun_add_virtiofs3 " + spec.GetLayerTag(), func() int32 { return lib.addVirtiofs3(ctx, spec.GetLayerTag(), spec.GetLayerDir(), 0, true) }},
		// The scratch disk is the first block device, /dev/vda,
		// read-write (VM-5).
		{"krun_add_disk", func() int32 { return lib.addDisk(ctx, scratchBlockID, spec.GetScratchDisk(), false) }},
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
	} {
		if err := check(s.name, s.call()); err != nil {
			return err
		}
	}
	return nil
}
