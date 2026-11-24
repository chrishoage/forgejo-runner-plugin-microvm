// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cancelBound is how soon after its executor's context is cancelled a
// running step's executor must have returned and its job process been
// reaped.
const cancelBound = 5 * time.Second

// jobStarted is the plugin's log line for a job process it launched.
var jobStarted = regexp.MustCompile(`msg="job process started" .*\bpid=(\d+)`)

// TestCancelledStep is the protocol test of cancelling a running step
// (GST-8): a workflow step runs `sleep 600`, and once it has started, the
// executor's context is cancelled. Within 5 s the executor has returned and
// the job's job process has been reaped. The test fails at that bound, and
// has an overall timeout of its own, so it never waits on the sleep.
func TestCancelledStep(t *testing.T) {
	t.Parallel()
	wf := writeWorkflow(t, step("sleep", `echo "sleep-$((6 * 100))-started"; sleep 600`))
	p := startPlugin(t, buildPlugin(t))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	stepCtx, cancelStep := context.WithCancel(ctx)
	defer cancelStep()
	log := &syncBuffer{}
	run := planExecutor(t, p, wf, map[string]string{"microvm": readImage(t)}, log)
	ran := make(chan error, 1)
	done := make(chan struct{})
	go func() { defer close(done); ran <- run(stepCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the executor did not return 30 s after the test's end")
		}
		logIfFailed(t, "job log", log)
	})

	// The step's output, computed by its shell, so the log's copy of the
	// script never matches it.
	for !strings.Contains(log.String(), "sleep-600-started") {
		select {
		case err := <-ran:
			t.Fatalf("the workflow ended before its step started: %v", err)
		case <-ctx.Done():
			t.Fatal("the step did not start within the test's timeout")
		case <-time.After(10 * time.Millisecond):
		}
	}
	m := jobStarted.FindStringSubmatch(p.log.String())
	if m == nil {
		t.Fatalf("the plugin's log names no job process:\n%s", p.log.String())
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}

	cancelStep()
	cancelled := time.Now()
	deadline := cancelled.Add(cancelBound)
	bound := time.NewTimer(cancelBound)
	defer bound.Stop()
	select {
	case err := <-ran:
		t.Logf("the executor returned %v, %v after its context was cancelled", err, time.Since(cancelled).Round(time.Millisecond))
	case <-bound.C:
		t.Fatalf("the executor had not returned %v after its context was cancelled", cancelBound)
	}
	// Reaped: not even a zombie is left.
	for {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job process %d not reaped %v after the executor's context was cancelled", pid, cancelBound)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
