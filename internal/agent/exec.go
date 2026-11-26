// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/secret"
)

// outputGrace is how long Exec waits for the end of the output once the
// command exited and what it wrote is sent, so a background child holding
// the output open can't keep the step running (GST-9).
const outputGrace = 200 * time.Millisecond

// relayReadSize is how much of an output Exec reads, and sends, at once.
const relayReadSize = 32 << 10

// Exec runs one command and streams its output, then its exit code, or why
// it could not start. It runs as the request's user, else the image's, else
// root (GST-5), in the request's working directory, else the image's, else
// / (GST-6), with the image's environment under the request's (GST-4).
// Cancelling the call kills the command's process group at once (GST-8).
//
// The request is converted at once, and only its conversion is used: its
// env values are then held as secrets for the whole call (LOG-2).
func (s *server) Exec(req *agentv1.ExecRequest, stream agentv1.AgentService_ExecServer) error {
	return s.exec(newExecRequest(req), stream)
}

// execRequest is an Exec as the agent holds it, its env values secrets, so
// that logging it shows them redacted (LOG-2).
type execRequest struct {
	argv       []string
	env        secret.Env
	user       string
	workingDir string
}

func newExecRequest(req *agentv1.ExecRequest) execRequest {
	return execRequest{argv: req.GetArgv(), env: secret.EnvOf(req.GetEnv()), user: req.GetUser(), workingDir: req.GetWorkingDir()}
}

// LogValue implements slog.LogValuer: argv[0] and the environment's names,
// never another argument, the working directory or a value.
func (r execRequest) LogValue() slog.Value {
	argv0 := ""
	if len(r.argv) > 0 {
		argv0 = r.argv[0]
	}
	return slog.GroupValue(slog.String("argv0", argv0), slog.Any("env", r.env))
}

func (s *server) exec(req execRequest, stream agentv1.AgentService_ExecServer) error {
	sendStartError := func(format string, args ...any) error {
		return stream.Send(&agentv1.ExecResponse{Event: &agentv1.ExecResponse_StartError{StartError: fmt.Sprintf(format, args...)}})
	}
	c, err := prepare(s.root, s.image, req)
	var cantStart *startError
	if errors.As(err, &cantStart) {
		return sendStartError("%v", cantStart.err)
	}
	if err != nil {
		return err
	}

	// Cancelled with the call, or when the output can't be sent.
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	cmd := c.command(ctx)
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
	if s.execStdout != nil {
		s.execStdout(stdout.r)
	}
	cmd.Stdout, cmd.Stderr = stdout.w, stderr.w
	if err := cmd.Start(); err != nil {
		return sendStartError("%v", err)
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
	readSize := relayReadSize
	if s.execReadSize > 0 {
		readSize = s.execReadSize
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
			err := o.out.relay(send(o.wrap), readSize)
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

// command is a command the agent is about to start, an Exec's or the
// service's, resolved by one rule (GST-4 to GST-6).
type command struct {
	path string
	argv []string
	env  secret.Env
	dir  string
	cred *syscall.Credential
}

// startError is why a command can't start: a missing executable, an
// unknown user or group, a working directory that can't be made. Exec sends
// it as its start error, not as a failure of the call.
type startError struct{ err error }

func (e *startError) Error() string { return e.err.Error() }
func (e *startError) Unwrap() error { return e.err }

// prepare resolves req against the image's defaults in the job's root at
// root: it runs as the request's user, else the image's, else root (GST-5),
// in the request's working directory, else the image's, else /, which it
// creates if missing (GST-6), with the image's environment under the
// request's and argv[0] looked up in that environment's PATH, else the
// default PATH (GST-4). Why the command can't start is a *startError.
func prepare(root string, image *agentv1.Image, req execRequest) (command, error) {
	argv := req.argv
	if len(argv) == 0 {
		return command{}, &startError{errors.New("no command")}
	}
	id, err := resolveUser(root, cmp.Or(req.user, image.GetUser()))
	if err != nil {
		return command{}, &startError{err}
	}
	cred, err := credential(id)
	if err != nil {
		return command{}, err
	}
	env := environ(image.GetEnv(), req.env, image.GetDefaultPath(), id.home)
	path, err := lookPath(argv[0], env)
	if err != nil {
		return command{}, &startError{err}
	}
	dir := cmp.Or(req.workingDir, image.GetWorkingDir(), "/")
	if err := makeWorkDir(dir, id); err != nil {
		return command{}, &startError{err}
	}
	return command{path: path, argv: argv, env: env, dir: dir, cred: cred}, nil
}

// command returns c to start, as the leader of a process group of its own,
// which ctx's end kills (GST-8).
func (c command) command(ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.path, c.argv[1:]...) //nolint:gosec // G204: running the job's command is the agent's purpose
	cmd.Args[0] = c.argv[0]
	cmd.Env = c.env.Environ()
	cmd.Dir = c.dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: c.cred}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd
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
func (o *output) relay(send func([]byte) error, readSize int) error {
	buf := make([]byte, readSize)
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

// environ returns a command's environment, its values secrets (LOG-2): the
// image's, with the request's on top (GST-4), PATH set to defaultPath when
// neither sets it, and HOME to home, the user's, when neither sets it
// (GST-5). An image entry without `=` names no variable and is dropped.
func environ(image []string, req secret.Env, defaultPath, home string) secret.Env {
	vars := make(secret.Env, len(image)+len(req)+2)
	for _, kv := range image {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = secret.New(v)
		}
	}
	maps.Copy(vars, req)
	if _, ok := vars["PATH"]; !ok && defaultPath != "" {
		vars["PATH"] = secret.New(defaultPath)
	}
	if _, ok := vars["HOME"]; !ok {
		vars["HOME"] = secret.New(home)
	}
	return vars
}

// lookPath finds name in env's PATH, as the command's own environment
// would, not the agent's (GST-4). Its error quotes the PATH: it goes only
// to the runner, which shows it in the job's log, never to a log.
func lookPath(name string, env secret.Env) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	pathVar := env["PATH"].Reveal()
	for _, dir := range filepath.SplitList(pathVar) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", &notFoundError{name: name, path: pathVar}
}

// notFoundError is a command lookPath can't find. Its message quotes the
// PATH, for the runner only: the service's start failure, which is logged,
// leaves it out, since a service's PATH may be its env's (LOG-2).
type notFoundError struct{ name, path string }

func (e *notFoundError) Error() string {
	return fmt.Sprintf("%s: not found in PATH %s", e.name, e.path)
}
