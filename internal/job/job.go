// SPDX-License-Identifier: GPL-3.0-or-later

// Package job is the job process: it reads its spec from the control
// channel, runs the job network, takes its disks' locks, starts a VM child
// for each of the spec's VMs and reports status events. With `vm` it is a VM
// child instead.
//
// The job network and the VM child are under internal/, so the compiler stops
// any package outside this one from importing them.
package job

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
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
// VM child, with `version` its version, and with `libs [<dir>]` the check of
// libkrun and libkrunfw that the plugin runs at startup (vm.Libs). It
// returns the exit code.
func Main(args []string) int {
	switch {
	case len(args) == 1 && args[0] == "version":
		fmt.Println(Version)
		return 0
	case len(args) == 1 && args[0] == "libs":
		return vm.Libs("", os.Stdout, os.Stderr)
	case len(args) == 2 && args[0] == "libs":
		return vm.Libs(args[1], os.Stdout, os.Stderr)
	case len(args) == 1 && args[0] == "vm":
		log, level := logging.Setup("runner-microvm-vm")
		live, err := liveness()
		if err != nil {
			log.Error("VM child", "err", err)
			return 1
		}
		go dieWithPlugin(live, log, killSelf)
		return vm.Main(os.Stdin, log, level)
	case len(args) == 0:
		log, level := logging.Setup("runner-microvm-job")
		live, err := liveness()
		if err != nil {
			log.Error("job process", "err", err)
			return 1
		}
		go dieWithPlugin(live, log, killGroup)
		if err := Run(context.Background(), os.Stdin, os.Stdout, live, log, level); err != nil {
			log.Error("job process", "err", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "usage: runner-microvm-job [vm | version | libs [<dir>]]\n")
		return 2
	}
}

// The most of a console or VM log file a VMExited carries: what the
// plugin's error for the VM's exit keeps, its last 20 lines, is at most
// this much too (PROTO-10, VM-8).
const tailBytes = 4 << 10

// maxVMs is the most VMs a job has: the job network's /24 gives the gateway
// .1 and VM n .(2+n), up to .254.
const maxVMs = 253

// maxSpec bounds the job spec read from the control channel.
const maxSpec = 1 << 20

// livenessFD is the liveness pipe's descriptor in the job process and in
// each VM child.
const livenessFD = 3

// liveness returns the liveness pipe this process inherited as fd 3, made
// close-on-exec, so only a child it is handed to explicitly holds it. Being
// open is not enough: when fd 3 is free at exec, Go's runtime keeps a file
// of its own there (its cgroup's CPU limit), so fd 3 must be a pipe.
func liveness() (*os.File, error) {
	var st unix.Stat_t
	if err := unix.Fstat(livenessFD, &st); err != nil {
		return nil, fmt.Errorf("no liveness pipe on fd %d: %w", livenessFD, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFIFO {
		return nil, fmt.Errorf("fd %d is not the liveness pipe: mode %#o", livenessFD, st.Mode)
	}
	unix.CloseOnExec(livenessFD)
	return os.NewFile(livenessFD, "liveness pipe"), nil
}

// dieWithPlugin reads the liveness pipe until it ends, which it does once
// the plugin, the only holder of its write end, is gone, and then calls
// kill. Nothing is ever written to the pipe.
func dieWithPlugin(live *os.File, log *slog.Logger, kill func()) {
	_, err := io.Copy(io.Discard, live)
	log.Error("the plugin is gone: its liveness pipe ended", "err", err)
	kill()
}

// killGroup kills the job process's group, its VM children and mkfs
// included, and so itself. The launcher makes each job process the leader
// of a group of its own; one that isn't kills only itself.
func killGroup() {
	if pid := os.Getpid(); unix.Getpgrp() == pid {
		_ = unix.Kill(-pid, unix.SIGKILL)
	}
	killSelf()
}

// killSelf kills the VM child, however stuck its libkrun threads are.
func killSelf() { _ = unix.Kill(os.Getpid(), unix.SIGKILL) }

// Run is the job process: it reads the job spec from in, starts the job
// network and attaches every VM to it, reports NetworkUp, runs each VM in a
// VM child of its own, all at once, and writes status events to out until
// every VM child has exited (runVMs). A VM never boots before its switch
// serves its socket. Cancelling ctx kills every VM child. live is the
// liveness pipe, which each VM child gets as its fd 3.
//
// Every child, each VM child and mkfs, is forked with Pdeathsig SIGKILL
// from a locked OS thread of Run's own, which lives until Run returns, by
// when every child has been waited for.
//
// log writes the job process log, at level, which Run sets to the spec's
// log level before it logs anything else.
func Run(ctx context.Context, in io.Reader, out io.Writer, live *os.File, log *slog.Logger, level *slog.LevelVar) error {
	th := startThread()
	defer th.stop()
	return run(ctx, in, out, live, th, log, level)
}

// run is Run, forking on th.
func run(ctx context.Context, in io.Reader, out io.Writer, live *os.File, th *thread, log *slog.Logger, level *slog.LevelVar) error {
	spec := &jobv1.Spec{}
	if err := (protodelim.UnmarshalOptions{MaxSize: maxSpec}).UnmarshalFrom(bufio.NewReader(in), spec); err != nil {
		return failBug(out, fmt.Errorf("read the job spec: %w", err))
	}
	if n := len(spec.GetVms()); n < 1 || n > maxVMs {
		return failBug(out, fmt.Errorf("the job spec has %d VMs; a job has 1 to %d, the job's container first", n, maxVMs))
	}
	// The plugin always sets it: any other value is its bug.
	if err := logging.SetLevel(level, spec.GetLogLevel()); err != nil {
		return failBug(out, fmt.Errorf("the job spec: %w", err))
	}
	log = log.With("env", spec.GetJob())
	log.Debug("read the job spec", "vms", len(spec.GetVms()), "subnet", spec.GetNetwork().GetSubnet(),
		"lib_dir", spec.GetLibDir(), "libkrun_log_level", spec.GetLibkrunLogLevel(), "log_level", spec.GetLogLevel())
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
	return runVMs(ctx, spec, links, live, th, out, log)
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
	for i, v := range spec.GetVms() {
		l, err := n.Attach(cfg.VMs[i].MAC)
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

// preparedVM is one of the job's VMs ready for its VM child: its mounts
// checked, its disks open and locked, and its scratch disk made.
type preparedVM struct {
	index int
	vm    *jobv1.VM
	// link is its VM's end of its frame socket.
	link *os.File
	// disks[i] is vm.Disks[i]'s file.
	disks []*os.File
}

// vmExit is how one VM child ended: its exit code, 128+n when signal n
// ended it, or the error its wait returned.
type vmExit struct {
	index int
	code  int32
	err   error
}

// runVMs runs the job's VMs, links[i] being VM i's end of its frame socket:
// it prepares each VM in the spec's order, then starts every VM child, each
// on th with the liveness pipe live as its fd 3 and its link as its fd 4,
// reporting each one's VMStarted, and reports each one's VMExited as it
// exits, until every VM child has exited and been waited for.
//
// A VM other than the job's container exiting doesn't end the job. The
// job's container exiting does: runVMs kills the other VM children, reports
// each one's VMExited once it is reaped, and returns. A VM that can't be
// prepared fails the job before any VM child starts; one whose VM child
// can't start fails it once the VM children already started are killed and
// reaped, with no VMExited for them, so Failed is the plugin's first word on
// why. A send nobody reads kills every VM child too.
//
// Failed's reason names the cause first, as Start's error shows it
// (PROTO-10): a mount or a disk, by its source, the scratch disk, whose path
// names its VM's directory, or the VM.
func runVMs(ctx context.Context, spec *jobv1.Spec, links []*os.File, live *os.File, th *thread, out io.Writer, log *slog.Logger) error {
	vms, err := prepareVMs(ctx, spec, links, th)
	if err != nil {
		return fail(out, err)
	}
	// The job process holds each VM's disks until its VM child has exited.
	// Closing a VM's files again does nothing.
	defer func() {
		for _, v := range vms {
			closeAll(v.disks)
		}
	}()
	exits := make(chan vmExit, len(vms))
	cmds := make([]*exec.Cmd, len(vms))
	running := 0
	var startErr, sendErr error
	for i, v := range vms {
		cmd, err := startVM(ctx, spec, v, live, th, log)
		if err != nil {
			startErr = fmt.Errorf("VM %s: %w", v.vm.GetName(), err)
			break
		}
		cmds[i] = cmd
		running++
		go func() { exits <- waitVM(i, cmd) }()
		err = send(out, &jobv1.Event{Event: &jobv1.Event_VmStarted{VmStarted: &jobv1.VMStarted{
			Vm: uint32(i), Pid: int32(cmd.Process.Pid), //nolint:gosec // G115: an index is below maxVMs, and a pid fits in an int32
		}}})
		if err != nil {
			// Nobody reads the control channel: the job is of no use.
			sendErr = err
			break
		}
	}
	if startErr != nil || sendErr != nil {
		killAll(cmds)
		for ; running > 0; running-- {
			closeAll(vms[(<-exits).index].disks)
		}
		if startErr != nil {
			return fail(out, startErr)
		}
		return sendErr
	}
	var waitErr error
	for ; running > 0; running-- {
		x := <-exits
		closeAll(vms[x.index].disks)
		v := vms[x.index].vm
		switch {
		case waitErr != nil || sendErr != nil:
			// The job is ending: the rest are only reaped.
			continue
		case x.err != nil:
			waitErr = fmt.Errorf("VM %s: %w", v.GetName(), x.err)
			killAll(cmds)
			continue
		}
		log.Info("VM exited", "vm", v.GetName(), "index", x.index, "exit_code", x.code)
		err := send(out, &jobv1.Event{Event: &jobv1.Event_VmExited{VmExited: &jobv1.VMExited{
			Vm:          uint32(x.index), //nolint:gosec // G115: an index is below maxVMs
			ExitCode:    x.code,
			ConsoleTail: tail(v.GetConsole()),
			LogTail:     tail(v.GetVmLog()),
		}}})
		if err != nil {
			sendErr = err
			killAll(cmds)
			continue
		}
		if x.index == 0 && running > 1 {
			log.Info("the job's container exited; stopping the job's other VMs", "vms", running-1)
			killAll(cmds)
		}
	}
	if waitErr != nil {
		return fail(out, waitErr)
	}
	return sendErr
}

// prepareVMs prepares each of the spec's VMs, in its order, links[i] being
// VM i's end of its frame socket: it checks the VM's mounts, opens and locks
// its disks, and makes its scratch disk. On error it closes every disk it
// opened, which releases their locks.
func prepareVMs(ctx context.Context, spec *jobv1.Spec, links []*os.File, th *thread) (vms []*preparedVM, err error) {
	defer func() {
		if err != nil {
			for _, v := range vms {
				closeAll(v.disks)
			}
			vms = nil
		}
	}()
	for i, v := range spec.GetVms() {
		if err := checkMounts(v.GetMounts()); err != nil {
			return vms, err
		}
		disks, err := openDisks(v.GetDisks())
		if err != nil {
			return vms, err
		}
		vms = append(vms, &preparedVM{index: i, vm: v, link: links[i], disks: disks})
		if err := createScratchDisk(ctx, th, spec.GetMkfsExt4(), v.GetScratchDisk(), v.GetScratchSizeBytes()); err != nil {
			return vms, fmt.Errorf("scratch disk: %s: %w", v.GetScratchDisk(), err)
		}
	}
	return vms, nil
}

// killAll kills each VM child that has started. One that has exited
// already is left alone.
func killAll(cmds []*exec.Cmd) {
	for _, cmd := range cmds {
		if cmd != nil {
			_ = cmd.Process.Kill()
		}
	}
}

// waitVM waits for VM index's VM child, cmd, to exit.
func waitVM(index int, cmd *exec.Cmd) vmExit {
	err := cmd.Wait()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return vmExit{index: index, err: err}
	}
	ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if ws.Signaled() {
		return vmExit{index: index, code: 128 + int32(ws.Signal())} //nolint:gosec // G115: a signal number is below 65
	}
	return vmExit{index: index, code: int32(ws.ExitStatus())} //nolint:gosec // G115: an exit status is 0 to 255
}

// startVM starts v's VM child on th, with the liveness pipe live as its
// fd 3, its link as its fd 4 and its disks as fd 5 and up, and returns it
// running. Once it has started, only the VM child holds v's link.
func startVM(ctx context.Context, spec *jobv1.Spec, v *preparedVM, live *os.File, th *thread, log *slog.Logger) (*exec.Cmd, error) {
	vm := v.vm
	childSpec, err := specFile(&vmv1.Spec{
		LibDir:          spec.GetLibDir(),
		LibkrunLogLevel: spec.GetLibkrunLogLevel(),
		LogLevel:        spec.GetLogLevel(),
		Cpus:            vm.GetCpus(),
		MemoryMib:       vm.GetMemoryMib(),
		RootDir:         vm.GetRootDir(),
		ScratchDisk:     vm.GetScratchDisk(),
		LayerDir:        vm.GetLayerDir(),
		LayerTag:        vm.GetLayerTag(),
		Mounts:          childMounts(vm.GetMounts()),
		Disks:           childDisks(vm.GetDisks()),
		Console:         vm.GetConsole(),
		AgentSocket:     vm.GetAgentSocket(),
		AgentPort:       vm.GetAgentPort(),
		GuestMac:        vm.GetGuestMac(),
		Boot:            vm.GetBoot(),
	})
	if err != nil {
		return nil, fmt.Errorf("VM child spec: %w", err)
	}
	// The VM child has its own copies once it has started.
	defer func() { _ = childSpec.Close() }()
	vmLog, err := os.OpenFile(vm.GetVmLog(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("VM log: %w", err)
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
	// fd 3 is the liveness pipe, fd 4 its VM's end of the frame socket,
	// and fd 5 and up its disks, in order.
	cmd.ExtraFiles = append([]*os.File{live, v.link}, v.disks...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := th.do(ctx, cmd.Start); err != nil {
		return nil, fmt.Errorf("start the VM child: %w", err)
	}
	// Only the VM child holds the VM's end now, so once it exits the
	// switch's writes to it fail instead of filling its buffer.
	_ = v.link.Close()
	log.Info("VM started", "vm", vm.GetName(), "index", v.index, "pid", cmd.Process.Pid)
	return cmd, nil
}

// checkMounts checks each mount's source again, before the VM child starts:
// the config checked it when it was read, but a source can disappear since,
// and libkrun would report one missing only as -EINVAL at boot. A source
// that is gone or no longer a directory is an error naming it (STO-5,
// PROTO-10).
func checkMounts(mounts []*jobv1.Mount) error {
	for _, m := range mounts {
		fi, err := os.Stat(m.GetSource())
		var pe *fs.PathError
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("mount %s: does not exist", m.GetSource())
		case errors.As(err, &pe):
			return fmt.Errorf("mount %s: %w", m.GetSource(), pe.Err)
		case err != nil:
			return fmt.Errorf("mount %s: %w", m.GetSource(), err)
		case !fi.IsDir():
			return fmt.Errorf("mount %s: is not a directory", m.GetSource())
		}
	}
	return nil
}

// openDisks opens each disk's source, read-only for a read-only disk, and
// takes its disk lock, for the VM child to inherit, and returns the files in
// disks' order. A source that is missing or not a regular file is an error
// naming it, and so is a disk whose lock another process holds; either
// closes the files already open, which releases their locks (STO-5, STO-7,
// STO-8, PROTO-10). The config checked each source when it was read, but a
// source can change since; libkrun is handed the open file, so the file
// checked, and locked, is the file attached. The open never blocks, so a
// FIFO put at the source can't hang the job, and nor does the lock: a disk
// in use fails at once, so the order the disks are locked in can't deadlock
// two jobs. The plugin's disk queues admit its own jobs to a disk only once
// every conflicting holder's job process group is reaped, so only a holder
// outside the plugin makes a disk in use here.
//
// Each file is close-on-exec, so mkfs never holds a lock; only the VM
// child, which gets the files as fd 5 and up, does. The lock belongs to
// the open file, which the job process and its VM child then share, so it
// lasts until both have closed it: as long as any process that can use the
// disk lives.
func openDisks(disks []*jobv1.Disk) (files []*os.File, err error) {
	defer func() {
		if err != nil {
			closeAll(files)
			files = nil
		}
	}()
	for _, d := range disks {
		flag := os.O_RDONLY
		if d.GetReadWrite() {
			flag = os.O_RDWR
		}
		f, err := os.OpenFile(d.GetSource(), flag|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
		var pe *fs.PathError
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return files, fmt.Errorf("disk %s: does not exist", d.GetSource())
		case errors.As(err, &pe):
			return files, fmt.Errorf("disk %s: %w", d.GetSource(), pe.Err)
		case err != nil:
			return files, fmt.Errorf("disk %s: %w", d.GetSource(), err)
		}
		files = append(files, f)
		fi, err := f.Stat()
		if err != nil {
			return files, fmt.Errorf("disk %s: %w", d.GetSource(), err)
		}
		if !fi.Mode().IsRegular() {
			return files, fmt.Errorf("disk %s: is not a regular file", d.GetSource())
		}
		if err := lockDisk(f, d.GetReadWrite()); err != nil {
			return files, err
		}
	}
	return files, nil
}

// lockDisk takes the disk lock on f, a disk's open source: an exclusive
// flock for a read-write disk, a shared one for a read-only disk, never
// waiting. A lock another open file holds, in this process or another, is
// `disk <source> is in use by another process`, whichever lock this job
// wanted (STO-7, STO-8).
func lockDisk(f *os.File, readWrite bool) error {
	how := unix.LOCK_SH
	if readWrite {
		how = unix.LOCK_EX
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return fmt.Errorf("disk %s: lock: %w", f.Name(), err)
	}
	var lockErr error
	err = rc.Control(func(fd uintptr) {
		for {
			lockErr = unix.Flock(int(fd), how|unix.LOCK_NB)
			if !errors.Is(lockErr, unix.EINTR) {
				return
			}
		}
	})
	switch {
	case err != nil:
		return fmt.Errorf("disk %s: lock: %w", f.Name(), err)
	case errors.Is(lockErr, unix.EWOULDBLOCK):
		return fmt.Errorf("disk %s is in use by another process", f.Name())
	case lockErr != nil:
		return fmt.Errorf("disk %s: lock: %w", f.Name(), lockErr)
	}
	return nil
}

// closeAll closes files.
func closeAll(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// childDisks are the VM child's copies of the job spec's disks.
func childDisks(disks []*jobv1.Disk) []*vmv1.Disk {
	var out []*vmv1.Disk
	for _, d := range disks {
		out = append(out, &vmv1.Disk{Source: d.GetSource(), BlockId: d.GetBlockId(), ReadWrite: d.GetReadWrite()})
	}
	return out
}

// childMounts are the VM child's copies of the job spec's mounts.
func childMounts(mounts []*jobv1.Mount) []*vmv1.Mount {
	var out []*vmv1.Mount
	for _, m := range mounts {
		out = append(out, &vmv1.Mount{Source: m.GetSource(), Tag: m.GetTag(), ReadWrite: m.GetReadWrite()})
	}
	return out
}

// mkfsArgs format a scratch disk quickly: the inode tables and journal are
// initialised lazily by the guest, so formatting takes about 50 ms and
// allocates about 4 MB, and the root directory is root's.
var mkfsArgs = []string{"-q", "-F", "-E", "lazy_itable_init=1,lazy_journal_init=1,root_owner=0:0"}

// createScratchDisk creates path as a sparse file of size bytes, mode 0600,
// and formats it as ext4 with mkfs. The file must not exist: a scratch disk
// is never reused, so nothing written in one job is visible in the next
// (VM-5). A disk that can't be made is deleted.
func createScratchDisk(ctx context.Context, th *thread, mkfs, path string, size uint64) (err error) {
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
	// mkfs is in the job process's group, so killing the job kills it, and
	// it dies with the job process too.
	cmd := exec.CommandContext(ctx, mkfs, append(mkfsArgs, path)...) //nolint:gosec // G204: the mkfs.ext4 the plugin found at startup
	cmd.WaitDelay = time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = th.do(ctx, cmd.Start)
	if err == nil {
		err = cmd.Wait()
	}
	if err != nil {
		return fmt.Errorf("%s: %w: %s", mkfs, err, strings.TrimSpace(out.String()))
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

// tail returns the end of the file at path, at most tailBytes, less a
// character the cut splits or any other invalid UTF-8, which a proto string
// can't carry; a file that can't be read has an empty tail.
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
	return strings.ToValidUTF8(string(b), "")
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

// failBug is fail for a failure only a bug in the plugin can cause, a job
// spec it can't read or that doesn't describe a job: its Failed event says
// so, and Start reports it as Internal.
func failBug(out io.Writer, err error) error {
	return errors.Join(err, send(out, &jobv1.Event{Event: &jobv1.Event_Failed{Failed: &jobv1.Failed{Reason: err.Error(), Bug: true}}}))
}

// errThreadGone is do's error once its thread has exited.
var errThreadGone = errors.New("the job process's forking thread is gone")

// thread is a goroutine locked to an OS thread of its own, which forks every
// child of the job process. Pdeathsig fires when the thread that forked a
// child exits, not when its process does, and a goroutine that isn't locked
// may fork on a thread the runtime ends at any time.
type thread struct {
	calls chan func()
	// tid is the thread's id, never the main thread's.
	tid   int
	ready chan struct{}
	quit  chan struct{}
	done  chan struct{}
	once  sync.Once
}

func startThread() *thread {
	t := &thread{calls: make(chan func()), ready: make(chan struct{}), quit: make(chan struct{}), done: make(chan struct{})}
	go t.serve()
	<-t.ready
	return t
}

func (t *thread) serve() {
	runtime.LockOSThread()
	if unix.Gettid() == unix.Getpid() {
		// The main thread never exits while the process runs: the runtime
		// parks it instead. Hand the work to another thread, which the
		// new goroutine must be while this one holds the main thread.
		go t.serve()
		<-t.ready
		runtime.UnlockOSThread()
		return
	}
	defer close(t.done)
	t.tid = unix.Gettid()
	close(t.ready)
	// Never unlocked: when serve returns, the runtime ends the thread
	// rather than hand it to another goroutine, so a child's Pdeathsig
	// fires only when its thread is truly gone.
	for {
		select {
		case f := <-t.calls:
			f()
		case <-t.quit:
			return
		}
	}
}

// do runs f on the thread and returns its error, or ctx's error if the
// thread is busy until ctx ends.
func (t *thread) do(ctx context.Context, f func() error) error {
	errc := make(chan error, 1)
	select {
	case t.calls <- func() { errc <- f() }:
		return <-errc
	case <-t.done:
		return errThreadGone
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stop ends the thread, which fires the Pdeathsig of every child it forked.
func (t *thread) stop() {
	t.once.Do(func() { close(t.quit) })
	<-t.done
}
