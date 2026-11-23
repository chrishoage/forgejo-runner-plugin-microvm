// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
)

// jobFiles lists every path under the state, runtime and cache directories
// that belongs to the job id: one whose path names it.
func jobFiles(t *testing.T, srv *Server, id string) []string {
	t.Helper()
	var found []string
	for _, dir := range []string{srv.cfg.Dirs.State, srv.cfg.Dirs.Runtime, srv.cfg.Dirs.Cache} {
		err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if strings.Contains(path, id) {
				found = append(found, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return found
}

// Remove of an unknown id fails with NotFound, and Remove of a started job
// leaves none of its files under the state, runtime or cache directories
// (PROTO-12).
func TestRemoveDeletesEveryFileOfTheJob(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeCache{}, &fakeLauncher{})
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	_, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: "job-00000000"})
	wantCode(t, err, codes.NotFound)

	id := create(t, c)
	if _, err := start(testContext(t), c, id); err != nil {
		t.Fatal(err)
	}
	before := jobFiles(t, srv, id)
	// The job's own directories in each of the three, and the files the
	// plugin, the job process and the image cache made in them.
	for _, want := range []string{
		filepath.Join(srv.cfg.Dirs.State, "jobs", id, "job.log"),
		filepath.Join(srv.cfg.Dirs.State, "jobs", id, "0", "scratch.ext4"),
		filepath.Join(srv.cfg.Dirs.State, "jobs", id, "0", "console"),
		filepath.Join(srv.cfg.Dirs.State, "jobs", id, "0", "root", ".runner-microvm-boot"),
		filepath.Join(srv.cfg.Dirs.Runtime, id, "0", "agent.sock"),
		filepath.Join(srv.cfg.Dirs.Cache, "jobs", id, "0", "0.erofs"),
	} {
		if !strings.Contains(strings.Join(before, "\n")+"\n", want+"\n") {
			t.Errorf("a started job has no %s; it has %v", want, before)
		}
	}
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatal(err)
	}
	if left := jobFiles(t, srv, id); len(left) != 0 {
		t.Errorf("Remove left %v", left)
	}
}

// startStream is the runner's side of a Start, whose context the test
// cancels.
type startStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *startStream) Context() context.Context { return s.ctx }

func (*startStream) Send(*pluginv1alpha.StartOutput) error { return nil }

// unwindingAgent's Exec blocks until its call is cancelled, after sending on
// entered, then until unwind closes: an Exec the agent is still unwinding.
// It closes returned as it returns.
type unwindingAgent struct {
	fakeAgent
	entered  chan struct{}
	unwind   chan struct{}
	returned chan struct{}
}

func (a *unwindingAgent) Exec(_ *agentv1.ExecRequest, stream agentv1.AgentService_ExecServer) error {
	defer close(a.returned)
	a.entered <- struct{}{}
	<-stream.Context().Done()
	<-a.unwind
	return stream.Context().Err()
}

// Remove succeeds while a cancelled Start, and while a cancelled Exec, of
// the same job is still unwinding, and neither call reports an internal
// error (PROTO-13). The runner cancels a call's stream and sends Remove at
// once.
//
// A cancelled Exec's unwinding is the agent's: Remove returns while the
// agent's Exec has not. A cancelled Start's is the image cache's Acquire,
// and Remove waits for it, by design: Release must wait for an Acquire of
// the job before it deletes the job's link directories (IMG-14), and Remove
// must end any job process the Start launched before it returns (PROTO-12).
// So Remove arriving mid-unwind is held, not refused, and succeeds.
func TestRemoveWhileACancelledCallUnwinds(t *testing.T) {
	t.Parallel()
	t.Run("Start", func(t *testing.T) {
		t.Parallel()
		// The cancelled Start's Acquire returns only once unwind closes.
		cache := &fakeCache{block: true, entered: make(chan string, 1), unwind: make(chan struct{})}
		launcher := &fakeLauncher{}
		srv := newTestServer(t, cache, launcher)
		c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
		id := create(t, c)
		ctx, cancel := context.WithCancel(testContext(t))
		defer cancel()
		stream := &startStream{ctx: ctx}
		started := make(chan error, 1)
		go func() { started <- srv.Start(&pluginv1alpha.StartRequest{EnvironmentId: id}, stream) }()
		within(t, cache.entered, "Start reaching the image cache")
		cancel()
		removed := make(chan error, 1)
		go func() {
			_, err := srv.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id})
			removed <- err
		}()
		// Remove has the job out of the table while Start still unwinds.
		deadline := time.Now().Add(10 * time.Second)
		for inTable(srv, id) {
			if time.Now().After(deadline) {
				t.Fatal("Remove did not begin")
			}
			time.Sleep(time.Millisecond)
		}
		select {
		case err := <-started:
			t.Fatalf("Start returned %v before its Acquire unwound", err)
		case err := <-removed:
			t.Fatalf("Remove returned %v before the Acquire it must wait for unwound", err)
		default:
		}
		close(cache.unwind)
		if err := within(t, removed, "Remove"); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if err := within(t, started, "Start's return"); status.Code(err) != codes.Canceled {
			t.Errorf("the cancelled Start returned %v, want Canceled", err)
		}
		if len(launcher.specs) != 0 {
			t.Error("a job process was launched for a cancelled Start")
		}
	})
	t.Run("Exec", func(t *testing.T) {
		t.Parallel()
		agent := &unwindingAgent{entered: make(chan struct{}, 1), unwind: make(chan struct{}), returned: make(chan struct{})}
		launcher := &fakeLauncher{agent: agent}
		srv := newTestServer(t, &fakeCache{}, launcher)
		c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
		id := create(t, c)
		if _, err := start(testContext(t), c, id); err != nil {
			t.Fatal(err)
		}
		// The agent's Exec unwinds only once the test ends.
		t.Cleanup(func() { close(agent.unwind) })
		ctx, cancel := context.WithCancel(testContext(t))
		defer cancel()
		stream := newStalledStream(ctx, nil)
		execed := make(chan error, 1)
		go func() {
			execed <- srv.Exec(&pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"sleep", "600"}}, stream)
		}()
		within(t, agent.entered, "Exec reaching the agent")
		cancel()
		if _, err := srv.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		select {
		case <-agent.returned:
			t.Fatal("the agent's Exec returned before Remove did, so Remove was not shown to return mid-unwind")
		default:
		}
		if err := within(t, execed, "Exec's return"); status.Code(err) != codes.Canceled {
			t.Errorf("the cancelled Exec returned %v, want Canceled", err)
		}
		if launcher.alive() != 0 {
			t.Error("the job process outlived Remove")
		}
	})
}

// inTable says whether id is in srv's job table.
func inTable(srv *Server, id string) bool {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	_, ok := srv.jobs[id]
	return ok
}

// Two Starts of different jobs are both in progress at once: no lock spans
// jobs (PROTO-16). Each Start's Acquire returns only once both have begun,
// so Starts that ran one at a time would never end.
func TestStartsOfDifferentJobsRunInParallel(t *testing.T) {
	t.Parallel()
	cache := &fakeCache{block: true, entered: make(chan string, 2), release: make(chan struct{})}
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, cache, &fakeLauncher{})))
	ids := []string{create(t, c), create(t, c)}
	started := make(chan error, len(ids))
	for _, id := range ids {
		go func() {
			_, err := start(testContext(t), c, id)
			started <- err
		}()
	}
	in := map[string]bool{}
	for range ids {
		in[within(t, cache.entered, "both Starts in progress at once")] = true
	}
	if !in[ids[0]] || !in[ids[1]] {
		t.Fatalf("Starts in progress: %v, want %v", in, ids)
	}
	close(cache.release)
	for range ids {
		if err := within(t, started, "Start"); err != nil {
			t.Error(err)
		}
	}
}

// hangingAgent's Exec sends on entered, then blocks until its call ends.
type hangingAgent struct {
	fakeAgent
	entered chan struct{}
}

func (a *hangingAgent) Exec(_ *agentv1.ExecRequest, stream agentv1.AgentService_ExecServer) error {
	a.entered <- struct{}{}
	<-stream.Context().Done()
	return stream.Context().Err()
}

// startedWith returns a client, its server, and a started job's id, on a
// fake job process serving agent.
func startedWith(t *testing.T, launcher *fakeLauncher, lostAgentGrace time.Duration) (pluginv1alpha.BackendPluginClient, string) {
	t.Helper()
	srv := newTestServer(t, &fakeCache{}, launcher)
	if lostAgentGrace != 0 {
		srv.cfg.LostAgentGrace = lostAgentGrace
	}
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	id := create(t, c)
	if _, err := start(testContext(t), c, id); err != nil {
		t.Fatal(err)
	}
	return c, id
}

// wantGone fails t unless err is FailedPrecondition with the message want.
func wantGone(t *testing.T, what string, err error, want string) {
	t.Helper()
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != want {
		t.Errorf("%s: %v; want FailedPrecondition: %s", what, err, want)
	}
}

// Once the job process reports that the job's VM exited, an Exec in flight
// and every later Exec, CopyIn and CopyOut fail with FailedPrecondition
// naming the VM, its exit code and its console tail, not with the broken
// connection's Unavailable. A job process that fails, or exits with
// neither said, fails them the same way, naming the VM. Remove then works
// as usual.
func TestCallsOnAJobWhoseVMDied(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		last []*jobv1.Event
		want string
	}{
		"VM exits": {
			[]*jobv1.Event{{Event: &jobv1.Event_VmExited{VmExited: &jobv1.VMExited{
				ExitCode: 1, ConsoleTail: "Kernel panic - not syncing: Attempted to kill init!\n", LogTail: "the VM log",
			}}}},
			"VM job exited (1): Kernel panic - not syncing: Attempted to kill init!",
		},
		"job process fails": {
			[]*jobv1.Event{{Event: &jobv1.Event_Failed{Failed: &jobv1.Failed{Reason: "the job network stopped"}}}},
			"VM job: its job process failed: the job network stopped",
		},
		"job process exits": {nil, "VM job: its job process exited"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			agent := &hangingAgent{entered: make(chan struct{}, 1)}
			launcher := &fakeLauncher{agent: agent}
			c, id := startedWith(t, launcher, 0)
			inFlight := make(chan error, 1)
			go func() {
				_, err := execAll(t, c, &pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"sleep", "600"}})
				inFlight <- err
			}()
			within(t, agent.entered, "Exec reaching the agent")
			launcher.die(t, id, tc.last...)
			wantGone(t, "the Exec in flight", within(t, inFlight, "the Exec in flight"), tc.want)

			_, err := execAll(t, c, &pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"true"}})
			wantGone(t, "a later Exec", err, tc.want)
			dest := "/var/run/act/"
			err = copyInAll(t, c, &pluginv1alpha.CopyInChunk{EnvironmentId: &id, DestPath: &dest, Data: []byte("ok")})
			wantGone(t, "a later CopyIn", err, tc.want)
			_, err = copyOutAll(t, c, id, "/x")
			wantGone(t, "a later CopyOut", err, tc.want)

			if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
				t.Fatalf("Remove: %v", err)
			}
			if launcher.alive() != 0 {
				t.Error("the job process outlived Remove")
			}
		})
	}
}

// A connection to the agent that breaks while the job process reports no
// exit fails the call, after the lost agent grace, with FailedPrecondition
// naming the VM, not the broken connection's Unavailable.
func TestCallWhoseAgentConnectionBroke(t *testing.T) {
	t.Parallel()
	agent := &hangingAgent{entered: make(chan struct{}, 1)}
	launcher := &fakeLauncher{agent: agent}
	c, id := startedWith(t, launcher, 100*time.Millisecond)
	inFlight := make(chan error, 1)
	go func() {
		_, err := execAll(t, c, &pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"sleep", "600"}})
		inFlight <- err
	}()
	within(t, agent.entered, "Exec reaching the agent")
	launcher.breakAgent(t, id)
	err := within(t, inFlight, "the Exec in flight")
	if status.Code(err) != codes.FailedPrecondition ||
		!strings.HasPrefix(status.Convert(err).Message(), "VM job: the connection to its agent broke, and the VM has not exited: ") {
		t.Errorf("Exec = %v; want FailedPrecondition naming the VM and the broken connection", err)
	}
}

// failingAgent's Exec sends on entered, then, once fail closes, fails with
// err, or with err nil ends without an exit code. Once dying closes, its
// Ping fails, as a dying VM's agent does, after sending on pinged.
type failingAgent struct {
	fakeAgent
	err     error
	entered chan struct{}
	fail    chan struct{}
	dying   chan struct{}
	pinged  chan struct{}
}

func newFailingAgent(err error) *failingAgent {
	return &failingAgent{
		err: err, entered: make(chan struct{}, 1), fail: make(chan struct{}),
		dying: make(chan struct{}), pinged: make(chan struct{}, 1),
	}
}

func (a *failingAgent) Ping(context.Context, *agentv1.PingRequest) (*agentv1.PingResponse, error) {
	select {
	case <-a.dying:
		select {
		case a.pinged <- struct{}{}:
		default:
		}
		return nil, status.Error(codes.Unavailable, "the agent is gone")
	default:
		return &agentv1.PingResponse{}, nil
	}
}

func (a *failingAgent) Exec(_ *agentv1.ExecRequest, _ agentv1.AgentService_ExecServer) error {
	a.entered <- struct{}{}
	<-a.fail
	return a.err
}

// A call whose agent fails other than with Unavailable, here with the
// Internal of a reset stream or by ending Exec without an exit code, while
// its VM dies, fails as one whose connection broke does: with the VM's
// exit, once the job process reports it.
func TestCallWhoseAgentFailsAsItsVMDies(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"reset stream": status.Error(codes.Internal, "stream terminated by RST_STREAM with error code: PROTOCOL_ERROR"),
		"no exit code": nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			agent := newFailingAgent(err)
			launcher := &fakeLauncher{agent: agent}
			c, id := startedWith(t, launcher, time.Minute)
			inFlight := make(chan error, 1)
			go func() {
				_, err := execAll(t, c, &pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"sleep", "600"}})
				inFlight <- err
			}()
			within(t, agent.entered, "Exec reaching the agent")
			close(agent.dying)
			close(agent.fail)
			// The plugin checks the agent before it waits for the VM's exit.
			select {
			case <-agent.pinged:
			case err := <-inFlight:
				t.Fatalf("the Exec returned %v without waiting for its VM's exit", err)
			case <-time.After(10 * time.Second):
				t.Fatal("the plugin did not check the agent within 10s")
			}
			launcher.report(t, id, &jobv1.Event{Event: &jobv1.Event_VmExited{VmExited: &jobv1.VMExited{
				ExitCode: 1, ConsoleTail: "Kernel panic - not syncing: Attempted to kill init!\n",
			}}})
			wantGone(t, "the Exec in flight", within(t, inFlight, "the Exec in flight"),
				"VM job exited (1): Kernel panic - not syncing: Attempted to kill init!")
		})
	}
}

// A call whose agent fails while the agent still answers fails at once,
// without the lost agent grace, with the agent's own error, or Internal
// naming the VM for an Exec that ended without an exit code: never
// Unavailable.
func TestCallWhoseLiveAgentFails(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"agent's error": {
			status.Error(codes.Internal, "copy out /x: permission denied"),
			status.Error(codes.Internal, "copy out /x: permission denied"),
		},
		"no exit code": {nil, status.Error(codes.Internal, "VM job: its agent ended Exec without an exit code")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			agent := newFailingAgent(tc.err)
			close(agent.fail)
			launcher := &fakeLauncher{agent: agent}
			// within's 10 s is far below the grace.
			c, id := startedWith(t, launcher, time.Hour)
			done := make(chan error, 1)
			go func() {
				_, err := execAll(t, c, &pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"true"}})
				done <- err
			}()
			err := within(t, done, "the Exec")
			if status.Code(err) != status.Code(tc.want) || status.Convert(err).Message() != status.Convert(tc.want).Message() {
				t.Errorf("Exec = %v; want %v", err, tc.want)
			}
		})
	}
}

// Exec, CopyIn and CopyOut made while Start is still in progress, here
// blocked in its pull, fail at once with FailedPrecondition: they never
// wait for the job's lock, so none runs once the Start succeeds (PROTO-11).
func TestCallsDuringStartFailAtOnce(t *testing.T) {
	t.Parallel()
	cache := &fakeCache{block: true, entered: make(chan string, 1), release: make(chan struct{})}
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, cache, &fakeLauncher{})))
	id := create(t, c)
	started := make(chan error, 1)
	go func() {
		_, err := start(testContext(t), c, id)
		started <- err
	}()
	within(t, cache.entered, "Start reaching the image cache")
	dest := "/var/run/act/"
	for name, call := range map[string]func() error{
		"Exec": func() error {
			_, err := execAll(t, c, &pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"true"}})
			return err
		},
		"CopyIn": func() error {
			return copyInAll(t, c, &pluginv1alpha.CopyInChunk{EnvironmentId: &id, DestPath: &dest, Data: []byte("ok")})
		},
		"CopyOut": func() error {
			_, err := copyOutAll(t, c, id, "/x")
			return err
		},
	} {
		returned := make(chan error, 1)
		go func() { returned <- call() }()
		if err := within(t, returned, name+" during Start"); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("%s during Start returned %v, want FailedPrecondition", name, err)
		}
	}
	close(cache.release)
	if err := within(t, started, "Start"); err != nil {
		t.Fatal(err)
	}
}

// copyOutStream is the runner's side of a CopyOut, whose context the test
// sets.
type copyOutStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *copyOutStream) Context() context.Context { return s.ctx }

func (*copyOutStream) Send(*pluginv1alpha.CopyOutChunk) error { return nil }

// A call whose context ends while it waits for the job's lock, held here by
// an Exec the agent is still running, returns its context's code at once:
// no wait for the lock outlives the call's context.
func TestCancelledCallStopsWaitingForTheJob(t *testing.T) {
	t.Parallel()
	agent := &unwindingAgent{entered: make(chan struct{}, 1), unwind: make(chan struct{}), returned: make(chan struct{})}
	srv := newTestServer(t, &fakeCache{}, &fakeLauncher{agent: agent})
	// Runs before the server's Close, which waits for the agent's Exec.
	t.Cleanup(func() { close(agent.unwind) })
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	id := create(t, c)
	if _, err := start(testContext(t), c, id); err != nil {
		t.Fatal(err)
	}
	holding, release := context.WithCancel(testContext(t))
	t.Cleanup(release)
	holder := newStalledStream(holding, nil)
	go func() {
		_ = srv.Exec(&pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"sleep", "600"}}, holder)
	}()
	within(t, agent.entered, "the first Exec reaching the agent")

	cancelled, cancel := context.WithCancel(testContext(t))
	cancel()
	dest := "/var/run/act/"
	execStream := newStalledStream(cancelled, nil)
	copyInStream := newStalledStream(cancelled, &pluginv1alpha.CopyInChunk{EnvironmentId: &id, DestPath: &dest})
	copyOut := &copyOutStream{ctx: cancelled}
	startS := &startStream{ctx: cancelled}
	for name, call := range map[string]func() error{
		"Exec": func() error {
			return srv.Exec(&pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"true"}}, execStream)
		},
		"CopyIn": func() error { return srv.CopyIn(copyInStream) },
		"CopyOut": func() error {
			return srv.CopyOut(&pluginv1alpha.CopyOutRequest{EnvironmentId: id, SrcPath: "/x"}, copyOut)
		},
		"Start": func() error { return srv.Start(&pluginv1alpha.StartRequest{EnvironmentId: id}, startS) },
	} {
		returned := make(chan error, 1)
		go func() { returned <- call() }()
		if err := within(t, returned, "a cancelled "+name+" waiting for the job"); status.Code(err) != codes.Canceled {
			t.Errorf("a cancelled %s waiting for the job returned %v, want Canceled", name, err)
		}
	}
}
