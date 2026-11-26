// SPDX-License-Identifier: GPL-3.0-or-later

// Package agent is the agent that runs inside every VM: it makes the guest's
// boot mounts, configures its network, hostname and name resolution from its
// boot config, then serves the agent protocol's Ping, StartService, Exec,
// CopyIn, CopyOut, Flush and Health; once StartService has started the boot
// config's service, and its image's health check, until the service exits.
//
// The agent binary may depend only on the standard library, grpc, protobuf,
// vishvananda/netlink, moby/sys/user, this package, internal/logging,
// internal/secret and the agent protocol's generated package;
// cmd/deps_test.go checks it.
package agent

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/logging"
)

// stopGrace is how long the agent, once its service couldn't start, lets
// the calls in flight finish, StartService's reply among them, before it
// stops serving and exits.
const stopGrace = 5 * time.Second

// Main runs the agent, and returns its exit code. libkrun's init, PID 1,
// starts it as its workload, with an empty environment. It reads its boot
// config, prepares the guest, and only then listens on the boot config's
// vsock port: the agent is up when it answers Ping, so every call finds the
// guest prepared. A service's VM starts its service when StartService asks.
func Main(args []string) int {
	log, level := logging.Setup("runner-microvm-agent")
	if len(args) > 0 {
		log.Error("the agent takes no arguments", "args", args)
		return 2
	}
	cfg, err := ReadBootConfig(BootConfigPath, level)
	if err != nil {
		log.Error("boot", "err", err)
		return 1
	}
	log.Debug("read the boot config", "layers", len(cfg.GetLayers()), "mounts", len(cfg.GetMounts()), "disks", len(cfg.GetDisks()),
		"scratch_paths", len(cfg.GetScratchPaths()), "service", cfg.GetService() != nil, "log_level", cfg.GetLogLevel())
	return run(log, cfg, guest{
		prepare: prepareGuest,
		root:    "/",
		listen:  listenVsock,
		// libkrun's init gives the agent the VM's console as its
		// output.
		stdout: os.Stdout, stderr: os.Stderr,
		stopGrace: stopGrace,
	})
}

// guest is the VM as run sees it, so a test can run the agent on the host.
type guest struct {
	// prepare builds the job's root and moves into it, then configures
	// the network and names, and returns the read-write disks it
	// mounted.
	prepare func(*agentv1.BootConfig) ([]rwDisk, error)
	// root is the job's root once prepare has returned: /.
	root string
	// listen listens on the agent's vsock port.
	listen func(port uint32) (net.Listener, error)
	// stdout and stderr are the VM's console, the service's output.
	stdout, stderr *os.File
	// stopGrace bounds how long the agent lets the calls in flight finish
	// once its service couldn't start: stopGrace, or a test's.
	stopGrace time.Duration
}

// prepareGuest is the VM's guest.prepare.
func prepareGuest(cfg *agentv1.BootConfig) ([]rwDisk, error) {
	rwDisks, err := boot(kernel{}, cfg)
	if err != nil {
		return nil, err
	}
	if err := configureNetwork(rtnetlink{}, cfg.GetNetwork()); err != nil {
		return nil, err
	}
	// The job's root is / now.
	if err := configureNames(sethostname, "/", rootOwner, cfg); err != nil {
		return nil, err
	}
	return rwDisks, nil
}

// run marks every descriptor the agent inherited above 2 close-on-exec,
// before it starts anything, prepares the guest g from cfg, then serves on the vsock port and
// returns the agent's exit code, which ends the agent and so stops the VM:
// libkrun's init, PID 1, has the agent as its workload.
//
// A VM without a service serves until the listener fails. A service's VM
// serves without its service until StartService starts it, with no timer of
// its own: the plugin kills a VM whose call never comes. Once the service
// runs, run returns when it exits, having logged its status, with its exit
// code. If it can't start, run lets the calls in flight finish, so the
// failed StartService's reply reaches the plugin, for at most g.stopGrace,
// then returns 1. If the listener fails, run kills and reaps the service,
// if it runs, and returns 1.
func run(log *slog.Logger, cfg *agentv1.BootConfig, g guest) int {
	if err := closeInheritedOnExec(); err != nil {
		log.Error("boot", "err", err)
		return 1
	}
	rwDisks, err := g.prepare(cfg)
	if err != nil {
		log.Error("boot", "err", err)
		return 1
	}
	log.Info("booted", "layers", len(cfg.GetLayers()), "address", cfg.GetNetwork().GetAddress(), "hostname", cfg.GetHostname())
	lis, err := g.listen(cfg.GetVsockPort())
	if err != nil {
		log.Error("listen", "err", err)
		return 1
	}
	var start *serviceStart
	// A nil channel: a VM without a service serves until its listener
	// fails.
	var called <-chan struct{}
	if cfg.GetService() != nil {
		start = newServiceStart(log, g.root, cfg.GetImage(), cfg.GetService(), g.stdout, g.stderr)
		called = start.called
		// The health checks end with the agent, whichever way it ends.
		defer start.endChecks()
	}
	gs := newGRPCServer(&server{log: log, image: cfg.GetImage(), root: g.root, rwDisks: rwDisks, service: start})
	served := make(chan error, 1)
	go func() { served <- serveOn(gs, lis) }()
	log.Info("serving", "addr", lis.Addr())
	// listenerFailed ends a run whose listener failed: no service starts
	// any more, and a running one is killed and reaped.
	listenerFailed := func(err error) int {
		if svc := start.stop(); svc != nil {
			svc.kill()
		}
		gs.Stop()
		log.Error("serve", "err", err)
		return 1
	}
	select {
	case err := <-served:
		return listenerFailed(err)
	case <-called:
	}
	svc, err := start.result()
	if err != nil {
		log.Error("service", "err", err)
		stopWithin(gs, g.stopGrace)
		if err := <-served; err != nil {
			log.Error("serve", "err", err)
		}
		return 1
	}
	select {
	case err := <-served:
		return listenerFailed(err)
	case <-svc.done:
		code := svc.exited(log)
		gs.Stop()
		if err := <-served; err != nil {
			log.Error("serve", "err", err)
		}
		return code
	}
}

// stopWithin stops gs gracefully, letting the calls in flight finish and
// their replies go out, for at most grace; then it stops gs at once, which
// cancels the calls still running.
func stopWithin(gs *grpc.Server, grace time.Duration) {
	stopped := make(chan struct{})
	go func() {
		gs.GracefulStop()
		close(stopped)
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-stopped:
	case <-timer.C:
		gs.Stop()
		<-stopped
	}
}

// monitorHealth runs h's checks with run in a goroutine of its own, until
// ctx ends at the latest, and returns the function that ends them: it kills
// a check still running and waits until the goroutine has returned.
func monitorHealth(ctx context.Context, log *slog.Logger, h *health, run checker) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.monitor(ctx, run, log)
	}()
	return func() {
		cancel()
		<-done
	}
}

// serve serves s on lis until lis fails.
func serve(lis net.Listener, s *server) error {
	return serveOn(newGRPCServer(s), lis)
}

func newGRPCServer(s *server) *grpc.Server {
	gs := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(gs, s)
	return gs
}

// serveOn serves gs on lis until lis fails or gs stops; a stop is no error.
func serveOn(gs *grpc.Server, lis net.Listener) error {
	err := gs.Serve(lis)
	if errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

// server is the agent protocol's service.
type server struct {
	agentv1.UnimplementedAgentServiceServer
	log *slog.Logger
	// image is the boot config's defaults for Exec: the image's
	// environment, user and working directory, and the default PATH.
	image *agentv1.Image
	// root is the job's root, whose /etc/passwd and /etc/group Exec
	// resolves users against: /, once the agent has moved into it; a
	// directory of a test's.
	root string
	// rwDisks are the read-write disks boot mounted, which Flush freezes.
	rwDisks []rwDisk
	// service is the boot config's service, which StartService starts,
	// and whose health Health answers; nil for a VM without one.
	service *serviceStart
	// flusher is Flush's kernel; nil is the real one.
	flusher flusher
	// execExited, when set, is called once Exec has told its output that
	// the command exited: a test's view of it. Nil outside tests.
	execExited func()
	// execStdout, when set, is called with the read end of the command's
	// stdout pipe, before the command starts: a test's hold on the pipe's
	// size. Nil outside tests.
	execStdout func(*os.File)
	// execReadSize, when positive, replaces relayReadSize: a test's way to
	// make a small output take several reads. Zero outside tests.
	execReadSize int
}

// Ping answers: the agent is up. A service's VM answers before its service
// has started.
func (s *server) Ping(context.Context, *agentv1.PingRequest) (*agentv1.PingResponse, error) {
	return &agentv1.PingResponse{}, nil
}

// Health answers with the service's health: none for a VM without a
// service, or a service without a check.
func (s *server) Health(context.Context, *agentv1.HealthRequest) (*agentv1.HealthResponse, error) {
	if s.service == nil {
		return &agentv1.HealthResponse{State: agentv1.HealthResponse_STATE_NONE}, nil
	}
	return s.service.healthResponse(), nil
}
