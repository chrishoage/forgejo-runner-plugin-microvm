// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/imagecache"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/joblaunch"
)

// newTestServer returns a server on temporary directories with the given
// fakes for its seams. It is closed when the test ends. The directories are
// deep enough that every agent socket's path outgrows bind and connect's 107
// bytes, whatever the host's temporary directory is.
func newTestServer(t *testing.T, cache *fakeCache, launcher *fakeLauncher) *Server {
	t.Helper()
	base := filepath.Join(t.TempDir(), strings.Repeat("d", 60), strings.Repeat("e", 60))
	dirs := testDirs(base)
	if err := os.MkdirAll(dirs.Runtime, 0o755); err != nil {
		t.Fatal(err)
	}
	// The agent, as startup writes it, on the state directory's filesystem.
	agent := filepath.Join(dirs.State, "bin", "runner-microvm-agent-0")
	if err := os.MkdirAll(filepath.Dir(agent), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent, []byte("agent binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(discardLog(), Config{
		Dirs:         dirs,
		JobBinary:    "/fake/runner-microvm-job",
		MkfsExt4:     "/fake/mkfs.ext4",
		Agent:        agent,
		Cache:        cache,
		Launcher:     launcher,
		ReadyTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := srv.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return srv
}

// fakeCache stands in for the image cache.
type fakeCache struct {
	// err fails Acquire.
	err error
	// block makes Acquire wait for its context, after closing entered.
	block   bool
	entered chan struct{}

	mu       sync.Mutex
	acquired []imagecache.Request
	released []string
}

func (c *fakeCache) Acquire(ctx context.Context, req imagecache.Request) (imagecache.Image, error) {
	c.mu.Lock()
	c.acquired = append(c.acquired, req)
	c.mu.Unlock()
	if c.block {
		close(c.entered)
		<-ctx.Done()
		return imagecache.Image{}, ctx.Err()
	}
	if c.err != nil {
		return imagecache.Image{}, c.err
	}
	return imagecache.Image{
		Digest:  "sha256:0",
		Env:     []string{"PATH=/usr/bin:/bin", "A=b=c"},
		LinkDir: "/cache/jobs/" + req.Job + "/0",
		Layers:  []string{"0.erofs", "1.erofs"},
	}, nil
}

func (c *fakeCache) Release(_ context.Context, job string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released = append(c.released, job)
	return nil
}

// How the fake job process behaves once launched.
type behaviour int

const (
	// The VM boots and its agent answers.
	boots behaviour = iota
	// The VM exits during boot, with a console tail.
	exitsDuringBoot
	// The VM never answers.
	hangs
	// The job network fails to start.
	networkFails
)

// fakeLauncher stands in for the job launcher. Its job processes are a fake
// agent served on the spec's agent socket.
type fakeLauncher struct {
	err   error
	does  behaviour
	agent agentv1.AgentServiceServer

	mu      sync.Mutex
	specs   []*jobv1.Spec
	logs    []string
	running map[*joblaunch.Process]*fakeProcess
	kills   int
}

type fakeProcess struct {
	exited chan struct{}
	once   sync.Once
	gs     *grpc.Server
	served chan struct{}
	// dir is the agent socket's directory, which the fake binds through.
	dir *sockDir
}

func (p *fakeProcess) end() {
	p.once.Do(func() {
		if p.gs != nil {
			p.gs.Stop()
			<-p.served
		}
		// The listener unlinks its socket through the directory, so the
		// directory closes after it.
		if p.dir != nil {
			_ = p.dir.Close()
		}
		close(p.exited)
	})
}

func (l *fakeLauncher) Launch(ctx context.Context, spec *jobv1.Spec, logPath string) (*joblaunch.Process, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.specs = append(l.specs, spec)
	l.logs = append(l.logs, logPath)
	if l.err != nil {
		return nil, l.err
	}
	events := make(chan *jobv1.Event, 4)
	fp := &fakeProcess{exited: make(chan struct{})}
	p := &joblaunch.Process{Events: events, Exited: fp.exited}
	switch l.does {
	case boots:
		// Like the VM child, it binds the agent socket relative to its
		// directory, as the full path can outgrow bind's 107 bytes.
		sock := spec.GetVms()[0].GetAgentSocket()
		dir, err := openSockDir(filepath.Dir(sock))
		if err != nil {
			return nil, err
		}
		lis, err := new(net.ListenConfig).Listen(ctx, "unix", dir.path(filepath.Base(sock)))
		if err != nil {
			_ = dir.Close()
			return nil, err
		}
		fp.dir = dir
		agent := l.agent
		if agent == nil {
			agent = &fakeAgent{}
		}
		fp.gs = grpc.NewServer()
		agentv1.RegisterAgentServiceServer(fp.gs, agent)
		fp.served = make(chan struct{})
		go func() { defer close(fp.served); _ = fp.gs.Serve(lis) }()
		events <- &jobv1.Event{Event: &jobv1.Event_VmStarted{VmStarted: &jobv1.VMStarted{Pid: 42}}}
	case exitsDuringBoot:
		events <- &jobv1.Event{Event: &jobv1.Event_VmStarted{VmStarted: &jobv1.VMStarted{Pid: 42}}}
		events <- &jobv1.Event{Event: &jobv1.Event_VmExited{VmExited: &jobv1.VMExited{
			ExitCode: 1, ConsoleTail: "Kernel panic - not syncing",
		}}}
	case hangs:
	case networkFails:
		// The job process says why and exits, so its events end and Exited
		// closes, both ready by the time Start looks.
		events <- &jobv1.Event{Event: &jobv1.Event_Failed{Failed: &jobv1.Failed{Reason: "network: cannot reserve gateway IP"}}}
		close(events)
		fp.end()
	}
	if l.running == nil {
		l.running = map[*joblaunch.Process]*fakeProcess{}
	}
	l.running[p] = fp
	return p, nil
}

func (l *fakeLauncher) Kill(_ context.Context, p *joblaunch.Process) error {
	l.mu.Lock()
	fp, ok := l.running[p]
	delete(l.running, p)
	l.kills++
	l.mu.Unlock()
	if !ok {
		return errors.New("Kill of a process the launcher never started, or twice")
	}
	fp.end()
	return nil
}

// alive returns how many job processes are not killed.
func (l *fakeLauncher) alive() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.running)
}

// fakeAgent is the agent inside a fake VM.
type fakeAgent struct {
	agentv1.UnimplementedAgentServiceServer
}

func (*fakeAgent) Ping(context.Context, *agentv1.PingRequest) (*agentv1.PingResponse, error) {
	return &agentv1.PingResponse{}, nil
}

// Exec writes its argv and working directory to stdout, its env's A to
// stderr, and exits 3; a command named start-error can't start.
func (*fakeAgent) Exec(req *agentv1.ExecRequest, stream agentv1.AgentService_ExecServer) error {
	if req.GetArgv()[0] == "start-error" {
		return stream.Send(&agentv1.ExecResponse{Event: &agentv1.ExecResponse_StartError{StartError: "no such file"}})
	}
	out := strings.Join(req.GetArgv(), " ") + " in " + req.GetWorkingDir()
	for _, ev := range []*agentv1.ExecResponse{
		{Event: &agentv1.ExecResponse_Stdout{Stdout: []byte(out)}},
		{Event: &agentv1.ExecResponse_Stderr{Stderr: []byte(req.GetEnv()["A"])}},
		{Event: &agentv1.ExecResponse_ExitCode{ExitCode: 3}},
	} {
		if err := stream.Send(ev); err != nil {
			return err
		}
	}
	return nil
}

// CopyIn answers with its destination and data as an error, so the test
// can see them.
func (*fakeAgent) CopyIn(stream agentv1.AgentService_CopyInServer) error {
	var dest string
	var data []byte
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if req.GetDestination() != "" {
			dest = req.GetDestination()
		}
		data = append(data, req.GetChunk()...)
	}
	if string(data) == "ok" {
		return stream.SendAndClose(&agentv1.CopyInResponse{})
	}
	return status.Errorf(codes.InvalidArgument, "%s:%s", dest, data)
}

// CopyOut sends the path back in two chunks.
func (*fakeAgent) CopyOut(req *agentv1.CopyOutRequest, stream agentv1.AgentService_CopyOutServer) error {
	p := []byte(req.GetPath())
	for _, chunk := range [][]byte{p[:1], p[1:]} {
		if err := stream.Send(&agentv1.CopyOutResponse{Chunk: chunk}); err != nil {
			return err
		}
	}
	return nil
}
