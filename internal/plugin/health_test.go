// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/imagecache"
)

// healthAgent is a fake service's agent whose Health answers with its
// current answer, starting until the test sets another, and whose Ping
// fails with pingErr once the test sets one.
type healthAgent struct {
	fakeAgent
	mu      sync.Mutex
	answer  *agentv1.HealthResponse
	err     error
	pingErr error
	// calls counts its Health calls.
	calls int
}

func newHealthAgent() *healthAgent {
	return &healthAgent{answer: &agentv1.HealthResponse{State: agentv1.HealthResponse_STATE_STARTING}}
}

func (a *healthAgent) Health(context.Context, *agentv1.HealthRequest) (*agentv1.HealthResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return a.answer, a.err
}

func (a *healthAgent) Ping(ctx context.Context, req *agentv1.PingRequest) (*agentv1.PingResponse, error) {
	a.mu.Lock()
	err := a.pingErr
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return a.fakeAgent.Ping(ctx, req)
}

// lose makes Health fail with err and Ping with pingErr from now on: an
// agent that no longer answers.
func (a *healthAgent) lose(err, pingErr error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answer, a.err, a.pingErr = nil, err, pingErr
}

func (a *healthAgent) set(answer *agentv1.HealthResponse, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answer, a.err = answer, err
}

func (a *healthAgent) called() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// startingJob is a job with the service cache, whose Start waits for the
// service's health.
type startingJob struct {
	srv      *Server
	launcher *fakeLauncher
	c        pluginv1alpha.BackendPluginClient
	id       string
	start    *runningStart
	cancel   context.CancelFunc
	log      *logBuffer
}

// startingService creates a job with the service cache, whose agent is
// agent, and starts it in the background, once Start has said both VMs
// are ready and that it waits for the service's health: after the image
// lines, the VMs' lines and the wait's, the wait's possibly before the job's
// container's, as the wait starts once the service's StartService succeeds.
func startingService(t *testing.T, agent *healthAgent) startingJob {
	t.Helper()
	return startingServiceWith(t, agent, 0)
}

// startingServiceWith is startingService on a plugin whose lost agent
// grace is lostAgentGrace, or the built-in one when it is 0.
func startingServiceWith(t *testing.T, agent *healthAgent, lostAgentGrace time.Duration) startingJob {
	t.Helper()
	j := startingJob{launcher: &fakeLauncher{vmAgents: map[int]agentv1.AgentServiceServer{1: agent}}, log: &logBuffer{}}
	srv := newTestServer(t, &fakeCache{}, j.launcher)
	srv.log = slog.New(slog.NewTextHandler(j.log, nil))
	if lostAgentGrace != 0 {
		srv.cfg.LostAgentGrace = lostAgentGrace
	}
	j.srv = srv
	j.c = pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	j.id = createWithServices(t, j.c, "build", svc("cache", "redis:8"))
	ctx, cancel := context.WithCancel(testContext(t))
	t.Cleanup(cancel)
	j.start, j.cancel = startInBackground(ctx, j.c, j.id), cancel
	waiting, ready := false, 0
	for !waiting || ready < 2 {
		l := j.start.next(t)
		switch {
		case strings.Contains(l, "] waiting for service cache to be healthy\n"):
			waiting = true
		case strings.Contains(l, "] VM ") && strings.Contains(l, " ready in "):
			ready++
		case !strings.Contains(l, "] image ") && !strings.Contains(l, "] VM "):
			t.Fatalf("progress line %q before the wait for the service's health", l)
		}
	}
	return j
}

// Start waits while a service's check is starting, asking its agent again
// every healthPoll, and completes once it is healthy, saying so.
func TestStartWaitsForAServicesHealth(t *testing.T) {
	t.Parallel()
	agent := newHealthAgent()
	j := startingService(t, agent)
	r := j.start
	waitFor(t, "a second Health call", func() bool { return agent.called() >= 2 })
	select {
	case err := <-r.done:
		t.Fatalf("Start returned %v while its service was starting", err)
	default:
	}
	agent.set(&agentv1.HealthResponse{State: agentv1.HealthResponse_STATE_HEALTHY, Output: "PONG\n"}, nil)
	if err := r.result(t); err != nil {
		t.Fatal(err)
	}
	lines := r.remaining()
	if len(lines) != 2 || !regexp.MustCompile(at+`service cache healthy after \d+\.\d\ds\n$`).MatchString(lines[0]) ||
		!strings.Contains(lines[1], "] started in ") {
		t.Errorf("progress lines after the wait %q; want the service's health, then the total", lines)
	}
	for _, want := range []string{`msg="waiting for a service's health" env=` + j.id + ` vm=cache`, `msg="service healthy" env=` + j.id + ` vm=cache took=`} {
		if !strings.Contains(j.log.String(), want) {
			t.Errorf("the plugin's log lacks %q:\n%s", want, j.log.String())
		}
	}
	if _, err := execAll(t, j.c, &pluginv1alpha.ExecRequest{EnvironmentId: j.id, Command: []string{"true"}, Workdir: "/"}); err != nil {
		t.Errorf("Exec once started: %v", err)
	}
}

// A service whose check reports unhealthy fails Start with
// FailedPrecondition, naming it, with the last check's output cut to its
// last lines; the job process is killed before Start returns.
func TestStartFailsOnAnUnhealthyService(t *testing.T) {
	t.Parallel()
	agent := newHealthAgent()
	j := startingService(t, agent)
	launcher, r := j.launcher, j.start
	var out strings.Builder
	var lines []string
	for i := range 30 {
		l := "refused " + strings.Repeat("x", i)
		out.WriteString(l + "\r\n")
		lines = append(lines, l)
	}
	agent.set(&agentv1.HealthResponse{State: agentv1.HealthResponse_STATE_UNHEALTHY, Output: out.String()}, nil)
	err := r.result(t)
	want := "service cache is unhealthy\nlast health check:\n" + strings.Join(lines[30-tailLines:], "\n")
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != want {
		t.Errorf("Start = %v; want FailedPrecondition: %q", err, want)
	}
	if launcher.alive() != 0 {
		t.Error("a failed Start left its job process running")
	}
}

// An unhealthy service's last check output goes to the runner, which masks
// the job's secrets, and not to the plugin's log, which masks nothing: the
// check runs under the service's env.
func TestStartFailedLogsNoHealthCheckOutput(t *testing.T) {
	t.Parallel()
	agent := newHealthAgent()
	j := startingService(t, agent)
	const marker = "check output marker"
	agent.set(&agentv1.HealthResponse{State: agentv1.HealthResponse_STATE_UNHEALTHY, Output: marker + "\n"}, nil)
	err := j.start.result(t)
	if !strings.Contains(status.Convert(err).Message(), marker) {
		t.Errorf("Start = %v; want the last check's output, %q", err, marker)
	}
	wantLine(t, j.log.String(), "start failed", regexp.QuoteMeta(`err="service cache is unhealthy"`))
	if strings.Contains(j.log.String(), marker) {
		t.Errorf("the plugin's log has the check's output:\n%s", j.log.String())
	}
}

// A Health that fails while the agent still answers fails Start at once,
// naming the VM and the agent's error.
func TestStartFailsWhenHealthFails(t *testing.T) {
	t.Parallel()
	agent := newHealthAgent()
	r := startingService(t, agent).start
	agent.set(nil, status.Error(codes.Internal, "broken"))
	err := r.result(t)
	if want := "VM cache: its agent's Health failed: rpc error: code = Internal desc = broken"; status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != want {
		t.Errorf("Start = %v; want FailedPrecondition: %q", err, want)
	}
}

// A Health that fails while the agent no longer answers Ping, most likely
// its VM dying, fails Start once the lost agent grace has passed with no
// exit reported, naming the VM and the Health's error; the job process is
// killed, and Remove leaves no file of the job.
func TestStartFailsWhenHealthFailsAndTheAgentIsGone(t *testing.T) {
	t.Parallel()
	agent := newHealthAgent()
	j := startingServiceWith(t, agent, 100*time.Millisecond)
	agent.lose(status.Error(codes.DeadlineExceeded, "hung"), status.Error(codes.Unavailable, "connection reset"))
	err := j.start.result(t)
	want := "VM cache: its agent's Health failed, and the VM has not exited: rpc error: code = DeadlineExceeded desc = hung"
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != want {
		t.Errorf("Start = %v; want FailedPrecondition: %q", err, want)
	}
	if j.launcher.alive() != 0 {
		t.Error("a failed Start left its job process running")
	}
	if _, err := j.c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: j.id}); err != nil {
		t.Fatalf("Remove after a failed Start: %v", err)
	}
	if left := jobFiles(t, j.srv, j.id); len(left) != 0 {
		t.Errorf("Remove left %q", left)
	}
}

// Start cancelled while it waits for a service's health returns Canceled,
// and its job process is gone.
func TestStartCancelledWhileAServiceIsStarting(t *testing.T) {
	t.Parallel()
	j := startingService(t, newHealthAgent())
	launcher, r := j.launcher, j.start
	j.cancel()
	if err := r.result(t); status.Code(err) != codes.Canceled {
		t.Errorf("Start = %v; want Canceled", err)
	}
	waitFor(t, "the job process to be killed", func() bool { return launcher.alive() == 0 })
}

// A service with no check is ready once its StartService succeeds: one
// Health call answers none, and Start says nothing of health. The boot config carries the image's
// HEALTHCHECK as the image sets it, for the agent.
func TestAServiceWithNoCheckIsReadyAtStartService(t *testing.T) {
	t.Parallel()
	agent := newHealthAgent()
	agent.set(&agentv1.HealthResponse{State: agentv1.HealthResponse_STATE_NONE}, nil)
	cache := &fakeCache{healthchecks: map[string]*imagecache.Healthcheck{
		"redis:8": {Test: []string{"CMD-SHELL", "redis-cli ping"}, Interval: time.Second, StartPeriod: 2 * time.Second, Retries: 4},
	}}
	launcher := &fakeLauncher{vmAgents: map[int]agentv1.AgentServiceServer{1: agent}}
	srv := newTestServer(t, cache, launcher)
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	id := createWithServices(t, c, "build", svc("cache", "redis:8"), svc("db", "postgres:17"))
	r := startInBackground(testContext(t), c, id)
	if err := r.result(t); err != nil {
		t.Fatal(err)
	}
	for _, l := range r.remaining() {
		if strings.Contains(l, "health") {
			t.Errorf("progress line %q for a service with no check", l)
		}
	}
	if n := agent.called(); n != 1 {
		t.Errorf("%d Health calls, want 1", n)
	}
	vms := specOf(t, launcher, id).GetVms()
	want := &agentv1.Healthcheck{Test: []string{"CMD-SHELL", "redis-cli ping"}, Interval: durationpb.New(time.Second), StartPeriod: durationpb.New(2 * time.Second), Retries: 4}
	if got := vms[1].GetBoot().GetService().GetHealthcheck(); !proto.Equal(got, want) {
		t.Errorf("the service's boot config's health check = %v, want %v", got, want)
	}
	if got := vms[2].GetBoot().GetService().GetHealthcheck(); got != nil {
		t.Errorf("the boot config of a service whose image has no HEALTHCHECK has %v", got)
	}
}

// A Health that fails as the service's VM dies waits for the job process
// to say so: Start fails naming the VM's exit, not the broken call.
func TestStartFailsWhenAServiceDiesWhileStarting(t *testing.T) {
	t.Parallel()
	agent := newHealthAgent()
	j := startingService(t, agent)
	launcher, id, r := j.launcher, j.id, j.start
	calls := agent.called()
	agent.set(nil, status.Error(codes.Unavailable, "connection reset"))
	waitFor(t, "the failed Health call", func() bool { return agent.called() > calls })
	launcher.report(t, id, &jobv1.Event{Event: &jobv1.Event_VmExited{VmExited: &jobv1.VMExited{Vm: 1, ExitCode: 137}}})
	err := r.result(t)
	if want := "VM cache exited during boot (137)"; status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != want {
		t.Errorf("Start = %v; want FailedPrecondition: %q", err, want)
	}
}

// A job process that exits, saying nothing, once every agent has answered
// but while a service's check is starting fails Start naming that service,
// with its VM's tails, not the job's container, which was ready.
func TestStartNamesTheStartingServiceWhenTheJobProcessExits(t *testing.T) {
	t.Parallel()
	j := startingService(t, newHealthAgent())
	if err := writeBootOutput(specOf(t, j.launcher, j.id).GetVms()[1]); err != nil {
		t.Fatal(err)
	}
	j.launcher.die(t, j.id)
	err := j.start.result(t)
	if want := "VM cache: its job process exited during boot" + bootOutput; status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != want {
		t.Errorf("Start = %v; want FailedPrecondition: %q", err, want)
	}
}

// fakeHealthClient is a service's agent for awaitHealth alone: Health
// answers with its current answer, or, when hang is set, blocks until its
// call's context ends; Ping succeeds.
type fakeHealthClient struct {
	mu     sync.Mutex
	answer *agentv1.HealthResponse
	hang   bool
	calls  int
}

func (a *fakeHealthClient) Health(ctx context.Context) (*agentv1.HealthResponse, error) {
	a.mu.Lock()
	a.calls++
	answer, hang := a.answer, a.hang
	a.mu.Unlock()
	if hang {
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return answer, nil
}

func (*fakeHealthClient) Ping(context.Context) error { return nil }

func (a *fakeHealthClient) set(answer *agentv1.HealthResponse) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answer = answer
}

// Start asks a starting service's Health every healthPoll, 1 s, whatever
// interval its check runs at, which only the agent knows, so it sees a
// pass within a second of it: under a 30 s interval, a check that passes
// 2.5 s in is seen at 3 s, on the fourth call.
func TestAwaitHealthSeesAPassWithinASecond(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a := &fakeHealthClient{answer: &agentv1.HealthResponse{State: agentv1.HealthResponse_STATE_STARTING}}
		events := make(chan healthEvent, 2)
		go awaitHealth(t.Context(), 1, a, events)
		if ev := <-events; !ev.starting || ev.i != 1 {
			t.Fatalf("first event %+v, want starting", ev)
		}
		time.Sleep(2500 * time.Millisecond)
		a.set(&agentv1.HealthResponse{State: agentv1.HealthResponse_STATE_HEALTHY})
		passed := time.Now()
		ev := <-events
		if took := time.Since(passed); ev.err != nil || ev.resp.GetState() != agentv1.HealthResponse_STATE_HEALTHY || took > healthPoll {
			t.Errorf("after the pass: %+v, %v later; want healthy within 1s", ev, took)
		}
		if a.calls != 4 {
			t.Errorf("%d Health calls, want 4: at 0, 1, 2 and 3 s", a.calls)
		}
	})
}

// A Health call that hangs ends at healthCallTimeout, and, its agent still
// answering Ping, ends the wait with its own error, so a hung agent fails
// Start rather than holding it.
func TestAwaitHealthBoundsEachCall(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a := &fakeHealthClient{hang: true}
		events := make(chan healthEvent, 2)
		begin := time.Now()
		go awaitHealth(t.Context(), 1, a, events)
		ev := <-events
		if status.Code(ev.err) != codes.DeadlineExceeded || !ev.alive || time.Since(begin) != healthCallTimeout {
			t.Errorf("a hung Health: %+v after %v; want DeadlineExceeded from a live agent after %v", ev, time.Since(begin), healthCallTimeout)
		}
	})
}
