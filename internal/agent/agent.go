// SPDX-License-Identifier: GPL-3.0-or-later

// Package agent is the agent that runs inside every VM: it makes the guest's
// boot mounts from its boot config, then serves the agent protocol's Ping,
// Exec, CopyIn, CopyOut and Flush.
//
// The agent binary may depend only on the standard library, grpc, protobuf,
// this package, internal/logging, internal/secret and the agent protocol's
// generated package; cmd/deps_test.go checks it.
package agent

import (
	"context"
	"errors"
	"log/slog"
	"net"

	"google.golang.org/grpc"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/logging"
)

// Port is the vsock port the agent listens on.
//
// SHORTCUT(#6): provisional pick: libkrun starts the agent as its workload,
// on vsock port 1024, and the VM is ready when the agent answers Ping.
const Port = 1024

// Main runs the agent, and returns its exit code.
func Main(args []string) int {
	log := logging.Setup("runner-microvm-agent")
	if len(args) > 0 {
		log.Error("the agent takes no arguments", "args", args)
		return 2
	}
	lis, err := listenVsock(Port)
	if err != nil {
		log.Error("listen", "err", err)
		return 1
	}
	log.Info("serving", "addr", lis.Addr())
	if err := Serve(lis, log); err != nil {
		log.Error("serve", "err", err)
		return 1
	}
	return 0
}

// Serve serves the agent protocol on lis until lis fails.
func Serve(lis net.Listener, log *slog.Logger) error {
	gs := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(gs, &server{log: log})
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
	// execExited, when set, is called once Exec has told its output that
	// the command exited: a test's view of it. Nil outside tests.
	execExited func()
}

// Ping answers: the agent is ready.
func (s *server) Ping(context.Context, *agentv1.PingRequest) (*agentv1.PingResponse, error) {
	return &agentv1.PingResponse{}, nil
}
