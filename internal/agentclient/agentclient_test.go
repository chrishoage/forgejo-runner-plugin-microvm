// SPDX-License-Identifier: GPL-3.0-or-later

package agentclient

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

type fakeAgent struct {
	agentv1.UnimplementedAgentServiceServer
}

func (fakeAgent) Ping(context.Context, *agentv1.PingRequest) (*agentv1.PingResponse, error) {
	return &agentv1.PingResponse{}, nil
}

// dropFirst closes the first n connections it accepts at once, as libkrun
// does while the guest's agent isn't listening yet.
type dropFirst struct {
	net.Listener
	n atomic.Int32
}

func (l *dropFirst) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.n.Add(-1) < 0 {
			return c, nil
		}
		_ = c.Close()
	}
}

func serveAgent(t *testing.T, lis net.Listener) {
	t.Helper()
	gs := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(gs, fakeAgent{})
	done := make(chan struct{})
	go func() { defer close(done); _ = gs.Serve(lis) }()
	t.Cleanup(func() { gs.Stop(); <-done })
}

func newClient(t *testing.T, path string) *Client {
	t.Helper()
	c, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// The agent answers only after libkrun has dropped several connections.
func TestWaitReadyRetriesDroppedConnections(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(t.TempDir(), "agent.sock")
	lis, err := new(net.ListenConfig).Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	dropping := &dropFirst{Listener: lis}
	dropping.n.Store(5)
	serveAgent(t, dropping)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := newClient(t, sock).WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if n := dropping.n.Load(); n >= 0 {
		t.Errorf("ready with %d drops left", n)
	}
}

// The socket appears only once the VM child has started libkrun.
func TestWaitReadyBeforeTheSocketExists(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(t.TempDir(), "agent.sock")
	c := newClient(t, sock)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ready := make(chan error, 1)
	go func() { ready <- c.WaitReady(ctx) }()
	lis, err := new(net.ListenConfig).Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	serveAgent(t, lis)
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
}

// An agent that never answers costs the caller its deadline, no more.
func TestWaitReadyGivesUpAtTheDeadline(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(t.TempDir(), "agent.sock")
	lis, err := new(net.ListenConfig).Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	dropping := &dropFirst{Listener: lis}
	dropping.n.Store(1 << 30)
	serveAgent(t, dropping)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = newClient(t, sock).WaitReady(ctx)
	if err == nil {
		t.Fatal("ready, but the agent never answered")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("WaitReady returned %v before its deadline", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("WaitReady took %v past a 300ms deadline", d)
	}
}
