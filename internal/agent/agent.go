// SPDX-License-Identifier: GPL-3.0-or-later

// Package agent is the agent that runs inside every VM: it makes the guest's
// boot mounts, configures its network, hostname and name resolution from its
// boot config, then serves the agent protocol's Ping, Exec, CopyIn, CopyOut
// and Flush.
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

	"google.golang.org/grpc"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/logging"
)

// Main runs the agent, and returns its exit code. libkrun's init, PID 1,
// starts it as its workload, with an empty environment. It reads its boot
// config, prepares the guest, and only then listens on the boot config's
// vsock port: the VM is ready when the agent answers Ping, so every call
// finds the guest prepared.
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
		"scratch_paths", len(cfg.GetScratchPaths()), "log_level", cfg.GetLogLevel())
	rwDisks, err := boot(kernel{}, cfg)
	if err != nil {
		log.Error("boot", "err", err)
		return 1
	}
	if err := configureNetwork(rtnetlink{}, cfg.GetNetwork()); err != nil {
		log.Error("boot", "err", err)
		return 1
	}
	// The job's root is / now.
	if err := configureNames(sethostname, "/", rootOwner, cfg); err != nil {
		log.Error("boot", "err", err)
		return 1
	}
	log.Info("booted", "layers", len(cfg.GetLayers()), "address", cfg.GetNetwork().GetAddress(), "hostname", cfg.GetHostname())
	lis, err := listenVsock(cfg.GetVsockPort())
	if err != nil {
		log.Error("listen", "err", err)
		return 1
	}
	log.Info("serving", "addr", lis.Addr())
	if err := serve(lis, &server{log: log, image: cfg.GetImage(), root: "/", rwDisks: rwDisks}); err != nil {
		log.Error("serve", "err", err)
		return 1
	}
	return 0
}

// serve serves s on lis until lis fails.
func serve(lis net.Listener, s *server) error {
	gs := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(gs, s)
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

// Ping answers: the agent is ready.
func (s *server) Ping(context.Context, *agentv1.PingRequest) (*agentv1.PingResponse, error) {
	return &agentv1.PingResponse{}, nil
}
