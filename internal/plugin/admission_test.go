// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/admission"
	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
)

// runningStart is a Start in progress through a client: its progress
// lines as they arrive, and its result once it ends.
type runningStart struct {
	lines chan string
	done  chan error
}

// startInBackground starts id on c with ctx, and returns it running.
func startInBackground(ctx context.Context, c pluginv1alpha.BackendPluginClient, id string) *runningStart {
	r := &runningStart{lines: make(chan string, 16), done: make(chan error, 1)}
	go func() {
		stream, err := c.Start(ctx, &pluginv1alpha.StartRequest{EnvironmentId: id})
		if err != nil {
			r.done <- err
			return
		}
		for {
			out, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				r.done <- nil
				return
			}
			if err != nil {
				r.done <- err
				return
			}
			if d := out.GetData(); d != nil {
				r.lines <- string(d.GetData())
			}
		}
	}()
	return r
}

// next returns the Start's next progress line.
func (r *runningStart) next(t *testing.T) string {
	t.Helper()
	return within(t, r.lines, "a progress line")
}

// result waits for the Start to end and returns its error.
func (r *runningStart) result(t *testing.T) error {
	t.Helper()
	return within(t, r.done, "Start's return")
}

// waiting waits until the Start sends its waiting line, which it wants to
// match re, and fails t if another line comes first but the image's.
func (r *runningStart) waiting(t *testing.T, re *regexp.Regexp) {
	t.Helper()
	if l := r.next(t); !strings.Contains(l, "image "+testImage+" ready") {
		t.Fatalf("first progress line %q, want the image's", l)
	}
	if l := r.next(t); !re.MatchString(l) {
		t.Fatalf("progress line %q, want one matching %s", l, re)
	}
}

// remaining drains the Start's progress lines once it has ended.
func (r *runningStart) remaining() []string {
	var ls []string
	for {
		select {
		case l := <-r.lines:
			ls = append(ls, l)
		default:
			return ls
		}
	}
}

// withBudget gives srv a memory budget of capMiB, as max_memory_mib does,
// and a log t can read. It runs before any call on srv.
func withBudget(srv *Server, capMiB uint64) *logBuffer {
	srv.memory = admission.New(capMiB)
	buf := &logBuffer{}
	srv.log = slog.New(slog.NewTextHandler(buf, nil))
	return buf
}

const at = `^\[runner-microvm \d+\.\d\ds\] `

// A job that needs more than max_memory_mib on its own fails at Create with
// FailedPrecondition, naming its need and the cap; one that fits is
// created.
func TestCreateRefusesAJobThatNeverFits(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeCache{}, &fakeLauncher{})
	withBudget(srv, 1023)
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	_, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{LabelArg: testImage})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Create = %v, want FailedPrecondition", err)
	}
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "1024 MiB") || !strings.Contains(msg, "max_memory_mib, 1023 MiB") {
		t.Errorf("Create's message %q names neither the need, 1024 MiB, nor the cap, 1023 MiB", msg)
	}
	if n := srv.jobCount(); n != 0 {
		t.Errorf("%d jobs in the table after a refused Create", n)
	}

	srv = newTestServer(t, &fakeCache{}, &fakeLauncher{})
	withBudget(srv, 1024)
	create(t, pluginv1alpha.NewBackendPluginClient(serve(t, srv)))
}

// A Start whose job doesn't fit waits after its image is ready, before its
// job process starts, sending a waiting line, and once the earlier job is
// removed, a got line, then boots; the plugin's log has both. A Start
// admitted at once sends neither line, and logs no wait.
func TestAStartWaitsForMemory(t *testing.T) {
	t.Parallel()
	launcher := &fakeLauncher{}
	srv := newTestServer(t, &fakeCache{}, launcher)
	log := withBudget(srv, 1536)
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	first, second := create(t, c), create(t, c)

	r := startInBackground(testContext(t), c, first)
	if err := r.result(t); err != nil {
		t.Fatal(err)
	}
	for _, l := range r.remaining() {
		if strings.Contains(l, "memory") {
			t.Errorf("a Start admitted at once sent %q", l)
		}
	}
	if strings.Contains(log.String(), "waiting for memory") || strings.Contains(log.String(), "got memory") {
		t.Errorf("a Start admitted at once logged a wait:\n%s", log)
	}

	r = startInBackground(testContext(t), c, second)
	r.waiting(t, regexp.MustCompile(at+`waiting for memory: needs 1024 MiB, 512 of 1536 MiB free; 0 ahead\n$`))
	if n := launcher.launched(); n != 1 {
		t.Fatalf("%d job processes launched while the second job waits, want 1", n)
	}
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: first}); err != nil {
		t.Fatal(err)
	}
	if l := r.next(t); !regexp.MustCompile(at + `got memory after \d+\.\d\ds\n$`).MatchString(l) {
		t.Errorf("progress line after the wait %q, want the got line", l)
	}
	if err := r.result(t); err != nil {
		t.Fatalf("the waiting Start, once admitted: %v", err)
	}
	logged := log.String()
	for _, want := range []string{
		`msg="waiting for memory" env=` + second + ` need_mib=1024 free_mib=512 cap_mib=1536 ahead=0`,
		`msg="got memory" env=` + second + ` need_mib=1024 took=`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("the plugin's log lacks %q:\n%s", want, logged)
		}
	}
}

// A Remove of a waiting job ends its Start with Canceled, and the wait is
// logged as stopped; a Start cancelled by the runner while it waits fails
// with its context's code. Neither holds memory afterwards.
func TestAWaitEndsWithItsStart(t *testing.T) {
	t.Parallel()
	for name, removed := range map[string]bool{"removed": true, "cancelled": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			launcher := &fakeLauncher{}
			srv := newTestServer(t, &fakeCache{}, launcher)
			log := withBudget(srv, 1024)
			c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
			first, second := create(t, c), create(t, c)
			if _, err := start(testContext(t), c, first); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(testContext(t))
			defer cancel()
			r := startInBackground(ctx, c, second)
			r.waiting(t, regexp.MustCompile(`waiting for memory: needs 1024 MiB, 0 of 1024 MiB free; 0 ahead`))
			if removed {
				if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: second}); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			if err := r.result(t); status.Code(err) != codes.Canceled {
				t.Errorf("the waiting Start = %v, want Canceled", err)
			}
			waitFor(t, "the stopped wait's line", func() bool {
				return strings.Contains(log.String(), `msg="stopped waiting for memory" env=`+second)
			})
			if launcher.launched() != 1 {
				t.Errorf("%d job processes launched, want only the first job's", launcher.launched())
			}
			// The first job holds the budget; the second holds nothing.
			if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: first}); err != nil {
				t.Fatal(err)
			}
			wantFree(t, srv, 1024)
		})
	}
}

// wantFree fails t unless needMiB of srv's memory budget is free, with no
// job waiting: a job needing it is admitted at once.
func wantFree(t *testing.T, srv *Server, needMiB uint64) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := srv.memory.Admit(ctx, "probe", needMiB, nil); err != nil {
		t.Errorf("%d MiB of the memory budget are not free: %v", needMiB, err)
		return
	}
	srv.memory.Release("probe")
}

// A drain fails a Start waiting for memory with Unavailable, as a new
// Create, and so does a Start that would begin waiting during it, at once,
// with no wait lines; a job
// already admitted runs on, and the drain ends once every job is removed.
func TestADrainEndsEveryWait(t *testing.T) {
	t.Parallel()
	f := newDrainFixture(t)
	log := withBudget(f.srv, 1024)
	running := f.started(t)
	waiting, late := create(t, f.c), create(t, f.c)
	r := startInBackground(testContext(t), f.c, waiting)
	r.waiting(t, regexp.MustCompile(`waiting for memory`))

	f.draining(t, syscall.SIGTERM)
	err := r.result(t)
	if status.Code(err) != codes.Unavailable || status.Convert(err).Message() != "runner-microvm is shutting down" {
		t.Errorf("the waiting Start during the drain = %v, want Unavailable: runner-microvm is shutting down", err)
	}
	if !strings.Contains(log.String(), `msg="stopped waiting for memory" env=`+waiting) {
		t.Errorf("the drained wait was not logged as stopped:\n%s", log)
	}
	// A Start that would have to wait once the drain has begun fails at
	// once: it never waits, so neither its setup log nor the plugin's
	// has a wait line.
	r = startInBackground(testContext(t), f.c, late)
	if err := r.result(t); status.Code(err) != codes.Unavailable {
		t.Errorf("a Start that begins waiting during the drain = %v, want Unavailable", err)
	}
	for _, l := range r.remaining() {
		if strings.Contains(l, "memory") {
			t.Errorf("a Start that never waited, during the drain, sent %q", l)
		}
	}
	if logged := log.String(); strings.Contains(logged, `waiting for memory" env=`+late) {
		t.Errorf("a Start that never waited, during the drain, logged a wait:\n%s", logged)
	}
	f.wantServing(t, "with jobs left")
	outs, err := execAll(t, f.c, &pluginv1alpha.ExecRequest{EnvironmentId: running, Command: []string{"true"}})
	if err != nil || len(outs) == 0 || outs[len(outs)-1].GetExecComplete() == nil {
		t.Errorf("Exec on the admitted job during the drain: %v, %v", outs, err)
	}
	f.remove(t, waiting)
	f.remove(t, late)
	f.remove(t, running)
	f.wantServed(t)
}

// A drain ends a wait whose waiting line the runner never takes, at once,
// with Unavailable: the line's send gives up with the wait, and the job,
// though admitted while that send blocked, boots nothing and holds nothing.
func TestADrainEndsAWaitWhoseLineIsStuck(t *testing.T) {
	t.Parallel()
	launcher := &fakeLauncher{}
	srv := newTestServer(t, &fakeCache{}, launcher)
	withBudget(srv, 1024)
	first := createOn(t, srv, testImage)
	if err := srv.Start(&pluginv1alpha.StartRequest{EnvironmentId: first}, &progressStream{ctx: testContext(t), data: func() error { return nil }}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	// As gRPC ends the stream once the handler returns.
	t.Cleanup(func() { close(release) })
	lines := 0
	// The image's line is sent; the waiting line's send blocks, as one on
	// a stream the runner's flow control holds does.
	stream := &progressStream{ctx: testContext(t), data: func() error {
		if lines++; lines == 1 {
			return nil
		}
		entered <- struct{}{}
		<-release
		return errors.New("the stream ended")
	}}
	second := createOn(t, srv, testImage)
	started := make(chan error, 1)
	go func() { started <- srv.Start(&pluginv1alpha.StartRequest{EnvironmentId: second}, stream) }()
	within(t, entered, "the waiting line")
	// The first job's memory frees while the waiting line's send blocks:
	// the second job is admitted, but its wait hasn't ended.
	if _, err := srv.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: first}); err != nil {
		t.Fatal(err)
	}
	srv.Drain()
	if err := within(t, started, "Start's return"); status.Code(err) != codes.Unavailable {
		t.Errorf("the waiting Start during the drain = %v, want Unavailable", err)
	}
	if n := launcher.launched(); n != 1 {
		t.Errorf("%d job processes launched, want only the first job's", n)
	}
	wantFree(t, srv, 1024)
}

// A failed Start frees its memory before it returns, once its job process
// is reaped: the next job doesn't wait for the failed one's Remove.
func TestAFailedStartFreesItsMemory(t *testing.T) {
	t.Parallel()
	launcher := &fakeLauncher{does: networkFails}
	srv := newTestServer(t, &fakeCache{}, launcher)
	withBudget(srv, 1024)
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	if _, err := start(testContext(t), c, create(t, c)); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Start = %v, want FailedPrecondition", err)
	}
	if launcher.alive() != 0 {
		t.Error("the failed Start left its job process running")
	}
	wantFree(t, srv, 1024)
}

// A job whose kill fails may still have its VM running, so it keeps its
// memory: a failed Start holds it until its Remove reaps the job process,
// and a Remove whose kill fails holds it for good.
func TestMemoryIsHeldUntilTheReap(t *testing.T) {
	t.Parallel()
	launcher := &fakeLauncher{does: networkFails}
	srv := newTestServer(t, &fakeCache{}, launcher)
	withBudget(srv, 1024)
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	failed := create(t, c)
	launcher.setKillFails(1)
	if _, err := start(testContext(t), c, failed); status.Code(err) == codes.OK {
		t.Fatal("Start whose job process failed succeeded")
	}
	wantHeld(t, srv, 1024)
	// Remove kills the job process again, and frees its memory once
	// that kill reaps it.
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: failed}); err != nil {
		t.Fatal(err)
	}
	if launcher.alive() != 0 {
		t.Error("Remove left the failed Start's job process running")
	}
	wantFree(t, srv, 1024)

	launcher.mu.Lock()
	launcher.does = boots
	launcher.mu.Unlock()
	started := create(t, c)
	if _, err := start(testContext(t), c, started); err != nil {
		t.Fatal(err)
	}
	launcher.setKillFails(1)
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: started}); status.Code(err) != codes.Internal {
		t.Errorf("Remove whose kill failed = %v, want Internal", err)
	}
	wantHeld(t, srv, 1024)
}

// setKillFails makes the next n Kills fail.
func (l *fakeLauncher) setKillFails(n int) {
	l.mu.Lock()
	l.killFails = n
	l.mu.Unlock()
}

// wantHeld fails t unless needMiB of srv's memory budget is held: a job
// needing it can't be admitted at once.
func wantHeld(t *testing.T, srv *Server, needMiB uint64) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := srv.memory.Admit(ctx, "probe", needMiB, nil); err == nil {
		srv.memory.Release("probe")
		t.Errorf("%d MiB of the memory budget are free, want them held", needMiB)
	}
}
