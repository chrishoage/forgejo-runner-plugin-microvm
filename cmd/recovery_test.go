// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	pluginv1alpha "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
)

// rwDiskProfile is the profile rw: a read-write disk at /data, in a VM as
// small as boot allows.
func rwDiskProfile(disk string) string {
	return fmt.Sprintf(`profiles:
  rw:
    cpus: 1
    memory_mib: 512
    disks: [{source: %s, target: /data, read_write: true}]
`, disk)
}

// writeUnflushed has the job v write a file to its read-write disk at
// /data, with no sync, so the VM holds writes the host may not have.
func writeUnflushed(t *testing.T, v *vm) {
	t.Helper()
	if code, out := as(t, v, "0", `echo unflushed > /data/unflushed`); code != 0 {
		t.Fatalf("the step exited %d:\n%s", code, out)
	}
}

// wantMountsReadWrite boots a job on p with the label argument ref and
// fails t unless the disk at /data, the host file disk, is mounted
// read-write and the step can write it. The killed job left disk needing
// recovery, as a read-write mount marks it, so the next mount replays its
// journal; whether the killed job's unflushed writes survived is not
// checked: STO-10 allows losing them.
func wantMountsReadWrite(t *testing.T, p *plugin, disk, name, ref string) {
	t.Helper()
	if !needsRecovery(t, disk) {
		t.Error("the killed job's disk needs no recovery: the test does not reach the journal's replay")
	}
	v := bootImage(t, p, name, ref)
	if code, out := as(t, v, "0", `
opts=$(awk '$2 == "/data" { o = $4 } END { print o }' /proc/mounts)
case ",$opts," in *,rw,*) ;; *) echo "options: $opts"; exit 1;; esac
echo next > /data/next && sync && test "$(cat /data/next)" = next`); code != 0 {
		t.Errorf("the next job's step exited %d:\n%s", code, out)
	}
}

// waitGone waits until every process in procs is gone, not even a zombie,
// failing t after bound.
func waitGone(t *testing.T, procs []proc, bound time.Duration, after string) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for {
		var left []proc
		for _, q := range procs {
			if q.alive() {
				left = append(left, q)
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still there %v after %s: %v", bound, after, left)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// STO-10, a killed VM: a job writes to a read-write disk, and the test
// kills its job process's group, the VM child in it, then removes the job,
// as the runner always does: its Remove finds the VM gone and still
// succeeds, and frees the disk (QUE-7) without touching it. The next job on
// that disk mounts it read-write and boots: no disk lock is left held, and
// the mount replays the journal the kill left.
func TestRecoveryAfterAKilledVM(t *testing.T) {
	t.Parallel()
	img, host := baseImage(t)
	disk := ext4Disk(t, "killed-vm")
	p := startPluginWith(t, buildPlugin(t), rwDiskProfile(disk))
	ref := pushImage(t, host, "killed-vm", img) + ";profile=rw"

	first := bootImage(t, p, "killed-vm-1", ref)
	writeUnflushed(t, first)
	m := jobStarted.FindStringSubmatch(p.log.String())
	if m == nil {
		t.Fatalf("the plugin's log names no job process:\n%s", p.log.String())
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	jobProc, _, ok := readProc(pid)
	if !ok {
		t.Fatalf("job process %d is not running", pid)
	}
	procs := append([]proc{jobProc}, descendants(pid)...)
	if len(procs) < 2 {
		t.Fatalf("job process %d has no VM child", pid)
	}
	// The job process leads its group, which holds its VM child.
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// The plugin, their subreaper, reaps them.
	waitGone(t, procs, 10*time.Second, "the job process's group was killed")
	first.remove(t)
	wantMountsReadWrite(t, p, disk, "killed-vm-2", ref)
}

// STO-10, a killed plugin: a job writes to a read-write disk, and the test
// kills the plugin with SIGKILL, waits until every process it started is
// gone, and starts it again on the same directories. The next job on that
// disk mounts it read-write and boots.
func TestRecoveryAfterAKilledPlugin(t *testing.T) {
	t.Parallel()
	img, host := baseImage(t)
	disk := ext4Disk(t, "killed-plugin")
	p := startPluginWith(t, buildPlugin(t), rwDiskProfile(disk))
	ref := pushImage(t, host, "killed-plugin", img) + ";profile=rw"

	first := bootImage(t, p, "killed-plugin-1", ref)
	writeUnflushed(t, first)
	// Its plugin is gone: nothing can remove it.
	first.removed = true
	procs := descendants(p.pid)
	if len(procs) < 2 {
		t.Fatalf("the plugin has no job process and VM child: %v", procs)
	}
	if err := syscall.Kill(p.pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// The liveness pipe and Pdeathsig end them; init reaps them.
	waitGone(t, procs, dieBound, "the plugin was killed")
	wantMountsReadWrite(t, p.restart(t), disk, "killed-plugin-2", ref)
}

// VM-7, startup cleanup: with a job started and an Exec of a long sleep
// running in it, the test kills the plugin with SIGKILL and starts it again
// on the same directories as soon as its process is gone. As soon as the
// new plugin serves, <state>/jobs/, <runtime>/ and <cache>/jobs/ hold
// nothing but the lock and the listen socket; within dieBound no process of
// the old job is left; and a kept log planted at <state>/logs/<id>/ before the restart
// is still there. A fresh job then runs the echo workflow.
func TestStartupCleanupAfterAKilledPlugin(t *testing.T) {
	t.Parallel()
	p := startPlugin(t, buildPlugin(t))
	v := bootVM(t, p, "startup-cleanup")
	// Its plugin is gone: nothing can remove it.
	v.removed = true
	execSleep(t, v)
	base := filepath.Dir(p.state)
	for _, d := range []string{filepath.Join(p.state, "jobs", v.id), filepath.Join(base, "runtime", v.id), filepath.Join(base, "cache", "jobs", v.id)} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("the running job has no %s: %v", d, err)
		}
	}
	logs := filepath.Join(p.state, "logs", v.id)
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "job.log"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	procs := descendants(p.pid)
	if len(procs) < 2 {
		t.Fatalf("the plugin has no job process and VM child: %v", procs)
	}

	if err := syscall.Kill(p.pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// Its directory locks are released once its process is gone.
	select {
	case <-p.exited:
	case <-time.After(dieBound):
		t.Fatalf("the plugin still runs %v after SIGKILL", dieBound)
	}
	q := p.restart(t)
	wantNoJobFiles(t, q, true)
	// A VM child of the killed plugin may still be dying, reaped by init:
	// it gets dieBound, as in the other kill tests.
	waitGone(t, procs, dieBound, "the new plugin served")
	if b, err := os.ReadFile(filepath.Join(logs, "job.log")); err != nil || string(b) != "kept" {
		t.Errorf("the kept log in %s: %q, %v; want it untouched", logs, b, err)
	}
	if !strings.Contains(q.log.String(), `msg="deleted crash leftovers"`) {
		t.Errorf("the new plugin's log does not say it deleted crash leftovers:\n%s", q.log.String())
	}

	log, err := runWorkflowOn(t, q, filepath.Join(workflows, "echo.yml"), map[string]string{"microvm": readImage(t)})
	if err != nil {
		t.Fatalf("the echo workflow failed after the restart: %v", err)
	}
	if !strings.Contains(log, stepOut) {
		t.Errorf("the job log does not show the first step's output %q", stepOut)
	}
}

// execSleep runs a long sleep in v with Exec and returns once the guest's
// shell says it is about to sleep, leaving the Exec running until its
// stream ends, at the latest on cleanup.
func execSleep(t *testing.T, v *vm) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	stream, err := v.c.Exec(ctx, &pluginv1alpha.ExecRequest{
		EnvironmentId: v.id, Command: []string{"sh", "-c", "echo sleeping; exec sleep 1000"}, Workdir: "/",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	var out strings.Builder
	for !strings.Contains(out.String(), "sleeping\n") {
		o, err := stream.Recv()
		if err != nil {
			t.Fatalf("Exec of the sleep ended before it slept: %v\n%s", err, out.String())
		}
		if o.GetExecComplete() != nil || o.GetExecFailed() != nil {
			t.Fatalf("Exec of the sleep ended before it slept: %v\n%s", o, out.String())
		}
		out.Write(o.GetData().GetData())
	}
	// The kill, or cleanup's cancel, ends the stream.
	var drained sync.WaitGroup
	drained.Go(func() {
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	})
	t.Cleanup(func() { cancel(); drained.Wait() })
}
