// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/secret"
)

// Docker's defaults for a HEALTHCHECK whose image leaves a field unset, so a
// workflow written for the Docker backend waits the same.
const (
	defaultHealthInterval      = 30 * time.Second
	defaultHealthTimeout       = 30 * time.Second
	defaultHealthStartInterval = 5 * time.Second
	defaultHealthRetries       = 3
)

// healthOutputMax is how much of a check's output Health returns: its last
// 4 KiB, as much as Docker keeps of each check.
const healthOutputMax = 4 << 10

// healthCheck is a service's HEALTHCHECK with Docker's rules applied: its
// command, and its timing with every unset field defaulted.
type healthCheck struct {
	argv                                          []string
	interval, timeout, startPeriod, startInterval time.Duration
	retries                                       int
}

// newHealthCheck returns the check of the image's HEALTHCHECK hc, by
// Docker's rules, or nil when there is none: hc absent, an empty test, or
// NONE. A test of a kind Docker doesn't know is no check either, as Docker
// ignores it; ignored then names the kind. ["CMD", args…] runs args, and
// ["CMD-SHELL", command] runs /bin/sh -c command. A zero or negative
// duration, and retries below 1, take Docker's default.
func newHealthCheck(hc *agentv1.Healthcheck) (check *healthCheck, ignored string) {
	test := hc.GetTest()
	if len(test) == 0 {
		return nil, ""
	}
	var argv []string
	switch test[0] {
	case "NONE":
		return nil, ""
	case "CMD":
		argv = test[1:]
	case "CMD-SHELL":
		argv = append([]string{"/bin/sh", "-c"}, test[1:]...)
	default:
		return nil, test[0]
	}
	retries := int(hc.GetRetries())
	if retries < 1 {
		retries = defaultHealthRetries
	}
	return &healthCheck{
		argv:          argv,
		interval:      durationOr(hc.GetInterval(), defaultHealthInterval),
		timeout:       durationOr(hc.GetTimeout(), defaultHealthTimeout),
		startPeriod:   durationOr(hc.GetStartPeriod(), 0),
		startInterval: durationOr(hc.GetStartInterval(), defaultHealthStartInterval),
		retries:       retries,
	}, ""
}

// durationOr is d, or def when d is absent, zero or negative.
func durationOr(d *durationpb.Duration, def time.Duration) time.Duration {
	if v := d.AsDuration(); d != nil && v > 0 {
		return v
	}
	return def
}

// checker runs one check, until it exits or ctx ends, and returns whether it
// passed, and its output or why it could not run.
type checker func(ctx context.Context) (passed bool, output string)

// health is the health of a VM's service, which Health answers with.
type health struct {
	// check is nil for a service with no check, or a VM with no service.
	check *healthCheck
	// started is when the service's command started: the start period's
	// beginning.
	started time.Time

	mu     sync.Mutex
	state  agentv1.HealthResponse_State
	output string
	// streak is how many checks have failed in a row, outside the start
	// period.
	streak int
}

// newHealth returns the health of a service started at started, whose check
// is check: starting, or none when check is nil.
func newHealth(check *healthCheck, started time.Time) *health {
	h := &health{check: check, started: started, state: agentv1.HealthResponse_STATE_NONE}
	if check != nil {
		h.state = agentv1.HealthResponse_STATE_STARTING
	}
	return h
}

// response is Health's answer: the state and the last check's output.
func (h *health) response() *agentv1.HealthResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	return &agentv1.HealthResponse{State: h.state, Output: h.output}
}

// intervalLocked is how long after a check the next one runs, by Docker's
// rule: the start interval while the service is starting within its start
// period, else the interval.
func (h *health) intervalLocked() time.Duration {
	if h.state == agentv1.HealthResponse_STATE_STARTING && time.Since(h.started) < h.check.startPeriod {
		return h.check.startInterval
	}
	return h.check.interval
}

// record records a check that began at begun and passed or failed with
// output, and reports whether the checks are over: the service is healthy at
// its first pass, and unhealthy once retries checks in a row have failed. A
// check that began within the start period doesn't count when it fails.
func (h *health) record(begun time.Time, passed bool, output string) (over bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.output = output
	switch {
	case passed:
		h.state = agentv1.HealthResponse_STATE_HEALTHY
		return true
	case begun.Sub(h.started) < h.check.startPeriod:
		return false
	}
	h.streak++
	if h.streak >= h.check.retries {
		h.state = agentv1.HealthResponse_STATE_UNHEALTHY
		return true
	}
	return false
}

// monitor runs h's check with run on Docker's timing until the service is
// healthy or unhealthy, or ctx ends: the first check one interval in force
// after the service started, each next one an interval in force after the
// last ended. A check still running at its timeout is ended, and fails.
func (h *health) monitor(ctx context.Context, run checker, log *slog.Logger) {
	h.mu.Lock()
	timer := time.NewTimer(time.Until(h.started.Add(h.intervalLocked())))
	h.mu.Unlock()
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		begun := time.Now()
		checkCtx, cancel := context.WithTimeout(ctx, h.check.timeout)
		passed, output := run(checkCtx)
		timedOut := !passed && errors.Is(checkCtx.Err(), context.DeadlineExceeded)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if timedOut {
			if output != "" && !strings.HasSuffix(output, "\n") {
				output += "\n"
			}
			output += fmt.Sprintf("the health check exceeded its timeout (%v)\n", h.check.timeout)
			log.Info("health check timed out", "timeout", h.check.timeout)
		}
		log.Debug("health check", "passed", passed)
		if h.record(begun, passed, output) {
			if passed {
				log.Info("service healthy", "after", time.Since(h.started).Round(time.Millisecond))
			} else {
				log.Warn("service unhealthy", "failures", h.check.retries)
			}
			return
		}
		h.mu.Lock()
		timer.Reset(h.intervalLocked())
		h.mu.Unlock()
	}
}

// commandChecker returns the checker that runs argv in the job's root at root as
// the service's command runs (startService): as the image's user, in its
// working directory, with the service's environment env laid over the
// image's, in a process group of its own, which the checker's context's end
// kills. Its output goes to the console and is kept for Health. Why it
// can't start names argv[0], never the PATH or the path found in it, which
// may be the service's env's (LOG-2).
func commandChecker(root string, image *agentv1.Image, env secret.Env, argv []string, console *os.File) checker {
	// fail is a check that can't run: why goes to the console too.
	fail := func(format string, args ...any) (bool, string) {
		msg := fmt.Sprintf(format, args...)
		_, _ = console.WriteString(msg)
		return false, msg
	}
	return func(ctx context.Context) (bool, string) {
		c, err := prepare(root, image, execRequest{argv: argv, env: env})
		var notFound *notFoundError
		switch {
		case errors.As(err, &notFound):
			return fail("the health check %s is not found in its PATH\n", notFound.name)
		case err != nil:
			return fail("the health check can't start: %v\n", err)
		}
		out := &checkOutput{console: console}
		cmd := c.command(ctx)
		cmd.Stdout, cmd.Stderr = out, out
		// A background child that keeps the output open can't hold the
		// check past its exit.
		cmd.WaitDelay = outputGrace
		if err := cmd.Start(); err != nil {
			// The path it quotes is found in the PATH: keep only why.
			if pe, ok := errors.AsType[*fs.PathError](err); ok {
				err = pe.Err
			}
			return fail("the health check %s can't start: %v\n", argv[0], err)
		}
		_ = cmd.Wait()
		return cmd.ProcessState.Success(), out.String()
	}
}

// checkOutput is a check's stdout and stderr: written through to the
// console, whose errors don't fail the check, and its last healthOutputMax
// bytes kept.
type checkOutput struct {
	console *os.File
	mu      sync.Mutex
	tail    []byte
}

func (o *checkOutput) Write(p []byte) (int, error) {
	_, _ = o.console.Write(p)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tail = append(o.tail, p...)
	if over := len(o.tail) - healthOutputMax; over > 0 {
		o.tail = append(o.tail[:0], o.tail[over:]...)
	}
	return len(p), nil
}

// String is the output kept, as valid UTF-8, which a proto string must be.
func (o *checkOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.ToValidUTF8(string(o.tail), "�")
}
