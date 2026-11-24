// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
	arch, err := runnerArch(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if cache.dir == "" {
		cache.dir = dirs.Cache
	}
	srv, err := NewServer(discardLog(), Config{
		Arch:      arch,
		Dirs:      dirs,
		JobBinary: "/fake/runner-microvm-job",
		MkfsExt4:  "/fake/mkfs.ext4",
		LibDir:    "/fake/lib",
		// Not the built-ins, so the test sees them passed on.
		LibkrunLogLevel: "debug",
		LogLevel:        "warn",
		Agent:           agent,
		Cache:           cache,
		Launcher:        launcher,
		ReadyTimeout:    5 * time.Second,
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

// fakeCache stands in for the image cache. Like the real one, it makes each
// VM's layer link directory, <dir>/jobs/<id>/<n>/, and Release deletes the
// job's.
type fakeCache struct {
	// dir is the cache directory; newTestServer sets it.
	dir string
	// err fails Acquire.
	err error
	// block makes Acquire send its job on entered, then wait for its
	// context, or for release to close when it is set.
	block   bool
	entered chan string
	release chan struct{}
	// unwind, when set, makes a cancelled Acquire wait for it to close
	// before it returns, as a call still unwinding.
	unwind chan struct{}
	// releasing, when set, makes Release send its job on it, then wait
	// for unrelease to close, as a removal still tearing its job down.
	releasing chan string
	unrelease chan struct{}

	mu       sync.Mutex
	acquired []imagecache.Request
	released []string
}

func (c *fakeCache) Acquire(ctx context.Context, req imagecache.Request) (imagecache.Image, error) {
	c.mu.Lock()
	c.acquired = append(c.acquired, req)
	c.mu.Unlock()
	if c.block {
		select {
		case c.entered <- req.Job:
		case <-ctx.Done():
			return imagecache.Image{}, ctx.Err()
		}
		select {
		case <-ctx.Done():
			if c.unwind != nil {
				<-c.unwind
			}
			return imagecache.Image{}, ctx.Err()
		case <-c.release:
		}
	}
	if c.err != nil {
		return imagecache.Image{}, c.err
	}
	link := filepath.Join(c.dir, "jobs", req.Job, strconv.Itoa(req.VM))
	if err := os.MkdirAll(link, 0o700); err != nil {
		return imagecache.Image{}, err
	}
	for _, l := range []string{"0.erofs", "1.erofs"} {
		if err := os.WriteFile(filepath.Join(link, l), nil, 0o600); err != nil {
			return imagecache.Image{}, err
		}
	}
	return imagecache.Image{
		Digest:     "sha256:0",
		Env:        []string{"PATH=/usr/bin:/bin", "A=b=c"},
		User:       "builder:staff",
		WorkingDir: "/src",
		LinkDir:    link,
		Layers:     []string{"0.erofs", "1.erofs"},
		Pulled:     1,
	}, nil
}

func (c *fakeCache) Release(_ context.Context, job string) error {
	if c.releasing != nil {
		c.releasing <- job
		<-c.unrelease
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released = append(c.released, job)
	return os.RemoveAll(filepath.Join(c.dir, "jobs", job))
}

// How the fake job process behaves once launched.
type behaviour int

const (
	// The VM boots and its agent answers.
	boots behaviour = iota
	// The VM exits during boot, with the tails of bootConsole and
	// bootLog.
	exitsDuringBoot
	// The VM never answers; its console and VM log files hold
	// bootConsole and bootLog.
	hangs
	// The job network fails to start.
	networkFails
	// The job process fails to make the scratch disk.
	scratchFails
	// The job process exits during boot, saying nothing; the console and
	// VM log files hold bootConsole and bootLog.
	exitsSilently
	// The job process can't read its spec: the plugin's own bug.
	specUnreadable
)

// What a fake VM that fails to boot wrote: a console of 25 short lines,
// and a VM log of 10 lines of 1000 bytes, more than the 4 KiB an error
// carries.
var (
	bootConsole = func() string {
		var b strings.Builder
		for i := range 25 {
			fmt.Fprintf(&b, "console line %d\n", i+1)
		}
		return b.String()
	}()
	bootLog = func() string {
		var b strings.Builder
		for i := range 10 {
			line := fmt.Sprintf("log line %d ", i+1)
			b.WriteString(line + strings.Repeat("-", 999-len(line)) + "\n")
		}
		return b.String()
	}()
)

// writeBootOutput writes bootConsole and bootLog to vm's console and VM
// log files, as its VM child does.
func writeBootOutput(vm *jobv1.VM) error {
	if err := os.WriteFile(vm.GetConsole(), []byte(bootConsole), 0o600); err != nil {
		return err
	}
	return os.WriteFile(vm.GetVmLog(), []byte(bootLog), 0o600)
}

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
	byJob   map[string]*fakeProcess
	kills   int
}

type fakeProcess struct {
	events chan *jobv1.Event
	exited chan struct{}
	once   sync.Once
	gs     *grpc.Server
	served chan struct{}
	// dir is the agent socket's directory, which the fake binds through.
	dir *sockDir
}

// end ends the fake job process: its agent goes, then, after the events
// last says, its events end and it exits, as a job process does.
func (p *fakeProcess) end(last ...*jobv1.Event) {
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
		for _, ev := range last {
			p.events <- ev
		}
		close(p.events)
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
	fp := &fakeProcess{events: events, exited: make(chan struct{})}
	p := &joblaunch.Process{Events: events, Exited: fp.exited}
	switch l.does {
	case boots:
		// Like the job process and its VM child, it writes the job
		// process log and the VM's scratch disk, VM log and console.
		vm := spec.GetVms()[0]
		for _, f := range []string{logPath, vm.GetScratchDisk(), vm.GetVmLog(), vm.GetConsole()} {
			if err := os.WriteFile(f, nil, 0o600); err != nil {
				return nil, err
			}
		}
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
		// The job process's tails are the files' last 4 KiB.
		events <- &jobv1.Event{Event: &jobv1.Event_VmStarted{VmStarted: &jobv1.VMStarted{Pid: 42}}}
		events <- &jobv1.Event{Event: &jobv1.Event_VmExited{VmExited: &jobv1.VMExited{
			ExitCode: 1, ConsoleTail: bootConsole, LogTail: bootLog[len(bootLog)-4096:],
		}}}
	case hangs:
		if err := writeBootOutput(spec.GetVms()[0]); err != nil {
			return nil, err
		}
	case networkFails:
		// The job process says why and exits, so its events end and Exited
		// closes, both ready by the time Start looks.
		fp.end(&jobv1.Event{Event: &jobv1.Event_Failed{Failed: &jobv1.Failed{Reason: "network: cannot reserve gateway IP"}}})
	case scratchFails:
		fp.end(&jobv1.Event{Event: &jobv1.Event_Failed{Failed: &jobv1.Failed{
			Reason: "scratch disk: " + spec.GetVms()[0].GetScratchDisk() + ": no space left on device",
		}}})
	case specUnreadable:
		fp.end(&jobv1.Event{Event: &jobv1.Event_Failed{Failed: &jobv1.Failed{
			Reason: "read the job spec: unexpected EOF", Bug: true,
		}}})
	case exitsSilently:
		if err := writeBootOutput(spec.GetVms()[0]); err != nil {
			return nil, err
		}
		fp.end()
	}
	if l.running == nil {
		l.running = map[*joblaunch.Process]*fakeProcess{}
		l.byJob = map[string]*fakeProcess{}
	}
	l.running[p] = fp
	l.byJob[spec.GetJob()] = fp
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

// die makes the job process of the job id end on its own, as when its VM
// exits: its agent's connection breaks, then it reports last, and exits. It
// stays the launcher's to Kill, which is then no error.
func (l *fakeLauncher) die(t *testing.T, id string, last ...*jobv1.Event) {
	t.Helper()
	l.mu.Lock()
	fp := l.byJob[id]
	l.mu.Unlock()
	if fp == nil {
		t.Fatalf("no job process of %s was launched", id)
	}
	fp.end(last...)
}

// breakAgent breaks the connection to the agent of the job id's job process
// without ending the process: its VM is not reported to exit.
func (l *fakeLauncher) breakAgent(t *testing.T, id string) {
	t.Helper()
	l.mu.Lock()
	fp := l.byJob[id]
	l.mu.Unlock()
	if fp == nil || fp.gs == nil {
		t.Fatalf("no agent of %s was served", id)
	}
	fp.gs.Stop()
	<-fp.served
}

// report makes the job process of the job id report ev while its agent
// still serves: the order in which a VM whose agent failed first, then
// exited, is seen.
func (l *fakeLauncher) report(t *testing.T, id string, ev *jobv1.Event) {
	t.Helper()
	l.mu.Lock()
	fp := l.byJob[id]
	l.mu.Unlock()
	if fp == nil {
		t.Fatalf("no job process of %s was launched", id)
	}
	fp.events <- ev
}

// launched returns how many job processes were launched.
func (l *fakeLauncher) launched() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.specs)
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
