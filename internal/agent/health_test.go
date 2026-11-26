// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// fakeChecks is a checker whose every check answers with the next of its
// results, the last one again once they run out; a check whose result is
// hang runs until its context ends.
type fakeChecks struct {
	results []fakeResult

	mu sync.Mutex
	// runs is how many checks have begun, at the fake clock's times.
	runs []time.Time
	// ended are the errors of the contexts of the checks that hung.
	ended []error
}

type fakeResult struct {
	passed bool
	output string
	hang   bool
}

func (f *fakeChecks) run(ctx context.Context) (bool, string) {
	f.mu.Lock()
	n := len(f.runs)
	f.runs = append(f.runs, time.Now())
	r := f.results[min(n, len(f.results)-1)]
	f.mu.Unlock()
	if r.hang {
		<-ctx.Done()
		f.mu.Lock()
		f.ended = append(f.ended, ctx.Err())
		f.mu.Unlock()
		return false, r.output
	}
	return r.passed, r.output
}

// count is how many checks have begun.
func (f *fakeChecks) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

// monitored starts monitoring hc's check, with checks, on the fake clock,
// as the agent does once the service has started, and returns its health;
// the monitor stops with the test.
func monitored(t *testing.T, hc *agentv1.Healthcheck, checks *fakeChecks) *health {
	t.Helper()
	check, ignored := newHealthCheck(hc)
	if check == nil {
		t.Fatalf("no check (ignored %q)", ignored)
	}
	h := newHealth(check, time.Now())
	stop := monitorHealth(t.Context(), slog.New(slog.DiscardHandler), h, checks.run)
	t.Cleanup(stop)
	return h
}

// at sleeps on the fake clock until d after the bubble's start, then waits
// for every goroutine to block, and returns h's answer.
func at(begin time.Time, d time.Duration, h *health) *agentv1.HealthResponse {
	time.Sleep(time.Until(begin.Add(d)))
	synctest.Wait()
	return h.response()
}

func wantState(t *testing.T, when string, got *agentv1.HealthResponse, want agentv1.HealthResponse_State) {
	t.Helper()
	if got.GetState() != want {
		t.Errorf("%s: %v (output %q), want %v", when, got.GetState(), got.GetOutput(), want)
	}
}

// A check that fails, then passes, is healthy after the start interval
// twice: the first check one start interval after the service starts, the
// next one start interval after it. Then the checks stop.
func TestHealthPassingOnItsSecondCheck(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		checks := &fakeChecks{results: []fakeResult{{output: "not yet\n"}, {passed: true, output: "ok\n"}}}
		begin := time.Now()
		h := monitored(t, &agentv1.Healthcheck{Test: []string{"CMD", "check"}, StartPeriod: durationpb.New(time.Minute), StartInterval: durationpb.New(2 * time.Second)}, checks)
		resp := at(begin, 0, h)
		wantState(t, "at once", resp, agentv1.HealthResponse_STATE_STARTING)
		if checks.count() != 0 {
			t.Errorf("at once: %d checks, want none", checks.count())
		}
		resp = at(begin, 3*time.Second, h)
		wantState(t, "after one check", resp, agentv1.HealthResponse_STATE_STARTING)
		if resp.GetOutput() != "not yet\n" {
			t.Errorf("after one check: output %q, want the check's", resp.GetOutput())
		}
		resp = at(begin, 4*time.Second, h)
		wantState(t, "after two checks", resp, agentv1.HealthResponse_STATE_HEALTHY)
		if resp.GetOutput() != "ok\n" {
			t.Errorf("healthy: output %q, want the check's", resp.GetOutput())
		}
		if want := []time.Time{begin.Add(2 * time.Second), begin.Add(4 * time.Second)}; !slices.Equal(checks.runs, want) {
			t.Errorf("checks ran at %v, want %v", checks.runs, want)
		}
		// Once healthy, no check runs again.
		at(begin, time.Hour, h)
		if n := checks.count(); n != 2 {
			t.Errorf("%d checks after the service was healthy, want 2", n)
		}
	})
}

// Failures during the start period don't count; once it is over, the
// checks run at the interval, and retries failures in a row make the
// service unhealthy, its output the last check's. Then the checks stop.
func TestHealthFailuresAfterTheStartPeriod(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		checks := &fakeChecks{results: []fakeResult{{output: "down\n"}}}
		begin := time.Now()
		h := monitored(t, &agentv1.Healthcheck{
			Test: []string{"CMD", "check"}, Interval: durationpb.New(10 * time.Second),
			StartPeriod: durationpb.New(20 * time.Second), StartInterval: durationpb.New(5 * time.Second), Retries: 2,
		}, checks)
		// Checks at 5, 10 and 15 s don't count; the one at 20 s, the start
		// period's end, is the first that does, and the next runs at the
		// interval.
		resp := at(begin, 29*time.Second, h)
		wantState(t, "after four failures, one counted", resp, agentv1.HealthResponse_STATE_STARTING)
		resp = at(begin, 30*time.Second, h)
		wantState(t, "after two counted failures", resp, agentv1.HealthResponse_STATE_UNHEALTHY)
		if resp.GetOutput() != "down\n" {
			t.Errorf("unhealthy: output %q, want the last check's", resp.GetOutput())
		}
		var want []time.Time
		for _, s := range []time.Duration{5, 10, 15, 20, 30} {
			want = append(want, begin.Add(s*time.Second))
		}
		if !slices.Equal(checks.runs, want) {
			t.Errorf("checks ran at %v, want %v", checks.runs, want)
		}
		at(begin, time.Hour, h)
		if n := checks.count(); n != 5 {
			t.Errorf("%d checks after the service was unhealthy, want 5", n)
		}
	})
}

// With Docker's defaults, 30 s apart with no start period, the third
// failure in a row makes the service unhealthy; a failure that follows a
// pass never comes, as the first pass is healthy.
func TestHealthDefaultsToDockers(t *testing.T) {
	t.Parallel()
	check, _ := newHealthCheck(&agentv1.Healthcheck{Test: []string{"CMD", "check"}, Interval: durationpb.New(-time.Second), Retries: -1})
	if want := (healthCheck{argv: []string{"check"}, interval: 30 * time.Second, timeout: 30 * time.Second, startInterval: 5 * time.Second, retries: 3}); !equalCheck(check, want) {
		t.Errorf("check %+v, want %+v", check, want)
	}
	synctest.Test(t, func(t *testing.T) {
		checks := &fakeChecks{results: []fakeResult{{output: "down\n"}}}
		begin := time.Now()
		h := monitored(t, &agentv1.Healthcheck{Test: []string{"CMD", "check"}}, checks)
		wantState(t, "after two failures", at(begin, 89*time.Second, h), agentv1.HealthResponse_STATE_STARTING)
		wantState(t, "after three failures", at(begin, 90*time.Second, h), agentv1.HealthResponse_STATE_UNHEALTHY)
		if n := checks.count(); n != 3 {
			t.Errorf("%d checks, want 3", n)
		}
	})
}

func equalCheck(got *healthCheck, want healthCheck) bool {
	return got != nil && slices.Equal(got.argv, want.argv) && got.interval == want.interval && got.timeout == want.timeout &&
		got.startPeriod == want.startPeriod && got.startInterval == want.startInterval && got.retries == want.retries
}

// A check still running at its timeout is ended, through its context, and
// fails, saying so after its output.
func TestHealthCheckOverItsTimeoutFails(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		checks := &fakeChecks{results: []fakeResult{{hang: true, output: "stuck"}}}
		begin := time.Now()
		h := monitored(t, &agentv1.Healthcheck{
			Test: []string{"CMD", "check"}, Interval: durationpb.New(time.Second), Timeout: durationpb.New(3 * time.Second), Retries: 1,
		}, checks)
		wantState(t, "within the timeout", at(begin, 3*time.Second, h), agentv1.HealthResponse_STATE_STARTING)
		resp := at(begin, 4*time.Second, h)
		wantState(t, "at the timeout", resp, agentv1.HealthResponse_STATE_UNHEALTHY)
		if want := "stuck\nthe health check exceeded its timeout (3s)\n"; resp.GetOutput() != want {
			t.Errorf("output %q, want %q", resp.GetOutput(), want)
		}
		if len(checks.ended) != 1 || !errors.Is(checks.ended[0], context.DeadlineExceeded) {
			t.Errorf("the check's context ended with %v, want the deadline", checks.ended)
		}
	})
}

// Ending the monitor ends a check still running, and the monitor with it.
func TestHealthStopEndsARunningCheck(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		checks := &fakeChecks{results: []fakeResult{{hang: true}}}
		check, _ := newHealthCheck(&agentv1.Healthcheck{Test: []string{"CMD", "check"}, Interval: durationpb.New(time.Second)})
		h := newHealth(check, time.Now())
		stop := monitorHealth(t.Context(), slog.New(slog.DiscardHandler), h, checks.run)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		stop()
		if len(checks.ended) != 1 || !errors.Is(checks.ended[0], context.Canceled) {
			t.Errorf("the check's context ended with %v, want cancelled", checks.ended)
		}
		wantState(t, "stopped", h.response(), agentv1.HealthResponse_STATE_STARTING)
	})
}

// No HEALTHCHECK, an empty test and NONE are no check, and so is a kind of
// test Docker ignores, which is named; CMD runs its arguments, and
// CMD-SHELL its command through /bin/sh -c.
func TestHealthCheckForms(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		hc      *agentv1.Healthcheck
		argv    []string
		ignored string
	}{
		{nil, nil, ""},
		{&agentv1.Healthcheck{}, nil, ""},
		{&agentv1.Healthcheck{Test: []string{"NONE"}}, nil, ""},
		{&agentv1.Healthcheck{Test: []string{"HTTP", "/"}}, nil, "HTTP"},
		{&agentv1.Healthcheck{Test: []string{"CMD", "redis-cli", "ping"}}, []string{"redis-cli", "ping"}, ""},
		{&agentv1.Healthcheck{Test: []string{"CMD-SHELL", "redis-cli ping | grep PONG"}}, []string{"/bin/sh", "-c", "redis-cli ping | grep PONG"}, ""},
	} {
		check, ignored := newHealthCheck(tc.hc)
		if (check == nil) != (tc.argv == nil) || (check != nil && !slices.Equal(check.argv, tc.argv)) || ignored != tc.ignored {
			t.Errorf("%v: check %+v, ignored %q; want argv %q, ignored %q", tc.hc, check, ignored, tc.argv, tc.ignored)
		}
	}
	if resp := newHealth(nil, time.Now()).response(); resp.GetState() != agentv1.HealthResponse_STATE_NONE {
		t.Errorf("no check: %v, want none", resp)
	}
}

// A check runs as the service's command does, in the test's root: its
// output, stdout and stderr, goes to the console and is kept, its last
// healthOutputMax bytes only; exit 0 passes and any other code fails;
// CMD-SHELL's command runs through /bin/sh -c; a check that can't start
// fails, naming its program and never the PATH; a check whose context ends
// is killed with its process group.
func TestCommandChecker(t *testing.T) {
	t.Parallel()
	r := newServiceRun(t)
	image := r.bootConfig(nil).GetImage()
	shell := func(cmd string) []string {
		check, _ := newHealthCheck(&agentv1.Healthcheck{Test: []string{"CMD-SHELL", cmd}})
		return check.argv
	}
	run := func(ctx context.Context, argv []string) (bool, string) {
		return commandChecker(r.root, image, nil, argv, r.console)(ctx)
	}
	if passed, out := run(testContext(t), shell(`echo out; echo err >&2; [ "$IMAGE_ONLY" = from-image ]`)); !passed || out != "out\nerr\n" {
		t.Errorf("a passing check: %v, %q", passed, out)
	}
	if passed, out := run(testContext(t), shell("echo nope; exit 3")); passed || out != "nope\n" {
		t.Errorf("a failing check: %v, %q", passed, out)
	}
	if passed, out := run(testContext(t), shell("head -c 5000 /dev/zero | tr '\\0' a; echo end")); passed != true ||
		len(out) != healthOutputMax || !strings.HasSuffix(out, "aend\n") {
		t.Errorf("a long check: %v, %d bytes ending %q; want its last %d", passed, len(out), out[max(0, len(out)-8):], healthOutputMax)
	}
	if passed, out := run(testContext(t), []string{"no-such-check"}); passed || out != "the health check no-such-check is not found in its PATH\n" {
		t.Errorf("a missing check: %v, %q", passed, out)
	}
	// A check whose context ends is killed, and its background child, in
	// its group, with it.
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithTimeout(testContext(t), 500*time.Millisecond)
	defer cancel()
	begin := time.Now()
	if passed, _ := run(ctx, shell("sleep 60 & echo $! > "+pidFile+"; wait")); passed {
		t.Error("a check killed at its timeout passed")
	}
	if d := time.Since(begin); d > serviceBound {
		t.Errorf("the killed check took %v", d)
	}
	pid, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	waitGone(t, strings.TrimSpace(string(pid)))
	console, err := os.ReadFile(r.console.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"out\nerr\n", "nope\n", "aend\n", "the health check no-such-check is not found in its PATH\n"} {
		if !strings.Contains(string(console), want) {
			t.Errorf("the console lacks %q", want)
		}
	}
}

// waitGone waits until the process pid has exited, failing t unless it does
// within serviceBound.
func waitGone(t *testing.T, pid string) {
	t.Helper()
	deadline := time.Now().Add(serviceBound)
	for {
		b, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
		// A zombie has exited: only its reaping is left.
		if err != nil || strings.Contains(string(b), ") Z ") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %s still runs after %v", pid, serviceBound)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The agent runs a service's health check once StartService has started
// the service, with the environment StartService sent, and Health answers
// with it: starting before StartService and until a file the check looks
// for appears, then healthy. A VM's agent without a service answers none.
func TestAgentAnswersHealth(t *testing.T) {
	t.Parallel()
	r := newServiceRun(t)
	ready := filepath.Join(t.TempDir(), "ready")
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := r.bootConfig([]string{"sh", "-c", `read -r line < "$FIFO"`})
	// The check looks for the file StartService's env names.
	cfg.Service.Healthcheck = &agentv1.Healthcheck{
		Test: []string{"CMD-SHELL", `test -e "$READY"`}, StartPeriod: durationpb.New(time.Minute), StartInterval: durationpb.New(20 * time.Millisecond),
	}
	r.start(t, cfg)
	r.ping(t)
	health := func() *agentv1.HealthResponse {
		t.Helper()
		resp, err := r.client().Health(testContext(t), &agentv1.HealthRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := health(); resp.GetState() != agentv1.HealthResponse_STATE_STARTING || resp.GetOutput() != "" {
		t.Errorf("before StartService: %v, want starting with no output", resp)
	}
	r.mustStartService(t, map[string]string{"FIFO": fifo, "READY": ready})
	resp := health()
	wantState(t, "before the file", resp, agentv1.HealthResponse_STATE_STARTING)
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(serviceBound)
	for health().GetState() != agentv1.HealthResponse_STATE_HEALTHY {
		if time.Now().After(deadline) {
			t.Fatalf("not healthy %v after the file appeared; the agent's log:\n%s", serviceBound, r.log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(r.log.String(), `msg="service healthy"`) {
		t.Errorf("the agent's log does not say the service is healthy:\n%s", r.log.String())
	}
	if err := os.WriteFile(fifo, []byte("stop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := r.wait(t); code != 0 {
		t.Errorf("the agent returned %d, want the service's 0", code)
	}

	s := &server{}
	if resp, err := s.Health(testContext(t), &agentv1.HealthRequest{}); err != nil || resp.GetState() != agentv1.HealthResponse_STATE_NONE {
		t.Errorf("Health without a service = %v, %v; want none", resp, err)
	}
}
