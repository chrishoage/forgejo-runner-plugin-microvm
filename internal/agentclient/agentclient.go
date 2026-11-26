// SPDX-License-Identifier: GPL-3.0-or-later

// Package agentclient is the plugin's side of the agent protocol: it dials a
// VM's agent over the unix socket libkrun maps to the guest's vsock port,
// decides readiness, starts a service's VM's service, and runs Exec, CopyIn,
// CopyOut and Flush.
//
// It is its own package so it can't drift into internal/agent and pull host
// code toward the guest.
package agentclient

import (
	"context"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// ReadyTimeout is how long the plugin waits for a VM's agent to answer Ping.
// Boot takes about 2 s; 30 s leaves room on a loaded host.
const ReadyTimeout = 30 * time.Second

// Until the agent listens, libkrun accepts a connection on the socket and
// drops it at once, so the client redials quickly: every 10 ms at first, at
// most every 100 ms, each attempt bounded by 500 ms.
var connectParams = grpc.ConnectParams{
	Backoff: backoff.Config{
		BaseDelay:  10 * time.Millisecond,
		Multiplier: 1.6,
		Jitter:     0.2,
		MaxDelay:   100 * time.Millisecond,
	},
	MinConnectTimeout: 500 * time.Millisecond,
}

// Client is a connection to one VM's agent.
type Client struct {
	conn *grpc.ClientConn
	rpc  agentv1.AgentServiceClient
}

// New returns a client for the agent behind the unix socket at path. It
// connects lazily: WaitReady says when the agent answers.
func New(path string) (*Client, error) {
	conn, err := grpc.NewClient("passthrough:///agent",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return new(net.Dialer).DialContext(ctx, "unix", path)
		}),
		grpc.WithConnectParams(connectParams),
	)
	if err != nil {
		return nil, fmt.Errorf("agent at %s: %w", path, err)
	}
	return &Client{conn: conn, rpc: agentv1.NewAgentServiceClient(conn)}, nil
}

// WaitReady returns once the agent answers Ping, or with ctx's error.
func (c *Client) WaitReady(ctx context.Context) error {
	_, err := c.rpc.Ping(ctx, &agentv1.PingRequest{}, grpc.WaitForReady(true))
	return err
}

// Ping returns once the agent answers, or at once with Unavailable while
// the connection to it is broken: it does not wait for the agent to be
// ready, as WaitReady does.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.rpc.Ping(ctx, &agentv1.PingRequest{})
	return err
}

// StartService starts the service of a service's VM with env, the service's
// own environment, whose values may be secrets (LOG-2), and returns once its
// process has started. It does not wait for the agent to be ready: call it
// after WaitReady. A command that can't start, a second call, and a VM
// without a service fail with the agent's FailedPrecondition.
func (c *Client) StartService(ctx context.Context, env map[string]string) error {
	_, err := c.rpc.StartService(ctx, &agentv1.StartServiceRequest{Env: env})
	return err
}

// Close closes the connection, ending every call on it.
func (c *Client) Close() error { return c.conn.Close() }

// Exec runs one command in the guest; the stream ends with its exit code or
// start error. Cancelling ctx kills the command.
func (c *Client) Exec(ctx context.Context, req *agentv1.ExecRequest) (agentv1.AgentService_ExecClient, error) {
	return c.rpc.Exec(ctx, req)
}

// CopyIn opens a CopyIn; its first request names the destination.
func (c *Client) CopyIn(ctx context.Context) (agentv1.AgentService_CopyInClient, error) {
	return c.rpc.CopyIn(ctx)
}

// CopyOut streams a guest path back as a tar.
func (c *Client) CopyOut(ctx context.Context, req *agentv1.CopyOutRequest) (agentv1.AgentService_CopyOutClient, error) {
	return c.rpc.CopyOut(ctx, req)
}

// Flush writes everything on the VM's read-write mounts and disks through
// to the host and leaves each read-write disk clean (STO-9). It does not
// wait for the agent to be ready: a VM whose agent is gone fails it at
// once.
func (c *Client) Flush(ctx context.Context) error {
	_, err := c.rpc.Flush(ctx, &agentv1.FlushRequest{})
	return err
}
