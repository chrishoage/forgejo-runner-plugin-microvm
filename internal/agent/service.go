// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/secret"
)

// StartService starts the boot config's service with the request's
// environment, laid over the image's (LOG-2: the request is converted at
// once, its values held as secrets). Why it fails is FailedPrecondition: a
// VM without a service, a second call, or a command that can't start, which
// names argv[0] and why, never the PATH or a value of the environment.
func (s *server) StartService(ctx context.Context, req *agentv1.StartServiceRequest) (*agentv1.StartServiceResponse, error) {
	env := secret.EnvOf(req.GetEnv())
	if s.service == nil {
		return nil, status.Error(codes.FailedPrecondition, "this VM runs no service")
	}
	if err := s.service.start(ctx, env); err != nil {
		return nil, err
	}
	return &agentv1.StartServiceResponse{}, nil
}

// serviceStart is the boot config's service until StartService starts it:
// the first call starts it, or fails to, and every later call fails.
type serviceStart struct {
	log            *slog.Logger
	root           string
	image          *agentv1.Image
	svc            *agentv1.Service
	stdout, stderr *os.File
	// check is the image's health check, by Docker's rules; nil for none.
	check *healthCheck
	// called is closed once the first StartService has started the
	// service, or failed to; result then says which.
	called chan struct{}

	mu sync.Mutex
	// tried is set by the first StartService; stopped once the agent no
	// longer serves, so no service may start.
	tried, stopped bool
	running        *service
	err            error
	// health is the running service's health; endHealth ends its checks.
	health    *health
	endHealth func()
}

func newServiceStart(log *slog.Logger, root string, image *agentv1.Image, svc *agentv1.Service, stdout, stderr *os.File) *serviceStart {
	check, ignored := newHealthCheck(svc.GetHealthcheck())
	if ignored != "" {
		log.Warn("ignoring the image's health check, of a kind Docker ignores too", "kind", ignored)
	}
	return &serviceStart{log: log, root: root, image: image, svc: svc, stdout: stdout, stderr: stderr, check: check, called: make(chan struct{})}
}

// start starts the service with env, once, for the call whose context is
// ctx, and its health checks with it, which run with the same env, timed
// from the service's start; its error is the call's status.
func (st *serviceStart) start(ctx context.Context, env secret.Env) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	switch {
	case st.stopped:
		return status.Error(codes.FailedPrecondition, "the agent is stopping")
	case st.tried:
		return status.Error(codes.FailedPrecondition, "StartService was already called")
	}
	st.tried = true
	defer close(st.called)
	st.running, st.err = startService(ctx, st.root, st.image, st.svc.GetArgv(), env, st.stdout, st.stderr)
	if st.err != nil {
		return status.Error(codes.FailedPrecondition, st.err.Error())
	}
	st.log.Info("service started", "argv0", st.running.argv0)
	st.health = newHealth(st.check, st.running.started)
	if st.check != nil {
		// The checks outlive the call, as the service does.
		st.endHealth = monitorHealth(context.WithoutCancel(ctx), st.log, st.health, commandChecker(st.root, st.image, env, st.check.argv, st.stdout))
	}
	return nil
}

// healthResponse is Health's answer: the running service's health, or,
// before it starts, starting when it has a check and none when it hasn't.
func (st *serviceStart) healthResponse() *agentv1.HealthResponse {
	st.mu.Lock()
	h := st.health
	st.mu.Unlock()
	if h == nil {
		h = newHealth(st.check, time.Time{})
	}
	return h.response()
}

// endChecks stops any later StartService from starting the service, and
// ends its health checks, killing a check still running, once they have
// returned. A nil st has none.
func (st *serviceStart) endChecks() {
	if st == nil {
		return
	}
	st.mu.Lock()
	st.stopped = true
	end := st.endHealth
	st.endHealth = nil
	st.mu.Unlock()
	if end != nil {
		end()
	}
}

// result returns the service the first StartService started, or why it
// couldn't, once called is closed.
func (st *serviceStart) result() (*service, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.running, st.err
}

// stop stops any later StartService from starting the service, and returns
// the service if it runs. A nil st has none.
func (st *serviceStart) stop() *service {
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.stopped = true
	return st.running
}

// service is a service's command, which StartService started.
type service struct {
	argv0 string
	// started is when the command started.
	started time.Time
	// cancel kills the command's process group.
	cancel context.CancelFunc
	// done is closed once the command has exited and been reaped; then
	// state and err are set.
	done  chan struct{}
	state *os.ProcessState
	err   error
}

// startService starts the boot config's service command argv in the job's
// root at root, as an Exec of it with env and no user or working directory
// of its own would run (prepare): as the image's user, in the image's
// working directory, with env laid over the image's environment, argv[0]
// looked up in its PATH, else the default PATH. It leads a process group of
// its own, its stdout and stderr are the VM's console, and it inherits no
// other descriptor: the agent opens every one close-on-exec. Why it can't
// start names argv[0], never the PATH or the path found in it, which may be
// env's (LOG-2). The command outlives ctx, the call's that starts it: only
// kill, or exited, ends it.
func startService(ctx context.Context, root string, image *agentv1.Image, argv []string, env secret.Env, stdout, stderr *os.File) (*service, error) {
	c, err := prepare(root, image, execRequest{argv: argv, env: env})
	var notFound *notFoundError
	if errors.As(err, &notFound) {
		return nil, fmt.Errorf("service %s: not found in its PATH", argv[0])
	}
	if err != nil {
		return nil, fmt.Errorf("service %s: %w", argv[0], err)
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cmd := c.command(ctx)
	// Files, not writers: the command writes the console itself, and
	// no goroutine of the agent's copies its output.
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		cancel()
		// The path it quotes is found in the PATH: keep only why.
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			err = pe.Err
		}
		return nil, fmt.Errorf("service %s: %w", argv[0], err)
	}
	s := &service{argv0: argv[0], started: time.Now(), cancel: cancel, done: make(chan struct{})}
	go func() {
		s.err = cmd.Wait()
		s.state = cmd.ProcessState
		close(s.done)
	}()
	return s, nil
}

// kill kills the command's process group and waits until it is reaped.
func (s *service) kill() {
	s.cancel()
	<-s.done
}

// exited logs how the command ended, once done is closed, and returns the
// agent's exit code: the command's, or 128+n for signal n, as Exec reports
// it (GST-7); 1 when it can't be told.
func (s *service) exited(log *slog.Logger) int {
	s.cancel()
	var exit *exec.ExitError
	if s.err != nil && !errors.As(s.err, &exit) {
		log.Error("service", "argv0", s.argv0, "err", s.err)
		return 1
	}
	if ws, ok := s.state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		log.Info("service exited", "argv0", s.argv0, "signal", ws.Signal().String())
		return 128 + int(ws.Signal())
	}
	log.Info("service exited", "argv0", s.argv0, "exit_code", s.state.ExitCode())
	return s.state.ExitCode()
}
