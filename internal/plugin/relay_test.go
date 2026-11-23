// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
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

// A step's relative working directory is taken from the job's workspace,
// the request's GITHUB_WORKSPACE, as the runner's own container backend
// takes it from the container's working directory, which is the same
// path. An absolute one, or a relative one with no absolute workspace,
// reaches the agent unchanged.
func TestExecTakesARelativeWorkingDirFromTheWorkspace(t *testing.T) {
	t.Parallel()
	c, id := startedJob(t)
	for _, tt := range []struct {
		workdir, workspace, want string
	}{
		{"not/yet/here", "/workspace/e2e/steps", "/workspace/e2e/steps/not/yet/here"},
		{"../sibling", "/workspace/e2e/steps", "/workspace/e2e/sibling"},
		{"/tmp", "/workspace/e2e/steps", "/tmp"},
		{"", "/workspace/e2e/steps", ""},
		{"not/yet/here", "", "not/yet/here"},
		{"not/yet/here", "relative", "not/yet/here"},
	} {
		env := map[string]string{}
		if tt.workspace != "" {
			env["GITHUB_WORKSPACE"] = tt.workspace
		}
		outs, err := execAll(t, c, &pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"pwd"}, Env: env, Workdir: tt.workdir})
		if err != nil {
			t.Fatal(err)
		}
		if len(outs) == 0 || string(outs[0].GetData().GetData()) != "pwd in "+tt.want {
			t.Errorf("Exec in %q with GITHUB_WORKSPACE %q = %v, want the agent to get %q", tt.workdir, tt.workspace, outs, tt.want)
		}
	}
}

// Exec, CopyIn and CopyOut on a job created but not started fail with
// FailedPrecondition: the job exists but can't take the call yet. An unknown
// id is NotFound (PROTO-11).
func TestCallsBeforeStart(t *testing.T) {
	t.Parallel()
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, &fakeCache{}, &fakeLauncher{})))
	id := create(t, c)
	_, err := execAll(t, c, &pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"true"}})
	wantCode(t, err, codes.FailedPrecondition)
	_, err = execAll(t, c, &pluginv1alpha.ExecRequest{EnvironmentId: "job-00000000", Command: []string{"true"}})
	wantCode(t, err, codes.NotFound)
	dest := "/var/run/act/"
	err = copyInAll(t, c, &pluginv1alpha.CopyInChunk{EnvironmentId: &id, DestPath: &dest, Data: []byte("ok")})
	wantCode(t, err, codes.FailedPrecondition)
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

// chunkAgent's CopyIn records the size of each chunk it gets, and what they
// make up.
type chunkAgent struct {
	fakeAgent

	mu     sync.Mutex
	dest   string
	sizes  []int
	copied []byte
}

func (a *chunkAgent) CopyIn(stream agentv1.AgentService_CopyInServer) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&agentv1.CopyInResponse{})
		}
		if err != nil {
			return err
		}
		a.mu.Lock()
		if req.GetDestination() != "" {
			a.dest = req.GetDestination()
		}
		a.sizes = append(a.sizes, len(req.GetChunk()))
		a.copied = append(a.copied, req.GetChunk()...)
		a.mu.Unlock()
	}
}

// CopyIn re-chunks the runner's chunks, up to 32 MiB, to at most 256 KiB
// toward the agent, which keeps gRPC's 4 MiB default limit, and the agent
// gets every byte in order, the destination in its first message.
func TestCopyInRechunksTowardTheAgent(t *testing.T) {
	t.Parallel()
	agent := &chunkAgent{}
	c, id := startedWith(t, &fakeLauncher{agent: agent}, 0)
	data := make([]byte, 20<<20+5)
	for i := range data {
		data[i] = byte(i % 251)
	}
	dest := "/var/run/act/"
	if err := copyInAll(t, c, &pluginv1alpha.CopyInChunk{EnvironmentId: &id, DestPath: &dest, Data: data[:20<<20]},
		&pluginv1alpha.CopyInChunk{Data: data[20<<20:]}); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if agent.dest != dest || !bytes.Equal(agent.copied, data) {
		t.Errorf("the agent got %d bytes at %q; want %d at %q", len(agent.copied), agent.dest, len(data), dest)
	}
	// 80 chunks of 256 KiB, then the runner's second chunk.
	if len(agent.sizes) != 81 || slices.Max(agent.sizes) != 256<<10 || agent.sizes[80] != 5 {
		t.Errorf("the agent got chunks of %v", agent.sizes)
	}
}
