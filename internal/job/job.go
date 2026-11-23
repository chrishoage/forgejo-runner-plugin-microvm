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
	"math"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/encoding/protodelim"

	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
	vmv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/job/internal/gen/runnermicrovm/vm/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/job/internal/jobnet"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/job/internal/vm"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/logging"
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

// Run is the job process: it reads the job spec from in, starts the job
// network and attaches every VM to it, reports NetworkUp, runs its VM in a
// VM child, and writes status events to out until the VM child exits. A VM
// never boots before its switch serves its socket. Cancelling ctx kills the
// VM child.
//
// SHORTCUT(#6): no liveness pipe and no Pdeathsig, so the job process and
// its VM child outlive a plugin that dies.
func Run(ctx context.Context, in io.Reader, out io.Writer, log *slog.Logger) error {
	spec := &jobv1.Spec{}
	if err := (protodelim.UnmarshalOptions{MaxSize: maxSpec}).UnmarshalFrom(bufio.NewReader(in), spec); err != nil {
		return fail(out, fmt.Errorf("read the job spec: %w", err))
	}
	if n := len(spec.GetVms()); n != 1 {
		return fail(out, fmt.Errorf("the job spec has %d VMs; a job has exactly one, the job's container", n))
	}
	log = log.With("job", spec.GetJob())
	network, links, err := startNetwork(spec, log)
	if err != nil {
		return fail(out, fmt.Errorf("network: %w", err))
	}
	defer func() {
		for _, l := range links {
			_ = l.Close()
		}
		if err := network.Close(); err != nil {
			log.Warn("job network", "err", err)
		}
	}()
	if err := send(out, &jobv1.Event{Event: &jobv1.Event_NetworkUp{NetworkUp: &jobv1.NetworkUp{}}}); err != nil {
		return err
	}
	v := spec.GetVms()[0]
	code, err := runVM(ctx, spec, v, links[0], out, log)
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

// startNetwork starts the spec's job network and attaches each of its VMs,
// returning each VM's end of its frame socket, in the spec's order.
func startNetwork(spec *jobv1.Spec, log *slog.Logger) (*jobnet.Network, []*os.File, error) {
	cfg, err := networkConfig(spec)
	if err != nil {
		return nil, nil, err
	}
	n, err := jobnet.New(cfg, log)
	if err != nil {
		return nil, nil, err
	}
	links := make([]*os.File, 0, len(spec.GetVms()))
	for _, v := range spec.GetVms() {
		l, err := n.Attach()
		if err != nil {
			for _, l := range links {
				_ = l.Close()
			}
			return nil, nil, errors.Join(fmt.Errorf("VM %s: %w", v.GetName(), err), n.Close())
		}
		links = append(links, l)
	}
	return n, links, nil
}

// networkConfig is the job network the spec describes.
func networkConfig(spec *jobv1.Spec) (jobnet.Config, error) {
	n := spec.GetNetwork()
	subnet, err := netip.ParsePrefix(n.GetSubnet())
	if err != nil {
		return jobnet.Config{}, fmt.Errorf("subnet: %w", err)
	}
	gateway, err := netip.ParseAddr(n.GetGatewayAddress())
	if err != nil {
		return jobnet.Config{}, fmt.Errorf("gateway: %w", err)
	}
	gatewayMAC, err := net.ParseMAC(n.GetGatewayMac())
	if err != nil {
		return jobnet.Config{}, fmt.Errorf("gateway: %w", err)
	}
	cfg := jobnet.Config{Subnet: subnet, Gateway: gateway, GatewayMAC: gatewayMAC, MTU: int(n.GetMtu())}
	for _, v := range spec.GetVms() {
		addr, err := netip.ParseAddr(v.GetGuestAddress())
		if err != nil {
			return jobnet.Config{}, fmt.Errorf("VM %s: %w", v.GetName(), err)
		}
		mac, err := net.ParseMAC(v.GetGuestMac())
		if err != nil {
			return jobnet.Config{}, fmt.Errorf("VM %s: %w", v.GetName(), err)
		}
		cfg.VMs = append(cfg.VMs, jobnet.VM{Address: addr, MAC: mac})
	}
	return cfg, nil
}

// runVM starts VM 0's VM child, with link, its VM's end of its frame
// socket, as its fd 4, reports VMStarted, and waits for it to exit. It
// returns the VM child's exit code, 128+n when signal n ended it.
func runVM(ctx context.Context, spec *jobv1.Spec, v *jobv1.VM, link *os.File, out io.Writer, log *slog.Logger) (int32, error) {
	if err := createScratchDisk(ctx, spec.GetMkfsExt4(), v.GetScratchDisk(), v.GetScratchSizeBytes()); err != nil {
		return 0, fmt.Errorf("scratch disk %s: %w", v.GetScratchDisk(), err)
	}
	childSpec, err := specFile(&vmv1.Spec{
		LibDir:      spec.GetLibDir(),
		Cpus:        v.GetCpus(),
		MemoryMib:   v.GetMemoryMib(),
		RootDir:     v.GetRootDir(),
		ScratchDisk: v.GetScratchDisk(),
		LayerDir:    v.GetLayerDir(),
		Console:     v.GetConsole(),
		AgentSocket: v.GetAgentSocket(),
		AgentPort:   v.GetAgentPort(),
		GuestMac:    v.GetGuestMac(),
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
	// fd 3 is the liveness pipe's, which the VM child doesn't have yet
	// (Run, above), and fd 4 its VM's end of the frame socket.
	cmd.ExtraFiles = []*os.File{nil, link}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start the VM child: %w", err)
	}
	// Only the VM child holds the VM's end now, so once it exits the
	// switch's writes to it fail instead of filling its buffer.
	_ = link.Close()
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

// mkfsArgs format a scratch disk quickly: the inode tables and journal are
// initialised lazily by the guest, so formatting takes about 50 ms and
// allocates about 4 MB, and the root directory is root's.
var mkfsArgs = []string{"-q", "-F", "-E", "lazy_itable_init=1,lazy_journal_init=1,root_owner=0:0"}

// createScratchDisk creates path as a sparse file of size bytes, mode 0600,
// and formats it as ext4 with mkfs. The file must not exist: a scratch disk
// is never reused, so nothing written in one job is visible in the next
// (VM-5). A disk that can't be made is deleted.
func createScratchDisk(ctx context.Context, mkfs, path string, size uint64) (err error) {
	if size == 0 || size > math.MaxInt64 {
		return fmt.Errorf("scratch size %d bytes", size)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the scratch disk the plugin named in the spec
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	err = f.Truncate(int64(size))
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	// mkfs is in the job process's group, so killing the job kills it.
	cmd := exec.CommandContext(ctx, mkfs, append(mkfsArgs, path)...) //nolint:gosec // G204: the mkfs.ext4 the plugin found at startup
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", mkfs, err, strings.TrimSpace(string(out)))
	}
	return nil
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
