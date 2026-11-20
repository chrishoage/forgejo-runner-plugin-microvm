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
	"os"
	"path/filepath"
	"runtime"

	"google.golang.org/protobuf/encoding/protodelim"

	vmv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/job/internal/gen/runnermicrovm/vm/v1"
)

// AgentPath is where the agent is in the VM's root, and what libkrun starts
// as its workload.
//
// SHORTCUT(#6): provisional pick, and the same path is written by the plugin
// (internal/plugin's agentPath); the protocol test fails if they part.
const AgentPath = "/.runner-microvm-agent"

// agentEnv is the agent's whole environment.
var agentEnv = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}

// libkrun's log level: warnings and errors, on the VM child's stderr.
const krunLogWarn = 2

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
		// SHORTCUT(#5): the root is a read-write host directory holding
		// the flattened image.
		{"krun_set_root", func() int32 { return lib.setRoot(ctx, spec.GetRootDir()) }},
		// SHORTCUT(#7): provisional pick: no implicit vsock and no
		// transparent socket impersonation, and no network device, so the
		// guest has no network at all.
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
