// SPDX-License-Identifier: GPL-3.0-or-later

// Package job is the job process: it reads its spec from the control
// channel, runs the job network, locks the read-write disks, starts a VM child
// for each of the spec's VMs and reports status events. With `vm` it is a VM
// child instead.
//
// The job network and the VM child are under internal/, so the compiler stops
// any package outside this one from importing them.
package job

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/encoding/protodelim"

	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
	vmv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/job/internal/gen/runnermicrovm/vm/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/job/internal/vm"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/logging"

	// SHORTCUT(#7): no job network yet; the blank import keeps
	// gvisor-tap-vsock in the job binary for the dependency test.
	_ "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/job/internal/jobnet"
)

// Version is what `runner-microvm-job version` prints. The plugin runs it
// once at startup, to prove the extracted job binary can execute.
const Version = "runner-microvm-job"

// Main runs the job binary: with no arguments the job process, with `vm` a
// VM child, with `version` its version. It returns the exit code.
func Main(args []string) int {
	switch {
	case len(args) == 1 && args[0] == "version":
		fmt.Println(Version)
		return 0
	case len(args) == 1 && args[0] == "vm":
		return vm.Main(os.Stdin, logging.Setup("runner-microvm-vm"))
	case len(args) == 0:
		log := logging.Setup("runner-microvm-job")
		if err := Run(context.Background(), os.Stdin, os.Stdout, log); err != nil {
			log.Error("job process", "err", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "usage: runner-microvm-job [vm | version]\n")
		return 2
	}
}

// The most of a console or VM log file a VMExited carries.
const tailBytes = 300

// maxSpec bounds the job spec read from the control channel.
const maxSpec = 1 << 20

// Run is the job process: it reads the job spec from in, runs its VM in a
// VM child, and writes status events to out until the VM child exits.
// Cancelling ctx kills the VM child.
//
// SHORTCUT(#6): no liveness pipe and no Pdeathsig, so the job process and
// its VM child outlive a plugin that dies.
// SHORTCUT(#7): no job network, so no NetworkUp.
func Run(ctx context.Context, in io.Reader, out io.Writer, log *slog.Logger) error {
	spec := &jobv1.Spec{}
	if err := (protodelim.UnmarshalOptions{MaxSize: maxSpec}).UnmarshalFrom(bufio.NewReader(in), spec); err != nil {
		return fail(out, fmt.Errorf("read the job spec: %w", err))
	}
	if n := len(spec.GetVms()); n != 1 {
		return fail(out, fmt.Errorf("the job spec has %d VMs; a job has exactly one, the job's container", n))
	}
	log = log.With("job", spec.GetJob())
	v := spec.GetVms()[0]
	code, err := runVM(ctx, spec, v, out, log)
	if err != nil {
		return fail(out, fmt.Errorf("VM %s: %w", v.GetName(), err))
	}
	log.Info("VM exited", "vm", v.GetName(), "exit_code", code)
	return send(out, &jobv1.Event{Event: &jobv1.Event_VmExited{VmExited: &jobv1.VMExited{
		Vm:          0,
		ExitCode:    code,
		ConsoleTail: tail(v.GetConsole()),
		LogTail:     tail(v.GetVmLog()),
	}}})
}

// runVM starts VM 0's VM child, reports VMStarted, and waits for it to
// exit. It returns the VM child's exit code, 128+n when signal n ended it.
func runVM(ctx context.Context, spec *jobv1.Spec, v *jobv1.VM, out io.Writer, log *slog.Logger) (int32, error) {
	childSpec, err := specFile(&vmv1.Spec{
		LibDir:      spec.GetLibDir(),
		Cpus:        v.GetCpus(),
		MemoryMib:   v.GetMemoryMib(),
		RootDir:     v.GetRootDir(),
		Console:     v.GetConsole(),
		AgentSocket: v.GetAgentSocket(),
		AgentPort:   v.GetAgentPort(),
		Boot:        v.GetBoot(),
	})
	if err != nil {
		return 0, fmt.Errorf("VM child spec: %w", err)
	}
	defer func() { _ = childSpec.Close() }()
	vmLog, err := os.OpenFile(v.GetVmLog(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("VM log: %w", err)
	}
	defer func() { _ = vmLog.Close() }()
	// The VM child stays in the job process's group, so killing the group
	// kills it too. Its spec goes in on stdin, never on its command line,
	// from a file, so no goroutine copies it in: a VM child that never
	// reads it, or a process it left holding stdin, can't keep Wait
	// waiting.
	cmd := exec.CommandContext(ctx, spec.GetJobBinary(), "vm") //nolint:gosec // G204: the plugin names its own job binary
	cmd.Stdin = childSpec
	cmd.Stdout, cmd.Stderr = vmLog, vmLog
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start the VM child: %w", err)
	}
	log.Info("VM started", "vm", v.GetName(), "pid", cmd.Process.Pid)
	err = send(out, &jobv1.Event{Event: &jobv1.Event_VmStarted{VmStarted: &jobv1.VMStarted{
		Vm: 0, Pid: int32(cmd.Process.Pid), //nolint:gosec // G115: a pid fits in an int32
	}}})
	if err != nil {
		// Nobody reads the control channel: the VM is of no use.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 0, err
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return 0, err
	}
	ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if ws.Signaled() {
		return 128 + int32(ws.Signal()), nil //nolint:gosec // G115: a signal number is below 65
	}
	return int32(ws.ExitStatus()), nil //nolint:gosec // G115: an exit status is 0 to 255
}

// specFile returns a memory file holding spec, length-delimited, to be read
// from its start.
func specFile(spec *vmv1.Spec) (*os.File, error) {
	fd, err := unix.MemfdCreate("vm-child-spec", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "vm-child-spec")
	_, err = protodelim.MarshalTo(f, spec)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// tail returns the end of the file at path, at most tailBytes; a file that
// can't be read has an empty tail.
func tail(path string) string {
	f, err := os.Open(path) //nolint:gosec // G304: a log file the plugin named in the spec
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err == nil && fi.Size() > tailBytes {
		if _, err := f.Seek(-tailBytes, io.SeekEnd); err != nil {
			return ""
		}
	}
	b, _ := io.ReadAll(io.LimitReader(f, tailBytes))
	return string(b)
}

// send writes one event to the control channel.
func send(out io.Writer, ev *jobv1.Event) error {
	_, err := protodelim.MarshalTo(out, ev)
	return err
}

// fail reports err as Failed, and returns it.
func fail(out io.Writer, err error) error {
	return errors.Join(err, send(out, &jobv1.Event{Event: &jobv1.Event_Failed{Failed: &jobv1.Failed{Reason: err.Error()}}}))
}
