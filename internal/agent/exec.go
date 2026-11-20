// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// defaultPath is the PATH a command gets when its environment sets none
// (GST-4).
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// outputGrace is how long Exec waits for the end of the output once the
// command exited and what it wrote is sent, so a background child holding
// the output open can't keep the step running (GST-9).
const outputGrace = 200 * time.Millisecond

// Exec runs one command and streams its output, then its exit code, or why
// it could not start. Cancelling the call kills the command's process group
// at once (GST-8).
//
// SHORTCUT(#6): the agent does only what the echo workflow needs: no image
// environment underneath the request's, no users, and a missing working
// directory is created owned by root.
func (s *server) Exec(req *agentv1.ExecRequest, stream agentv1.AgentService_ExecServer) error {
	startError := func(format string, args ...any) error {
		return stream.Send(&agentv1.ExecResponse{Event: &agentv1.ExecResponse_StartError{StartError: fmt.Sprintf(format, args...)}})
	}
	argv := req.GetArgv()
	if len(argv) == 0 {
		return startError("no command")
	}
	if u := req.GetUser(); u != "" && u != "root" && u != "0" && u != "0:0" {
		return startError("user %q: the agent runs commands as root only, for now", u)
	}
	env := environ(req.GetEnv())
	path, err := lookPath(argv[0], env)
	if err != nil {
		return startError("%v", err)
	}
	dir := req.GetWorkingDir()
	if dir == "" {
		dir = "/"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: a working directory every user may enter
		return startError("working directory: %v", err)
	}

	// Cancelled with the call, or when the output can't be sent.
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	cmd := exec.CommandContext(ctx, path, argv[1:]...) //nolint:gosec // G204: running the job's command is the agent's purpose
	cmd.Args[0] = argv[0]
	cmd.Env = env
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// The output pipes are the agent's own, not exec's, so Wait doesn't
	// close them: the output is read to its end below.
	stdout, err := newOutput()
	if err != nil {
		return err
	}
	stderr, err := newOutput()
	if err != nil {
		stdout.close()
		return err
	}
	defer stdout.close()
	defer stderr.close()
	cmd.Stdout, cmd.Stderr = stdout.w, stderr.w
	if err := cmd.Start(); err != nil {
		return startError("%v", err)
	}
	stdout.closeWriter()
	stderr.closeWriter()

	var mu sync.Mutex
	send := func(wrap func([]byte) *agentv1.ExecResponse) func([]byte) error {
		return func(b []byte) error {
			mu.Lock()
			defer mu.Unlock()
			// The message may be read after Send returns, and b is reused.
			return stream.Send(wrap(append([]byte(nil), b...)))
		}
	}
	relayed := make(chan error, 2)
	for _, o := range []struct {
		out  *output
		wrap func([]byte) *agentv1.ExecResponse
	}{
		{stdout, func(b []byte) *agentv1.ExecResponse {
			return &agentv1.ExecResponse{Event: &agentv1.ExecResponse_Stdout{Stdout: b}}
		}},
		{stderr, func(b []byte) *agentv1.ExecResponse {
			return &agentv1.ExecResponse{Event: &agentv1.ExecResponse_Stderr{Stderr: b}}
		}},
	} {
		go func() {
			err := o.out.relay(send(o.wrap))
			if err != nil {
				// Nobody gets the output: the command is of no use.
				cancel()
			}
			relayed <- err
		}()
	}
	waitErr := cmd.Wait()
	// The command has exited: everything it wrote is in the pipes, or
	// already sent.
	stdout.exited()
	stderr.exited()
	if s.execExited != nil {
		s.execExited()
	}
	err = errors.Join(<-relayed, <-relayed)
	if cerr := stream.Context().Err(); cerr != nil {
		return cerr
	}
	if err != nil {
		return err
	}
	var exit *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exit) {
		return waitErr
	}
	code := int32(cmd.ProcessState.ExitCode()) //nolint:gosec // G115: an exit status is -1 to 255
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		code = 128 + int32(ws.Signal()) //nolint:gosec // G115: a signal number is below 65
	}
	return stream.Send(&agentv1.ExecResponse{Event: &agentv1.ExecResponse_ExitCode{ExitCode: code}})
}

// output is one of a command's output pipes, read by the agent.
type output struct {
	r, w *os.File
}

func newOutput() (*output, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	return &output{r: r, w: w}, nil
}

// closeWriter closes the agent's copy of the write end, once the command
// has its own.
func (o *output) closeWriter() { _ = o.w.Close() }

func (o *output) close() {
	_ = o.r.Close()
	_ = o.w.Close()
}

// exited tells relay that the command has exited, by ending its current or
// next read with a deadline that has passed. It is called once.
func (o *output) exited() { _ = o.r.SetReadDeadline(time.Unix(1, 0)) }

// relay sends the output, chunk by chunk, until its end. Once the command
// has exited (exited), it still sends everything the command wrote before
// it exited, however slowly send takes it; after that it waits at most
// outputGrace for the end, which a background child holding the pipe open
// delays (GST-9).
func (o *output) relay(send func([]byte) error) error {
	buf := make([]byte, 32<<10)
	draining := false
	// owed is how much of what was in the pipe when the command exited is
	// not read yet.
	owed := 0
	graceSet := false
	for {
		n, err := o.r.Read(buf)
		if n > 0 {
			owed -= n
			if err := send(buf[:n]); err != nil {
				return err
			}
		}
		switch {
		case err == nil:
		case errors.Is(err, io.EOF):
			return nil
		case errors.Is(err, os.ErrDeadlineExceeded) && !draining:
			// The command has exited: the pipe holds the rest of what it
			// wrote, read without a deadline.
			draining = true
			if owed, err = o.buffered(); err != nil {
				return err
			}
			if err := o.r.SetReadDeadline(time.Time{}); err != nil {
				return err
			}
		case errors.Is(err, os.ErrDeadlineExceeded):
			// A background child still holds the pipe open.
			return nil
		default:
			return err
		}
		if draining && owed <= 0 && !graceSet {
			graceSet = true
			if err := o.r.SetReadDeadline(time.Now().Add(outputGrace)); err != nil {
				return err
			}
		}
	}
}

// buffered returns how many bytes the pipe holds unread.
func (o *output) buffered() (int, error) {
	rc, err := o.r.SyscallConn()
	if err != nil {
		return 0, err
	}
	var n int32
	var ioctlErr error
	err = rc.Control(func(fd uintptr) {
		// TIOCINQ is FIONREAD, which the syscall package doesn't name.
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCINQ, uintptr(unsafe.Pointer(&n))) //nolint:gosec // G103: ioctl writes its result through a pointer
		if errno != 0 {
			ioctlErr = os.NewSyscallError("ioctl FIONREAD", errno)
		}
	})
	return int(n), errors.Join(err, ioctlErr)
}

// environ returns env as KEY=VALUE, sorted, with the default PATH when env
// sets none.
func environ(env map[string]string) []string {
	out := make([]string, 0, len(env)+1)
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	if _, ok := env["PATH"]; !ok {
		out = append(out, "PATH="+defaultPath)
	}
	sort.Strings(out)
	return out
}

// lookPath finds name in env's PATH, as the command's own environment
// would, not the agent's (GST-4).
func lookPath(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	pathVar := defaultPath
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathVar = v
		}
	}
	for _, dir := range filepath.SplitList(pathVar) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: not found in PATH %s", name, pathVar)
}
