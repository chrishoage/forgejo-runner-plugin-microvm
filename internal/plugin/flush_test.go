// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/config"
	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
)

// flushAgent is a fake agent whose Flush is counted, and fails with err,
// or, when hang is set, waits for its context to end or for release to
// close. Each Flush first reports on entered, when it is set, and calls
// flushed, when it is set.
type flushAgent struct {
	fakeAgent
	err     error
	hang    bool
	release chan struct{}
	entered chan struct{}
	flushed func()

	mu    sync.Mutex
	calls int
}

func (a *flushAgent) Flush(ctx context.Context, _ *agentv1.FlushRequest) (*agentv1.FlushResponse, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	if a.flushed != nil {
		a.flushed()
	}
	if a.entered != nil {
		a.entered <- struct{}{}
	}
	if a.hang {
		select {
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		case <-a.release:
		}
	}
	if a.err != nil {
		return nil, a.err
	}
	return &agentv1.FlushResponse{}, nil
}

func (a *flushAgent) flushes() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// storageProfiles returns a profile for each shape of storage: none,
// read-only only, a read-write mount and a read-write disk. Each disk's
// source is an empty file of t's, which admission reads the key of; the
// fake job process opens no source.
func storageProfiles(t *testing.T) config.Profiles {
	t.Helper()
	ro, rw := diskFile(t, "ro.ext4"), diskFile(t, "rw.ext4")
	return config.Profiles{
		"none": {CPUs: 1, MemoryMiB: 512, ScratchGiB: 1},
		"ro": {
			CPUs: 1, MemoryMiB: 512, ScratchGiB: 1,
			Mounts: []config.Mount{{Source: "/srv/tools", Target: "/tools"}},
			Disks:  []config.Disk{{Source: ro, Target: "/ro"}},
		},
		"rw-mount": {
			CPUs: 1, MemoryMiB: 512, ScratchGiB: 1,
			Mounts: []config.Mount{{Source: "/srv/tools", Target: "/tools"}, {Source: "/srv/data", Target: "/data", ReadWrite: true}},
		},
		"rw-disk": {
			CPUs: 1, MemoryMiB: 512, ScratchGiB: 1,
			Disks: []config.Disk{{Source: ro, Target: "/ro"}, {Source: rw, Target: "/data", ReadWrite: true}},
		},
	}
}

// flushServer returns a server on the fakes, whose job processes serve
// agent, with storageProfiles, a flush timeout of timeout, and its log in
// log.
func flushServer(t *testing.T, agent *flushAgent, timeout time.Duration) (*Server, *fakeLauncher, *logBuffer) {
	t.Helper()
	launcher := &fakeLauncher{agent: agent}
	srv := newTestServer(t, &fakeCache{}, launcher)
	srv.cfg.FlushTimeout = timeout
	srv.SetProfiles(storageProfiles(t))
	log := &logBuffer{}
	srv.log = slog.New(slog.NewTextHandler(log, nil))
	return srv, launcher, log
}

// startedOn creates a job on srv with profile and starts it, and returns its
// id.
func startedOn(t *testing.T, srv *Server, profile string) string {
	t.Helper()
	id := createOn(t, srv, testImage+";profile="+profile)
	if err := srv.Start(&pluginv1alpha.StartRequest{EnvironmentId: id}, &progressStream{ctx: testContext(t), data: func() error { return nil }}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return id
}

func remove(t *testing.T, srv *Server, id string) {
	t.Helper()
	if _, err := srv.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

// Remove flushes a VM with a read-write mount or disk before it kills the
// job process, and doesn't flush one without: none, or only read-only
// storage (STO-9).
func TestRemoveFlushesReadWriteStorage(t *testing.T) {
	t.Parallel()
	for profile, want := range map[string]int{"none": 0, "ro": 0, "rw-mount": 1, "rw-disk": 1} {
		t.Run(profile, func(t *testing.T) {
			t.Parallel()
			agent := &flushAgent{}
			srv, launcher, log := flushServer(t, agent, 5*time.Second)
			var killedFirst atomic.Bool
			agent.flushed = func() {
				launcher.mu.Lock()
				killedFirst.Store(launcher.kills > 0)
				launcher.mu.Unlock()
			}
			id := startedOn(t, srv, profile)
			remove(t, srv, id)
			if got := agent.flushes(); got != want {
				t.Errorf("Remove flushed %d times, want %d", got, want)
			}
			if killedFirst.Load() {
				t.Error("Remove killed the job process before it flushed the VM")
			}
			if launcher.alive() != 0 {
				t.Error("the job process outlived Remove")
			}
			if flushed := strings.Contains(log.String(), `msg=flushed env=`+id); flushed != (want == 1) {
				t.Errorf("the log says flushed: %v, want %v:\n%s", flushed, want == 1, log)
			}
		})
	}
}

// A flush that fails, that the agent's absence fails, or that outlives its
// limit, is logged, naming the job and the error, and Remove still kills
// the job process and succeeds. With no agent, the flush fails at once
// with Unavailable rather than waiting for one: its limit there is longer
// than the test waits for Remove.
func TestRemoveSucceedsWhenTheFlushFails(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		agent   *flushAgent
		timeout time.Duration
		// noAgent stops the fake's agent, its listener and its socket
		// with it, before Remove, while its VM is not reported to exit.
		noAgent bool
		want    string
	}{
		"fails":    {agent: &flushAgent{err: status.Error(codes.FailedPrecondition, "flush: disk /data: freeze /data: input/output error")}, timeout: 200 * time.Millisecond, want: "freeze /data: input/output error"},
		"hangs":    {agent: &flushAgent{hang: true}, timeout: 200 * time.Millisecond, want: "DeadlineExceeded"},
		"vanishes": {agent: &flushAgent{}, timeout: 15 * time.Second, noAgent: true, want: "Unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, launcher, log := flushServer(t, tc.agent, tc.timeout)
			id := startedOn(t, srv, "rw-disk")
			if tc.noAgent {
				launcher.breakAgent(t, id)
			}
			removed := make(chan error, 1)
			ctx := testContext(t)
			go func() {
				_, err := srv.Remove(ctx, &pluginv1alpha.RemoveRequest{EnvironmentId: id})
				removed <- err
			}()
			if err := within(t, removed, "Remove"); err != nil {
				t.Fatalf("Remove = %v, want success", err)
			}
			if launcher.alive() != 0 {
				t.Error("the job process outlived Remove")
			}
			if n := tc.agent.flushes(); tc.noAgent && n != 0 {
				t.Errorf("a stopped agent answered %d flushes", n)
			}
			line := ""
			for l := range strings.SplitSeq(log.String(), "\n") {
				if strings.Contains(l, "flush failed") {
					line = l
				}
			}
			if !strings.Contains(line, "env="+id) || !strings.Contains(line, tc.want) {
				t.Errorf("no log line of the failed flush naming %s and %q:\n%s", id, tc.want, log)
			}
		})
	}
}

// A failed Start kills its VM at once, with no flush, and Remove after it
// flushes nothing: the VM is gone.
func TestFailedStartDoesNotFlush(t *testing.T) {
	t.Parallel()
	agent := &flushAgent{}
	srv, launcher, _ := flushServer(t, agent, 5*time.Second)
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	stream := &progressStream{ctx: ctx, data: func() error { return nil }, complete: func() error {
		cancel()
		return status.Error(codes.Canceled, "the runner went away")
	}}
	id := createOn(t, srv, testImage+";profile=rw-disk")
	if err := srv.Start(&pluginv1alpha.StartRequest{EnvironmentId: id}, stream); status.Code(err) != codes.Canceled {
		t.Fatalf("Start = %v, want Canceled", err)
	}
	if launcher.alive() != 0 {
		t.Error("the failed Start left its job process running")
	}
	remove(t, srv, id)
	if n := agent.flushes(); n != 0 {
		t.Errorf("a failed Start and its Remove flushed %d times, want none", n)
	}
}

// Remove of a job whose VM has exited makes no flush call, even while its
// agent's socket still answers: the plugin knows the VM is gone.
func TestRemoveSkipsTheFlushOfAnExitedVM(t *testing.T) {
	t.Parallel()
	agent := &flushAgent{}
	srv, launcher, _ := flushServer(t, agent, 5*time.Second)
	id := startedOn(t, srv, "rw-mount")
	srv.mu.Lock()
	j := srv.jobs[id]
	srv.mu.Unlock()
	launcher.report(t, id, &jobv1.Event{Event: &jobv1.Event_VmExited{VmExited: &jobv1.VMExited{ExitCode: 1}}})
	waitFor(t, "the job marked gone", func() bool { return j.gone.Err() != nil })
	remove(t, srv, id)
	if n := agent.flushes(); n != 0 {
		t.Errorf("Remove flushed an exited VM %d times, want none", n)
	}
}

// The backstop removes through Remove's path, so it flushes too
// (PROTO-14).
func TestBackstopFlushes(t *testing.T) {
	t.Parallel()
	agent := &flushAgent{}
	srv, launcher, _ := flushServer(t, agent, 5*time.Second)
	srv.cfg.BackstopDelay = time.Millisecond
	resp, err := srv.Create(testContext(t), &pluginv1alpha.CreateRequest{
		LabelArg: testImage + ";profile=rw-disk", EnvironmentTimeout: durationpb.New(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	id := resp.GetEnvironmentId()
	if err := srv.Start(&pluginv1alpha.StartRequest{EnvironmentId: id}, &progressStream{ctx: testContext(t), data: func() error { return nil }}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the backstop's removal", func() bool { return !inTable(srv, id) && launcher.alive() == 0 })
	if n := agent.flushes(); n != 1 {
		t.Errorf("the backstop flushed %d times, want 1", n)
	}
}

// Close, as the plugin stops, removes through Remove's path, so it flushes
// too.
func TestCloseFlushes(t *testing.T) {
	t.Parallel()
	agent := &flushAgent{}
	srv, launcher, _ := flushServer(t, agent, 5*time.Second)
	startedOn(t, srv, "rw-disk")
	if err := srv.Close(testContext(t)); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := agent.flushes(); n != 1 {
		t.Errorf("Close flushed %d times, want 1", n)
	}
	if launcher.alive() != 0 {
		t.Error("a job process outlived Close")
	}
}

// Close, as at shutdown, flushes every job's VM at once, so one whose flush
// hangs costs its own flush timeout, not every job's: both flushes are in
// flight together.
func TestCloseFlushesEveryJobAtOnce(t *testing.T) {
	t.Parallel()
	agent := &flushAgent{hang: true, release: make(chan struct{}), entered: make(chan struct{}, 2)}
	srv, launcher, _ := flushServer(t, agent, 20*time.Second)
	startedOn(t, srv, "rw-disk")
	startedOn(t, srv, "rw-mount")
	closed := make(chan error, 1)
	ctx := testContext(t)
	go func() { closed <- srv.Close(ctx) }()
	within(t, agent.entered, "the first flush")
	within(t, agent.entered, "the second flush, while the first is in flight")
	close(agent.release)
	if err := within(t, closed, "Close"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if launcher.alive() != 0 {
		t.Error("a job process outlived Close")
	}
}

// A Remove whose flush is in flight when the plugin stops, its call
// cancelled as the gRPC server's stop cancels every call, still finishes
// its flush and its kill, and Close waits for them: the plugin's liveness
// pipe closes only after, so a job that ended normally keeps its last
// writes (STO-9). The cancelled call returns at once.
func TestCloseWaitsForARemoveInFlight(t *testing.T) {
	t.Parallel()
	agent := &flushAgent{hang: true, release: make(chan struct{}), entered: make(chan struct{}, 1)}
	srv, launcher, log := flushServer(t, agent, 20*time.Second)
	id := startedOn(t, srv, "rw-disk")
	ctx, cancel := context.WithCancel(testContext(t))
	removed := make(chan error, 1)
	go func() {
		_, err := srv.Remove(ctx, &pluginv1alpha.RemoveRequest{EnvironmentId: id})
		removed <- err
	}()
	within(t, agent.entered, "the flush")
	cancel()
	if err := within(t, removed, "the cancelled Remove"); status.Code(err) != codes.Canceled {
		t.Fatalf("the cancelled Remove = %v, want Canceled", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- srv.Close(testContext(t)) }()
	close(agent.release)
	if err := within(t, closed, "Close"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.Contains(log.String(), `msg=flushed env=`+id) {
		t.Errorf("Close returned before the flush of the cancelled Remove succeeded:\n%s", log)
	}
	if launcher.alive() != 0 {
		t.Error("the job process outlived Close")
	}
}
