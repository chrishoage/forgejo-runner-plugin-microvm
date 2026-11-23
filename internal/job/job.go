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
	"bytes"
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
// VM child, with `version` its version. It returns the exit code.
func Main(args []string) int {
	switch {
	case len(args) == 1 && args[0] == "version":
		fmt.Println(Version)
		return 0
	case len(args) == 1 && args[0] == "vm":
		log := logging.Setup("runner-microvm-vm")
		live, err := liveness()
		if err != nil {
			log.Error("VM child", "err", err)
			return 1
		}
		go dieWithPlugin(live, log, killSelf)
		return vm.Main(os.Stdin, log)
	case len(args) == 0:
		log := logging.Setup("runner-microvm-job")
		live, err := liveness()
		if err != nil {
			log.Error("job process", "err", err)
			return 1
		}
		go dieWithPlugin(live, log, killGroup)
		if err := Run(context.Background(), os.Stdin, os.Stdout, live, log); err != nil {
			log.Error("job process", "err", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "usage: runner-microvm-job [vm | version]\n")
		return 2
	}
}

// The most of a console or VM log file a VMExited carries: what the
// plugin's error for the VM's exit keeps, its last 20 lines, is at most
// this much too (PROTO-10, VM-8).
const tailBytes = 4 << 10

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
// network and attaches every VM to it, reports NetworkUp, runs its VM in a
// VM child, and writes status events to out until the VM child exits. A VM
// never boots before its switch serves its socket. Cancelling ctx kills the
// VM child. live is the liveness pipe, which each VM child gets as its fd 3.
//
// Every child, each VM child and mkfs, is forked with Pdeathsig SIGKILL
// from a locked OS thread of Run's own, which lives until Run returns, by
// when every child has been waited for.
func Run(ctx context.Context, in io.Reader, out io.Writer, live *os.File, log *slog.Logger) error {
	th := startThread()
	defer th.stop()
	return run(ctx, in, out, live, th, log)
}

// run is Run, forking on th.
func run(ctx context.Context, in io.Reader, out io.Writer, live *os.File, th *thread, log *slog.Logger) error {
	spec := &jobv1.Spec{}
	if err := (protodelim.UnmarshalOptions{MaxSize: maxSpec}).UnmarshalFrom(bufio.NewReader(in), spec); err != nil {
		return failBug(out, fmt.Errorf("read the job spec: %w", err))
	}
	if n := len(spec.GetVms()); n != 1 {
		return failBug(out, fmt.Errorf("the job spec has %d VMs; a job has exactly one, the job's container", n))
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
	code, err := runVM(ctx, spec, v, links[0], live, th, out, log)
	if err != nil {
		return fail(out, err)
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

// runVM starts VM 0's VM child on th, with the liveness pipe live as its
// fd 3 and link, its VM's end of its frame socket, as its fd 4, reports
// VMStarted, and waits for it to exit. It returns the VM child's exit code,
// 128+n when signal n ended it.
//
// Its error names the cause first, as Start's error shows it (PROTO-10):
// the scratch disk, whose path names its VM's directory, or the VM.
func runVM(ctx context.Context, spec *jobv1.Spec, v *jobv1.VM, link, live *os.File, th *thread, out io.Writer, log *slog.Logger) (int32, error) {
	if err := createScratchDisk(ctx, th, spec.GetMkfsExt4(), v.GetScratchDisk(), v.GetScratchSizeBytes()); err != nil {
		return 0, fmt.Errorf("scratch disk: %s: %w", v.GetScratchDisk(), err)
	}
	code, err := startVM(ctx, spec, v, link, live, th, out, log)
	if err != nil {
		return 0, fmt.Errorf("VM %s: %w", v.GetName(), err)
	}
	return code, nil
}

// startVM is runVM once the VM's scratch disk is made.
func startVM(ctx context.Context, spec *jobv1.Spec, v *jobv1.VM, link, live *os.File, th *thread, out io.Writer, log *slog.Logger) (int32, error) {
	childSpec, err := specFile(&vmv1.Spec{
		LibDir:      spec.GetLibDir(),
		Cpus:        v.GetCpus(),
		MemoryMib:   v.GetMemoryMib(),
		RootDir:     v.GetRootDir(),
		ScratchDisk: v.GetScratchDisk(),
		LayerDir:    v.GetLayerDir(),
		LayerTag:    v.GetLayerTag(),
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
	// fd 3 is the liveness pipe, and fd 4 its VM's end of the frame
	// socket.
	cmd.ExtraFiles = []*os.File{live, link}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := th.do(ctx, cmd.Start); err != nil {
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
