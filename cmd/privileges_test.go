// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// procStatus returns the fields of pid's /proc/<pid>/status by name.
func procStatus(t *testing.T, pid int) map[string]string {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{}
	for line := range strings.Lines(string(b)) {
		if name, value, ok := strings.Cut(line, ":"); ok {
			fields[name] = strings.TrimSpace(value)
		}
	}
	return fields
}

// wantUnprivileged fails t unless pid's real, effective, saved and
// filesystem uids are all uid and its effective capability set is empty.
func wantUnprivileged(t *testing.T, what string, pid, uid int) {
	t.Helper()
	status := procStatus(t, pid)
	want := strings.Repeat(strconv.Itoa(uid)+" ", 4)
	if got := strings.Join(strings.Fields(status["Uid"]), " ") + " "; got != want {
		t.Errorf("%s %d: Uid %q, want %q, the plugin's, four times", what, pid, status["Uid"], strings.TrimSpace(want))
	}
	capEff, err := strconv.ParseUint(status["CapEff"], 16, 64)
	if err != nil {
		t.Errorf("%s %d: CapEff %q: %v", what, pid, status["CapEff"], err)
	} else if capEff != 0 {
		t.Errorf("%s %d: CapEff %016x, want none", what, pid, capEff)
	}
}

// TestNoPrivileges is VM-9's requirement that the plugin needs nothing but
// /dev/kvm: with the plugin run as the test's user, the job process and
// the VM child of a started job run under that uid with an empty effective
// capability set, so neither a setuid helper nor a file capability has crept
// in. As root only the uid could hold, so it skips there.
func TestNoPrivileges(t *testing.T) {
	t.Parallel()
	uid := os.Getuid()
	if uid == 0 {
		t.Skip("SKIPPED: the test runs as root, whose job processes hold every capability")
	}
	p := startPlugin(t, buildPlugin(t))
	bootVM(t, p, "no-privileges")
	var jobProcesses, vmChildren int
	for _, q := range descendants(p.pid) {
		pgid, err := syscall.Getpgid(q.pid)
		if err != nil {
			t.Fatalf("%d (%s): %v", q.pid, q.comm, err)
		}
		// A job process leads a group of its own; its VM child is in it.
		// Anything else the plugin runs, such as mkfs.ext4, has exited
		// once Start has returned.
		what := "the VM child"
		switch {
		case pgid == q.pid:
			what = "the job process"
			jobProcesses++
		case pgid != p.pid:
			vmChildren++
		default:
			what = "the plugin's child"
		}
		t.Logf("%s: %d (%s)", what, q.pid, q.comm)
		wantUnprivileged(t, what, q.pid, uid)
	}
	if jobProcesses != 1 || vmChildren != 1 {
		t.Fatalf("the plugin's descendants hold %d job processes and %d VM children, want one of each", jobProcesses, vmChildren)
	}
	wantUnprivileged(t, "the plugin", p.pid, uid)
}
