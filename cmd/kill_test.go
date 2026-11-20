// SPDX-License-Identifier: GPL-3.0-or-later

package cmd_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// killWithJobs kills the plugin at pid the hard way: first the process
// group of each of its children, because a job process leads a group of its
// own, which killing the plugin's group misses, then the plugin's group.
// Once the plugin is gone, its children are another process's, so the
// order matters.
func killWithJobs(pid int) {
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
			if pgid, err := syscall.Getpgid(child); err == nil && pgid != pid {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
			_ = syscall.Kill(child, syscall.SIGKILL)
		}
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// A child in a process group of its own, as a job process is, dies with
// its parent's forced cleanup.
func TestKillWithJobs(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	// With job control on, sh starts the background sleep in a process
	// group of its own.
	cmd := exec.CommandContext(ctx, "sh", "-c", "set -m; sleep 1000 & echo $! >"+pidFile+"; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { killWithJobs(cmd.Process.Pid); return nil }
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var child int
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(b), "\n") {
			if child, err = strconv.Atoi(strings.TrimSpace(string(b))); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	if pgid, err := syscall.Getpgid(child); err != nil || pgid == cmd.Process.Pid {
		t.Fatalf("the child's group is %d (%v), want one of its own", pgid, err)
	}

	cancel()
	_ = cmd.Wait()
	// Reparented and reaped elsewhere once killed.
	deadline = time.Now().Add(10 * time.Second)
	for syscall.Kill(child, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the child in its own group outlived the forced cleanup")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
