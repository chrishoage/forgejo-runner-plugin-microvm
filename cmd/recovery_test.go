// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"fmt"
	"strconv"
	"syscall"
	"testing"
	"time"
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
// kills its job process's group, the VM child in it, without a Remove. The
// next job on that disk mounts it read-write and boots: no disk lock is
// left held, and the mount replays the journal the kill left.
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
	wantMountsReadWrite(t, p, disk, "killed-vm-2", ref)
	// The first job's Remove, on cleanup, finds its VM gone, and still
	// succeeds.
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
