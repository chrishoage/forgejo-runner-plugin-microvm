// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// startAgent serves the agent on a unix socket on the host and returns a
// client for it.
func startAgent(t *testing.T) agentv1.AgentServiceClient {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "agent.sock")
	lis, err := new(net.ListenConfig).Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		// Closing the listener ends Serve with an error.
		_ = Serve(lis, slog.New(slog.DiscardHandler))
	}()
	conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		_ = lis.Close()
		<-served
	})
	return agentv1.NewAgentServiceClient(conn)
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPing(t *testing.T) {
	t.Parallel()
	if _, err := startAgent(t).Ping(testContext(t), &agentv1.PingRequest{}); err != nil {
		t.Fatal(err)
	}
}
