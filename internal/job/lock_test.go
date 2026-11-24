// SPDX-License-Identifier: GPL-3.0-or-later

package job

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
)

// diskFile makes an empty disk file in a fresh directory and returns its
// path.
func diskFile(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// hold opens p on an open file of the test's own and takes how, LOCK_EX or
// LOCK_SH, on it, failing the test if it can't; the file is closed, and the
// lock released, on cleanup or by the returned func.
func hold(t *testing.T, p string, how int) func() {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB); err != nil {
		_ = f.Close()
		t.Fatalf("lock %s: %v", p, err)
	}
	release := func() { _ = f.Close() }
	t.Cleanup(release)
	return release
}

// locked reports whether how, LOCK_EX or LOCK_SH, on p would have to wait
// for another holder.
func locked(t *testing.T, p string, how int) bool {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	err = unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
	if err != nil && !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("lock %s: %v", p, err)
	}
	return err != nil
}

// freed reports whether how, LOCK_EX or LOCK_SH, on p stops having to wait
// within a few seconds. A lock freed by closing its last open file isn't
// free at once while another test in this process is starting a child:
// from fork until exec closes it, the child holds a copy of every open
// file, close-on-exec ones included. So a test asserting that a lock is
// free waits for it; one asserting that it is held needs no wait, since
// such a copy can only hold a lock longer.
func freed(t *testing.T, p string, how int) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for locked(t, p, how) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}

// A read-write disk takes an exclusive lock, which fails against an
// exclusive or a shared holder; a read-only disk takes a shared one, which
// fails against an exclusive holder and succeeds beside a shared one. The
// failure's text is the same whichever lock the job wanted (STO-7, STO-8).
func TestOpenDisksLocks(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		held      int
		readWrite bool
		inUse     bool
	}{
		"read-write against exclusive": {unix.LOCK_EX, true, true},
		"read-write against shared":    {unix.LOCK_SH, true, true},
		"read-only against exclusive":  {unix.LOCK_EX, false, true},
		"read-only against shared":     {unix.LOCK_SH, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := diskFile(t, "disk.img")
			release := hold(t, p, tt.held)
			files, err := openDisks([]*jobv1.Disk{{Source: p, BlockId: "disk0", ReadWrite: tt.readWrite}})
			defer closeAll(files)
			if !tt.inUse {
				if err != nil {
					t.Fatalf("openDisks: %v", err)
				}
				release()
				if !locked(t, p, unix.LOCK_EX) {
					t.Error("the job's shared lock is gone")
				}
				return
			}
			if want := "disk " + p + " is in use by another process"; err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if files != nil {
				t.Errorf("files = %v, want none", files)
			}
		})
	}
}

// A disk that fails to lock closes every disk opened before it, so their
// locks are free again.
func TestOpenDisksReleasesOnFailure(t *testing.T) {
	t.Parallel()
	first, second := diskFile(t, "first.img"), diskFile(t, "second.img")
	hold(t, second, unix.LOCK_SH)
	files, err := openDisks([]*jobv1.Disk{
		{Source: first, BlockId: "disk0", ReadWrite: true},
		{Source: second, BlockId: "disk1", ReadWrite: true},
	})
	closeAll(files)
	if want := "disk " + second + " is in use by another process"; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if !freed(t, first, unix.LOCK_EX) {
		t.Errorf("%s is still locked after the failure", first)
	}
}

// A disk listed twice read-only, as a profile may, takes two shared locks
// in one process, which don't block each other.
func TestOpenDisksSharesAReadOnlyDiskTwice(t *testing.T) {
	t.Parallel()
	p := diskFile(t, "disk.img")
	files, err := openDisks([]*jobv1.Disk{{Source: p, BlockId: "disk0"}, {Source: p, BlockId: "disk1"}})
	defer closeAll(files)
	if err != nil {
		t.Fatalf("openDisks: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("%d files, want 2", len(files))
	}
}

// The lock belongs to the open file, so a child that inherited it, as the
// VM child does, keeps the disk locked after the job process has closed
// its copy, and the lock is free again once that holder exits. A holder
// waits for its lock to be dropped no longer than it lives.
func TestDiskLockLastsAsLongAsItsHolder(t *testing.T) {
	t.Parallel()
	p := diskFile(t, "disk.img")
	files, err := openDisks([]*jobv1.Disk{{Source: p, BlockId: "disk0", ReadWrite: true}})
	if err != nil {
		t.Fatalf("openDisks: %v", err)
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("sleep, which the dev shell provides: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, sleep, "60")
	cmd.ExtraFiles = files
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		closeAll(files)
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
			_ = cmd.Wait()
		}
	})
	closeAll(files)
	if !locked(t, p, unix.LOCK_SH) {
		t.Fatal("the disk is free while a child holding its open file lives")
	}
	_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
	_ = cmd.Wait()
	waited = true
	if !freed(t, p, unix.LOCK_EX) {
		t.Error("the disk is still locked after its holder exited")
	}
}

// The job process holds its disks' locks while its VM child runs, and
// they are free once it has ended; mkfs, which the job process runs after
// taking them, never inherits one.
func TestRunHoldsTheDiskLocks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := diskFile(t, "disk.img")
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	// The fake VM child closes the inherited disk, so only the job
	// process's copy keeps it locked while it waits.
	spec := testJobSpec(t, fakeJobBinary(t, "exec 5<&-\ntouch "+started+
		"\nwhile [ ! -e "+release+" ]; do sleep 0.01; done"))
	// The fake mkfs fails when it holds the disk open.
	spec.MkfsExt4 = fakeMkfs(t, "for fd in /proc/$$/fd/*; do "+
		"[ \"$(readlink $fd)\" != "+p+" ] || { echo mkfs holds the disk as fd ${fd##*/}; exit 1; }; done")
	spec.Vms[0].Disks = []*jobv1.Disk{{Source: p, BlockId: "disk0", ReadWrite: true}}
	type result struct {
		events []*jobv1.Event
		err    error
	}
	done := make(chan result, 1)
	go func() {
		events, err := runJob(t, spec)
		done <- result{events, err}
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
	})
	deadline := time.Now().Add(time.Minute)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case r := <-done:
			t.Fatalf("the job ended before its VM child started: %v, %v", r.err, r.events)
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the VM child never started")
		}
	}
	if !locked(t, p, unix.LOCK_SH) {
		t.Error("the disk is free while its job runs")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("the job failed: %v", r.err)
	}
	if !freed(t, p, unix.LOCK_EX) {
		t.Error("the disk is still locked after its job ended")
	}
}
