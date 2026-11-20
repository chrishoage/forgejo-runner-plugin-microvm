// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
)

// startedJob returns a client and a started job's environment id.
func startedJob(t *testing.T) (pluginv1alpha.BackendPluginClient, string) {
	t.Helper()
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, &fakeCache{}, &fakeLauncher{})))
	id := create(t, c)
	if _, err := start(testContext(t), c, id); err != nil {
		t.Fatal(err)
	}
	return c, id
}

func execAll(t *testing.T, c pluginv1alpha.BackendPluginClient, req *pluginv1alpha.ExecRequest) ([]*pluginv1alpha.ExecOutput, error) {
	t.Helper()
	stream, err := c.Exec(testContext(t), req)
	if err != nil {
		return nil, err
	}
	var outs []*pluginv1alpha.ExecOutput
	for {
		o, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return outs, nil
		}
		if err != nil {
			return outs, err
		}
		outs = append(outs, o)
	}
}

func TestExecRelaysTheAgent(t *testing.T) {
	t.Parallel()
	c, id := startedJob(t)
	outs, err := execAll(t, c, &pluginv1alpha.ExecRequest{
		EnvironmentId: id, Command: []string{"sh", "-e", "/var/run/act/workflow/0"},
		Env: map[string]string{"A": "secret"}, Workdir: "/workspace/e2e/echo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 3 ||
		outs[0].GetData().GetStream() != pluginv1alpha.DataChunk_STDOUT ||
		string(outs[0].GetData().GetData()) != "sh -e /var/run/act/workflow/0 in /workspace/e2e/echo" ||
		outs[1].GetData().GetStream() != pluginv1alpha.DataChunk_STDERR || string(outs[1].GetData().GetData()) != "secret" ||
		outs[2].GetExecComplete() == nil || outs[2].GetExecComplete().GetExitCode() != 3 {
		t.Errorf("Exec = %v", outs)
	}

	outs, err = execAll(t, c, &pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"start-error"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 || outs[0].GetExecFailed().GetErrorMessage() != "no such file" {
		t.Errorf("Exec of a command that can't start = %v", outs)
	}
}

func TestCallsBeforeStart(t *testing.T) {
	t.Parallel()
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, &fakeCache{}, &fakeLauncher{})))
	id := create(t, c)
	_, err := execAll(t, c, &pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"true"}})
	wantCode(t, err, codes.FailedPrecondition)
	_, err = execAll(t, c, &pluginv1alpha.ExecRequest{EnvironmentId: "job-00000000", Command: []string{"true"}})
	wantCode(t, err, codes.NotFound)
	_, err = copyOutAll(t, c, id, "/x")
	wantCode(t, err, codes.FailedPrecondition)
}

func copyInAll(t *testing.T, c pluginv1alpha.BackendPluginClient, chunks ...*pluginv1alpha.CopyInChunk) error {
	t.Helper()
	stream, err := c.CopyIn(testContext(t))
	if err != nil {
		return err
	}
	for _, ch := range chunks {
		if err := stream.Send(ch); err != nil {
			break
		}
	}
	_, err = stream.CloseAndRecv()
	return err
}

func TestCopyInRelaysTheAgent(t *testing.T) {
	t.Parallel()
	c, id := startedJob(t)
	dest := "/var/run/act/"
	if err := copyInAll(t, c, &pluginv1alpha.CopyInChunk{EnvironmentId: &id, DestPath: &dest},
		&pluginv1alpha.CopyInChunk{Data: []byte("o")}, &pluginv1alpha.CopyInChunk{Data: []byte("k")}); err != nil {
		t.Fatal(err)
	}
	// The agent's error comes back with what it received.
	err := copyInAll(t, c, &pluginv1alpha.CopyInChunk{EnvironmentId: &id, DestPath: &dest, Data: []byte("a")},
		&pluginv1alpha.CopyInChunk{Data: []byte("b")})
	wantCode(t, err, codes.InvalidArgument)
	if msg := status.Convert(err).Message(); msg != "/var/run/act/:ab" {
		t.Errorf("agent saw %q", msg)
	}
	// Only the first chunk may name the job and destination.
	err = copyInAll(t, c, &pluginv1alpha.CopyInChunk{EnvironmentId: &id, DestPath: &dest},
		&pluginv1alpha.CopyInChunk{DestPath: &dest})
	wantCode(t, err, codes.InvalidArgument)
	err = copyInAll(t, c, &pluginv1alpha.CopyInChunk{Data: []byte("x")})
	wantCode(t, err, codes.InvalidArgument)
}

func copyOutAll(t *testing.T, c pluginv1alpha.BackendPluginClient, id, path string) ([]byte, error) {
	t.Helper()
	stream, err := c.CopyOut(testContext(t), &pluginv1alpha.CopyOutRequest{EnvironmentId: id, SrcPath: path})
	if err != nil {
		return nil, err
	}
	var b []byte
	for {
		ch, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return b, nil
		}
		if err != nil {
			return b, err
		}
		b = append(b, ch.GetData()...)
	}
}

func TestCopyOutRelaysTheAgent(t *testing.T) {
	t.Parallel()
	c, id := startedJob(t)
	b, err := copyOutAll(t, c, id, "/var/run/act/workflow/envs.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "/var/run/act/workflow/envs.txt" {
		t.Errorf("CopyOut = %q", b)
	}
}

// stalledStream is the runner's side of a call that stops reading and
// sending: Send, and every Recv after the first, block until its context
// ends, as gRPC ends it once the handler returns.
type stalledStream struct {
	grpc.ServerStream
	ctx   context.Context
	first *pluginv1alpha.CopyInChunk

	once    sync.Once
	entered chan struct{}
	left    chan struct{}
}

func newStalledStream(ctx context.Context, first *pluginv1alpha.CopyInChunk) *stalledStream {
	return &stalledStream{ctx: ctx, first: first, entered: make(chan struct{}), left: make(chan struct{})}
}

func (s *stalledStream) Context() context.Context { return s.ctx }

func (s *stalledStream) stall() error {
	s.once.Do(func() { close(s.entered) })
	<-s.ctx.Done()
	close(s.left)
	return s.ctx.Err()
}

func (s *stalledStream) Send(*pluginv1alpha.ExecOutput) error { return s.stall() }

func (s *stalledStream) SendAndClose(*pluginv1alpha.CopyInResponse) error { return s.stall() }

func (s *stalledStream) Recv() (*pluginv1alpha.CopyInChunk, error) {
	if first := s.first; first != nil {
		s.first = nil
		return first, nil
	}
	return nil, s.stall()
}

// within waits for ch, failing the test after a deadline.
func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: still waiting after 10s", what)
		panic("unreachable")
	}
}

// Remove returns at once while an Exec whose runner stops reading, or a
// CopyIn whose runner stops sending, is blocked on the runner's stream, and
// the call returns too (PROTO-13).
func TestRemoveStalled(t *testing.T) {
	t.Parallel()
	for name, call := range map[string]func(srv *Server, id string, s *stalledStream) error{
		// Short names: the test's directory holds sockets.
		"Exec": func(srv *Server, id string, s *stalledStream) error {
			return srv.Exec(&pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"sh"}}, s)
		},
		"CopyIn": func(srv *Server, _ string, s *stalledStream) error {
			return srv.CopyIn(s)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			launcher := &fakeLauncher{}
			srv := newTestServer(t, &fakeCache{}, launcher)
			c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
			id := create(t, c)
			if _, err := start(testContext(t), c, id); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(testContext(t))
			// Unblocks a call that failed to return, so the test ends.
			t.Cleanup(cancel)
			dest := "/var/run/act/"
			stream := newStalledStream(ctx, &pluginv1alpha.CopyInChunk{EnvironmentId: &id, DestPath: &dest})
			returned := make(chan error, 1)
			go func() { returned <- call(srv, id, stream) }()
			within(t, stream.entered, "the call reaching the runner's stream")

			removed := make(chan error, 1)
			go func() {
				_, err := srv.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id})
				removed <- err
			}()
			if err := within(t, removed, "Remove"); err != nil {
				t.Fatal(err)
			}
			if err := within(t, returned, "the call's return"); err == nil {
				t.Error("the call succeeded on a removed job")
			}
			if launcher.alive() != 0 {
				t.Error("the job process outlived Remove")
			}
			// gRPC ends the stream once the handler returned; the blocked
			// stream call then ends too.
			cancel()
			within(t, stream.left, "the stream call's return")
		})
	}
}
