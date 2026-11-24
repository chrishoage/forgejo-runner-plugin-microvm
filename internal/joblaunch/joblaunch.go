// SPDX-License-Identifier: GPL-3.0-or-later

// Package joblaunch is the plugin's side of a job process: it starts the job
// binary with the liveness pipe and Pdeathsig, writes the job spec to its
// control channel, reads its status events and kills it. The plugin reaches
// it through its JobLauncher seam.
package joblaunch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"google.golang.org/protobuf/encoding/protodelim"

	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
)

// Process is the plugin's handle on one running job process.
//
// The caller may stop reading Events at any time, for example after the job's
// container's VMStarted, once it dials that VM's agent to learn when it
// answers Ping; today the job's container is the only VM, because the plugin
// protocol passes no VM options for services. The launcher
// reads the job process's stdout to its end whether or not anyone reads
// Events, so Exited and Kill never wait on the caller.
type Process struct {
	// Events delivers the job process's status events in order. It is
	// buffered for every event the control channel allows for the spec (at
	// most one NetworkUp and one Failed, and one VMStarted and one VMExited
	// for each of its VMs), so the launcher never blocks sending one, and an
	// unread VMExited stays in the buffer until the caller reads it. An event
	// that doesn't fit, which only a job process breaking the protocol can
	// send, is logged and dropped. Events is closed when the job process's
	// stdout ends.
	Events <-chan *jobv1.Event
	// Exited is closed once the job process and every other process in its
	// group, its VM children included, have exited and been reaped, whether
	// or not Events has been read. The launcher kills the group when the job
	// process exits, so a VM child it leaves behind never outlives Exited.
	Exited <-chan struct{}

	// pid is the job process's, and its process group's, id.
	pid int
	// mu orders Kill against the reaper: once exited is set, the job process
	// may be reaped and its group id reused, so nothing signals the group
	// again.
	mu     sync.Mutex
	exited bool
}

// maxEvent bounds one status event on the control channel.
const maxEvent = 1 << 20

// Launcher starts job processes. It is the plugin's JobLauncher.
//
// Every job process holds the read end of the launcher's liveness pipe as its
// fd 3, and only the launcher holds the write end, close-on-exec, so no child
// of the plugin inherits it. The pipe reads end of file once the plugin is
// gone, however it died, and the job process then kills its group. Each job
// process also gets Pdeathsig SIGKILL, the backstop for a job process too
// stuck to read the pipe, forked from the launcher's own locked OS thread.
type Launcher struct {
	log *slog.Logger
	// liveR is what each job process gets as fd 3; liveW is never written,
	// and is closed only by Close or when the plugin exits.
	liveR, liveW *os.File
	thread       *thread
}

// New returns a launcher, with its liveness pipe and its forking thread. Both
// last until Close, which the plugin calls as it exits.
func New(log *slog.Logger) (*Launcher, error) {
	liveR, liveW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("liveness pipe: %w", err)
	}
	return &Launcher{log: log, liveR: liveR, liveW: liveW, thread: startThread()}, nil
}

// Close ends the launcher's forking thread and closes both ends of its
// liveness pipe, so a job process still running dies as it would with the
// plugin. A closed launcher launches nothing. Closing it again does nothing.
func (l *Launcher) Close() {
	// Every Launch forks on the thread, so once it is gone no Launch can
	// still be handing liveR to a job process.
	l.thread.stop()
	_ = l.liveR.Close()
	_ = l.liveW.Close()
}

// SetSubreaper makes the calling process a child subreaper, so a VM child
// whose job process is gone is reparented to it, and the launcher can reap
// it before Exited closes. The plugin calls it once, at startup.
func SetSubreaper() error {
	const prSetChildSubreaper = 36
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		return fmt.Errorf("PR_SET_CHILD_SUBREAPER: %w", errno)
	}
	return nil
}

// Launch starts spec's job binary as a job process leading its own process
// group, with stderr on the job process log at logPath, and writes spec to
// its control channel. Its fd 3 is the liveness pipe, and it gets Pdeathsig
// SIGKILL from the launcher's thread. ctx bounds the launch only. On error,
// every process it started has been killed and reaped.
func (l *Launcher) Launch(ctx context.Context, spec *jobv1.Spec, logPath string) (*Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // G304: the plugin names the job's log file
	if err != nil {
		return nil, fmt.Errorf("job process log: %w", err)
	}
	defer func() { _ = logFile.Close() }()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() { _ = stdinW.Close() }()
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		return nil, err
	}
	var proc *os.Process
	err = l.thread.do(ctx, func() error {
		var err error
		proc, err = os.StartProcess(spec.GetJobBinary(), []string{spec.GetJobBinary()}, &os.ProcAttr{
			Files: []*os.File{stdinR, stdoutW, logFile, l.liveR},
			Sys:   &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL},
		})
		return err
	})
	// The child holds its own ends now.
	_ = stdinR.Close()
	_ = stdoutW.Close()
	if err != nil {
		_ = stdoutR.Close()
		return nil, fmt.Errorf("start the job process: %w", err)
	}
	// The reaper waits by process group, not through proc. Release
	// clears proc.Pid, so it is read first.
	pid := proc.Pid
	_ = proc.Release()
	if pid <= 1 {
		return nil, fmt.Errorf("start the job process: pid %d", pid)
	}

	events := make(chan *jobv1.Event, 2+2*len(spec.GetVms()))
	exited := make(chan struct{})
	p := &Process{Events: events, Exited: exited, pid: pid}
	log := l.log.With("env", spec.GetJob(), "pid", p.pid)
	go readEvents(stdoutR, events, log)
	go p.reap(exited)

	_, err = protodelim.MarshalTo(stdinW, spec)
	if err == nil {
		err = stdinW.Close()
	}
	if err != nil {
		_ = l.Kill(context.WithoutCancel(ctx), p)
		return nil, fmt.Errorf("write the job spec: %w", err)
	}
	log.Info("job process started")
	return p, nil
}

// Kill sends SIGKILL to the job process's group, and returns once every
// process in it has exited and been reaped, or with ctx's error. A job
// process that is already gone is not an error.
func (l *Launcher) Kill(ctx context.Context, p *Process) error {
	p.mu.Lock()
	if !p.exited {
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
	}
	p.mu.Unlock()
	select {
	case <-p.Exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readEvents delivers the control channel's events until it ends, then
// drains it, so the job process never blocks writing to it.
func readEvents(r *os.File, events chan<- *jobv1.Event, log *slog.Logger) {
	defer func() { _ = r.Close() }()
	defer close(events)
	br := bufio.NewReader(r)
	for {
		ev := &jobv1.Event{}
		err := (protodelim.UnmarshalOptions{MaxSize: maxEvent}).UnmarshalFrom(br, ev)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			log.Error("control channel", "err", err)
			_, _ = io.Copy(io.Discard, br)
			return
		}
		select {
		case events <- ev:
		default:
			log.Error("control channel: event beyond the protocol dropped", "event", ev)
		}
	}
}

// reap waits for the job process to exit, without reaping it so its group
// id can't be reused, kills the rest of its group, then reaps every process
// in the group, and closes exited.
func (p *Process) reap(exited chan<- struct{}) {
	defer close(exited)
	waitExitNoReap(p.pid)
	p.mu.Lock()
	p.exited = true
	_ = syscall.Kill(-p.pid, syscall.SIGKILL)
	p.mu.Unlock()
	for {
		var ws syscall.WaitStatus
		_, err := syscall.Wait4(-p.pid, &ws, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			// ECHILD: no process of the group is left.
			return
		}
	}
}

// waitExitNoReap blocks until pid has exited, leaving it a zombie.
func waitExitNoReap(pid int) {
	const (
		pPID    = 1
		wExited = 4
		wNoWait = 0x1000000
	)
	var info [128]byte // siginfo_t
	for {
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid),
			uintptr(unsafe.Pointer(&info)), wExited|wNoWait, 0, 0) //nolint:gosec // G103: waitid(2) fills the siginfo_t buffer
		if errno != syscall.EINTR {
			return
		}
	}
}

// errThreadGone is do's error once its thread has exited.
var errThreadGone = errors.New("the launcher's forking thread is gone")

// thread is a goroutine locked to an OS thread of its own, which forks every
// job process. Pdeathsig fires when the thread that forked a child exits, not
// when its process does, and a goroutine that isn't locked may fork on a
// thread the runtime ends at any time. This thread lives until the
// launcher's Close, as long as the plugin.
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
	if syscall.Gettid() == syscall.Getpid() {
		// The main thread never exits while the process runs: the runtime
		// parks it instead. Hand the work to another thread, which the
		// new goroutine must be while this one holds the main thread.
		go t.serve()
		<-t.ready
		runtime.UnlockOSThread()
		return
	}
	defer close(t.done)
	t.tid = syscall.Gettid()
	close(t.ready)
	// Never unlocked: when serve returns, the runtime ends the thread
	// rather than hand it to another goroutine, so a job process's
	// Pdeathsig fires only when its thread is truly gone.
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

// stop ends the thread, which fires the Pdeathsig of every job process it
// forked.
func (t *thread) stop() {
	t.once.Do(func() { close(t.quit) })
	<-t.done
}
