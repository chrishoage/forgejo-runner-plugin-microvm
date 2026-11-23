// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
)

// silence is how long the calls below wait without a message. A client
// pinging every 30 s, as the runner does, is cut off by grpc-go's default
// enforcement policy (no more than one ping every 5 minutes, two strikes
// allowed) at its third keepalive ping, about 90 s in; 5 minutes is well
// past that.
const silence = 5 * time.Minute

// The options of forgejo-runner v13.2.2's plugin client, act/plugin/client.go,
// mirrored because the client can't run in this process (its generated
// protocol and ours register the same proto file). TestRunnerClientOptions
// checks them against the runner module's source, so a runner update that
// changes one fails there.
const (
	runnerKeepaliveTime       = 30 * time.Second
	runnerKeepaliveTimeout    = 10 * time.Second
	runnerPermitWithoutStream = false
	runnerMaxMessageSize      = 32 * 1024 * 1024
)

// pipeListener is a net.Listener of in-memory connections. net.Pipe is made
// of channels, so goroutines blocked on it are durably blocked, which a
// socket's are not: under testing/synctest, time passes only while every
// goroutine of the bubble is.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (*pipeListener) Addr() net.Addr { return pipeAddr{} }

// dial returns the client end of a new connection to l.
func (l *pipeListener) dial(ctx context.Context) (net.Conn, error) {
	server, client := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// runnerClient returns a client of lis dialed with the options of
// forgejo-runner v13.2.2's plugin client, act/plugin/client.go: its keepalive
// parameters at their defaults, and 32 MiB messages.
func runnerClient(t *testing.T, lis *pipeListener) pluginv1alpha.BackendPluginClient {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///plugin",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.dial(ctx) }),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(runnerMaxMessageSize),
			grpc.MaxCallSendMsgSize(runnerMaxMessageSize),
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                runnerKeepaliveTime,
			Timeout:             runnerKeepaliveTimeout,
			PermitWithoutStream: runnerPermitWithoutStream,
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pluginv1alpha.NewBackendPluginClient(conn)
}

// silentStart creates a job through c and Starts it, with the image cache
// holding the pull for silence of fake time. A queued Start is the silent
// call the policy exists for, and the only one whose wait is made of
// channels: an Exec's goes through a socket to the agent, which a bubble
// doesn't count as blocked, so its clock would never advance. It returns
// the error that ends the Start, nil once Start completed.
func silentStart(t *testing.T, c pluginv1alpha.BackendPluginClient, cache *fakeCache) error {
	t.Helper()
	ctx := t.Context()
	id := create(t, c)
	started := make(chan error, 1)
	go func() {
		_, err := start(ctx, c, id)
		started <- err
	}()
	<-cache.entered
	time.Sleep(silence)
	close(cache.release)
	if err := <-started; err != nil {
		return err
	}
	if _, err := c.Remove(ctx, &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatal(err)
	}
	return nil
}

func queuedCache() *fakeCache {
	return &fakeCache{block: true, entered: make(chan string, 1), release: make(chan struct{})}
}

// Through the runner's own client options, on a fake clock, a call that
// stays silent for 5 minutes ends normally: Serve's keepalive policy accepts
// the client's pings every 30 s rather than cutting the connection off
// (PROTO-15). Only MinTime is proven: PermitWithoutStream and the server's
// own keepalive time are not. Under grpc-go's default policy the same call is cut off, which
// shows the first subtest could fail.
func TestASilentCallIsNotCutOff(t *testing.T) {
	t.Parallel()
	t.Run("Serve", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			cache := queuedCache()
			srv := newTestServer(t, cache, &fakeLauncher{})
			lis := newPipeListener()
			ctx, cancel := context.WithCancel(context.Background())
			served := make(chan error, 1)
			go func() { served <- Serve(ctx, lis, srv) }()
			defer func() {
				cancel()
				if err := <-served; err != nil {
					t.Errorf("Serve: %v", err)
				}
			}()
			begin := time.Now()
			if err := silentStart(t, runnerClient(t, lis), cache); err != nil {
				t.Fatalf("the silent call ended with %v", err)
			}
			if took := time.Since(begin); took < silence {
				t.Fatalf("the call ended after %v of fake time, before the silence did", took)
			}
		})
	})
	t.Run("default policy", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			cache := queuedCache()
			srv := newTestServer(t, cache, &fakeLauncher{})
			lis := newPipeListener()
			gs := grpc.NewServer(grpc.MaxRecvMsgSize(maxMessage))
			pluginv1alpha.RegisterBackendPluginServer(gs, srv)
			served := make(chan struct{})
			go func() { defer close(served); _ = gs.Serve(lis) }()
			defer func() {
				gs.Stop()
				<-served
			}()
			err := silentStart(t, runnerClient(t, lis), cache)
			wantCode(t, err, codes.Unavailable)
			if !strings.Contains(err.Error(), "too_many_pings") {
				t.Fatalf("the silent call ended with %v, want too_many_pings", err)
			}
		})
	})
}
