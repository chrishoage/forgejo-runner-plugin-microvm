// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// dieBound is how soon after the plugin is killed every process it started
// must be gone.
const dieBound = 5 * time.Second

// proc is a process, told from a later one with its pid by its start time.
type proc struct {
	pid   int
	start string
	comm  string
}

// readProc reads pid's start time, name and state from /proc, or reports
// that no such process exists.
func readProc(pid int) (p proc, state string, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return proc{}, "", false
	}
	// pid (comm) state ppid ...: comm may hold spaces and parentheses,
	// so the fields after it start after its last ')'. The start time
	// is field 22 of the whole line.
	s := string(b)
	i, j := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if i < 0 || j < i {
		return proc{}, "", false
	}
	rest := strings.Fields(s[j+1:])
	if len(rest) < 20 {
		return proc{}, "", false
	}
	return proc{pid: pid, start: rest[19], comm: s[i+1 : j]}, rest[0], true
}

// alive reports whether p still exists, a zombie included: the same pid
// with the same start time.
func (p proc) alive() bool {
	q, _, ok := readProc(p.pid)
	return ok && q.start == p.start
}

// descendants returns every process below pid, found through each of its
// threads' children in /proc.
func descendants(pid int) []proc {
	var out []proc
	tasks, _ := filepath.Glob("/proc/" + strconv.Itoa(pid) + "/task/*/children")
	for _, f := range tasks {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(string(b)) {
			child, err := strconv.Atoi(field)
			if err != nil {
				continue
			}
			if p, _, ok := readProc(child); ok {
				out = append(out, p)
				out = append(out, descendants(child)...)
			}
		}
	}
	return out
}

// TestPluginKilled is VM-6: a job's processes die with the plugin, also on
// kill -9. While a job's step sleeps, the test records the plugin's
// descendants, among them the job process and its VM child, sends the
// plugin SIGKILL, and checks that every one is gone, not even a zombie,
// within 5 s.
func TestPluginKilled(t *testing.T) {
	t.Parallel()
	wf := writeWorkflow(t, step("sleep", `echo "sleep-$((6 * 100))-started"; sleep 600`))
	p := startPlugin(t, buildPlugin(t))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	log := &syncBuffer{}
	run := planExecutor(t, p, wf, map[string]string{"microvm": readImage(t)}, log)
	ran := make(chan error, 1)
	done := make(chan struct{})
	go func() { defer close(done); ran <- run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the executor did not return 30 s after the test's end")
		}
		logIfFailed(t, "job log", log)
	})

	for !strings.Contains(log.String(), "sleep-600-started") {
		select {
		case err := <-ran:
			t.Fatalf("the workflow ended before its step started: %v", err)
		case <-ctx.Done():
			t.Fatal("the step did not start within the test's timeout")
		case <-time.After(10 * time.Millisecond):
		}
	}
	procs := descendants(p.pid)
	// Whatever this test misses, it kills on its way out.
	t.Cleanup(func() {
		for _, q := range procs {
			if q.alive() {
				_ = syscall.Kill(q.pid, syscall.SIGKILL)
			}
		}
	})
	var jobProcess, vmChild bool
	for _, q := range procs {
		t.Logf("descendant %d (%s)", q.pid, q.comm)
		pgid, err := syscall.Getpgid(q.pid)
		if err != nil {
			continue
		}
		// A job process leads a group of its own; its VM child is in it.
		jobProcess = jobProcess || pgid == q.pid
		vmChild = vmChild || (pgid != q.pid && pgid != p.pid)
	}
	if !jobProcess || !vmChild {
		t.Fatalf("the plugin's descendants hold no job process and VM child: %v", procs)
	}

	if err := syscall.Kill(p.pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	killed := time.Now()
	for {
		var left []proc
		for _, q := range procs {
			if q.alive() {
				left = append(left, q)
			}
		}
		if len(left) == 0 {
			t.Logf("every descendant was gone %v after the plugin was killed", time.Since(killed).Round(time.Millisecond))
			break
		}
		if time.Since(killed) > dieBound {
			for _, q := range left {
				_, state, _ := readProc(q.pid)
				t.Errorf("%d (%s) is still there, state %s, %v after the plugin was killed", q.pid, q.comm, state, dieBound)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The executor sees its plugin gone; it must not hang on it.
	cancel()
	select {
	case err := <-ran:
		if err == nil {
			t.Error("the executor succeeded with its plugin killed")
		}
	case <-time.After(30 * time.Second):
		t.Error("the executor did not return once its plugin was killed")
	}
}
