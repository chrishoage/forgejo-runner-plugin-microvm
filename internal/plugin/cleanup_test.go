// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/joblaunch"
)

// leftovers are the files a crashed plugin left in a job's directories,
// planted by plantLeftovers: every path is one startup cleanup deletes.
type leftovers struct {
	paths []string
}

// shortTempDir returns a new directory, deleted when the test ends, in the
// host's temporary directory itself, not t.TempDir: the stale agent socket
// plantLeftovers binds is at <runtime>/<id>/0/, and its path holds at most
// 107 bytes, which a test's name under CI's temp directory passes.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// plantLeftovers plants in dirs what a plugin killed mid-job leaves in
// <state>/jobs/ and <runtime>/: a job's nested state directory with a
// read-only subtree, a dangling symlink, a symlink to a directory outside,
// a FIFO, and a stale agent socket at <runtime>/<id>/0/. outside is the
// directory the symlink names, which cleanup must leave alone.
func plantLeftovers(t *testing.T, dirs Dirs, outside string) leftovers {
	t.Helper()
	const id = "4f1c2d"
	jobs := filepath.Join(dirs.State, "jobs")
	// Should cleanup miss them, the read-only subtree would fail the
	// temporary directory's removal.
	t.Cleanup(func() { _ = removeTree(jobs) })
	ro := filepath.Join(jobs, id, "0", "root", "ro")
	for _, d := range []string{filepath.Join(ro, "sub"), filepath.Join(dirs.Runtime, id, "0")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(jobs, id, "job.log"), filepath.Join(ro, "sub", "file")} {
		if err := os.WriteFile(f, []byte("left"), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{filepath.Join(ro, "sub"), ro} {
		if err := os.Chmod(d, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dirs.State, "nowhere"), filepath.Join(dirs.Runtime, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(jobs, "outside")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dirs.Runtime, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	leaveSocket(t, filepath.Join(dirs.Runtime, id, "0", "agent.sock"))
	return leftovers{paths: []string{
		filepath.Join(jobs, id), filepath.Join(jobs, "outside"),
		filepath.Join(dirs.Runtime, id), filepath.Join(dirs.Runtime, "dangling"), filepath.Join(dirs.Runtime, "fifo"),
	}}
}

// wantDeleted fails t unless every leftover is gone.
func (l leftovers) wantDeleted(t *testing.T) {
	t.Helper()
	for _, p := range l.paths {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("startup cleanup left %s: %v", p, err)
		}
	}
}

// plantLogs plants a kept log at <state>/logs/<id>/ and returns its
// directory.
func plantLogs(t *testing.T, dirs Dirs) string {
	t.Helper()
	logs := filepath.Join(dirs.State, "logs", "9a8b7c")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "job.log"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	return logs
}

// wantLogsKept fails t unless the log plantLogs planted in logs is there,
// unchanged.
func wantLogsKept(t *testing.T, logs string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(logs, "job.log"))
	if err != nil || string(b) != "kept" {
		t.Errorf("the kept log in %s: %q, %v; want it untouched", logs, b, err)
	}
}

// wantLocks fails t unless every directory's lock file is there.
func wantLocks(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if fi, err := os.Lstat(filepath.Join(d, lockFile)); err != nil || !fi.Mode().IsRegular() {
			t.Errorf("the lock file of %s: %v, %v", d, fi, err)
		}
	}
}

// inode returns the inode number of the file at p, not following a
// symlink.
func inode(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

// libsFails is a libs that fails, so run stops after startup cleanup and
// before it serves.
const libsFails = "echo 'the fake libs fails' >&2\nexit 1"

// A plugin started on a killed plugin's directories, the listen socket's
// directory being the runtime directory, deletes every per-job leftover in
// <state>/jobs/ and <runtime>/, before it checks the libraries and binds
// its socket. It keeps every lock file, the killed plugin's listen socket
// and its temporary one, which listen deals with, and <state>/logs/, and
// logs one line with the counts.
//
// It is not parallel: it sets PATH.
func TestStartupCleanup(t *testing.T) {
	fakeMkfsOnPath(t)
	dirs := testDirs(filepath.Join(shortTempDir(t), "plugin"))
	l := mustLock(t, dirs)
	logs := plantLogs(t, dirs)
	left := plantLeftovers(t, dirs, logs)
	socks := socketPaths(dirs)
	for _, s := range socks {
		leaveSocket(t, s)
	}
	inodes := []uint64{inode(t, socks[0]), inode(t, socks[1])}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	var out logBuffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	err := run(ctx, log, configOf(dirs), nil, nil, fakePayload(libsFails), newImageCache, joblaunch.New)
	if err == nil || !strings.Contains(err.Error(), "the fake libs fails") {
		t.Fatalf("run returned %v, want the fake libs's error", err)
	}
	left.wantDeleted(t)
	wantLocks(t, dirs.State, dirs.Cache, dirs.Runtime)
	for i, s := range socks {
		if got := inode(t, s); got != inodes[i] {
			t.Errorf("%s is inode %d, want the killed plugin's %d", s, got, inodes[i])
		}
	}
	wantLogsKept(t, logs)
	if want := `msg="deleted crash leftovers" state=` + dirs.State + " jobs=2 runtime=" + dirs.Runtime + " runtime_entries=3"; !strings.Contains(out.String(), want) {
		t.Errorf("the log does not say %q:\n%s", want, out.String())
	}
}

// With the listen socket in a directory of its own, a file in <runtime>/
// named as the socket is a leftover like any other. A clean directory
// logs nothing.
func TestStartupCleanupOutsideTheListenDir(t *testing.T) {
	t.Parallel()
	base := shortTempDir(t)
	dirs := testDirs(base)
	dirs.Listen = filepath.Join(base, "listen", "runner-microvm.sock")
	_ = mustLock(t, dirs)
	logs := plantLogs(t, dirs)
	left := plantLeftovers(t, dirs, logs)
	for _, name := range []string{filepath.Base(dirs.Listen), "." + filepath.Base(dirs.Listen) + ".new"} {
		leaveSocket(t, filepath.Join(dirs.Runtime, name))
		left.paths = append(left.paths, filepath.Join(dirs.Runtime, name))
	}
	var out logBuffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	if err := cleanStartup(log, dirs); err != nil {
		t.Fatal(err)
	}
	left.wantDeleted(t)
	wantLocks(t, dirs.State, dirs.Cache, dirs.Runtime, filepath.Dir(dirs.Listen))
	wantLogsKept(t, logs)
	if !strings.Contains(out.String(), "jobs=2") || !strings.Contains(out.String(), "runtime_entries=5") {
		t.Errorf("the log does not count 2 jobs and 5 runtime entries:\n%s", out.String())
	}

	var again logBuffer
	if err := cleanStartup(slog.New(slog.NewTextHandler(&again, nil)), dirs); err != nil {
		t.Fatal(err)
	}
	if again.String() != "" {
		t.Errorf("a clean start logged:\n%s", again.String())
	}
}

// Before the first job there is no <state>/jobs/, and cleanup needs none.
func TestStartupCleanupWithNoJobsDir(t *testing.T) {
	t.Parallel()
	dirs := testDirs(t.TempDir())
	_ = mustLock(t, dirs)
	if err := cleanStartup(discardLog(), dirs); err != nil {
		t.Fatal(err)
	}
}

// An entry startup cleanup can't delete stops startup with an error naming
// its directory, before the libraries are checked or the socket bound: a
// plugin serving on top of it could hand a new job a stale file of the same
// id.
//
// It is not parallel: it sets PATH.
func TestStartupCleanupStopsOnAnEntryItCantDelete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root deletes from a read-only directory")
	}
	fakeMkfsOnPath(t)
	for _, sub := range []string{"state", "runtime"} {
		t.Run(sub, func(t *testing.T) {
			dirs := testDirs(filepath.Join(t.TempDir(), "plugin"))
			_ = mustLock(t, dirs).Close()
			dir := map[string]string{"state": filepath.Join(dirs.State, "jobs"), "runtime": dirs.Runtime}[sub]
			if err := os.MkdirAll(filepath.Join(dir, "4f1c2d"), 0o700); err != nil {
				t.Fatal(err)
			}
			// Its directory read-only, so the entry can't be unlinked.
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			marker := filepath.Join(t.TempDir(), "libs-ran")
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			err := run(ctx, discardLog(), configOf(dirs), nil, nil, fakePayload("touch '"+marker+"'\n"+libsFails), newImageCache, joblaunch.New)
			if err == nil || !strings.Contains(err.Error(), "the directory "+dir+": startup cleanup") {
				t.Fatalf("run returned %v, want startup cleanup's error naming %s", err, dir)
			}
			if _, err := os.Lstat(filepath.Join(dir, "4f1c2d")); err != nil {
				t.Errorf("the entry is gone: %v", err)
			}
			if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("libs ran after startup cleanup failed: %v", err)
			}
			for _, p := range socketPaths(dirs) {
				if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("the failed startup left %s: %v", p, err)
				}
			}
		})
	}
}

// An entry startup cleanup can't delete stops it only after it logs what it
// deleted: here the jobs in <state>/jobs/, before an entry in a read-only
// <runtime>/.
func TestStartupCleanupLogsWhatItDeletedBeforeItStops(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root deletes from a read-only directory")
	}
	t.Parallel()
	dirs := testDirs(t.TempDir())
	_ = mustLock(t, dirs)
	for _, id := range []string{"4f1c2d", "9a0b3e"} {
		if err := os.MkdirAll(filepath.Join(dirs.State, "jobs", id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dirs.Runtime, "4f1c2d"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Its directory read-only, so the entry can't be unlinked.
	if err := os.Chmod(dirs.Runtime, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dirs.Runtime, 0o700) })
	var out logBuffer
	err := cleanStartup(slog.New(slog.NewTextHandler(&out, nil)), dirs)
	if err == nil || !strings.Contains(err.Error(), "the directory "+dirs.Runtime+": startup cleanup") {
		t.Fatalf("cleanStartup returned %v, want its error naming %s", err, dirs.Runtime)
	}
	if want := `msg="deleted crash leftovers" state=` + dirs.State + " jobs=2 runtime=" + dirs.Runtime + " runtime_entries=0"; !strings.Contains(out.String(), want) {
		t.Errorf("the log does not say %q:\n%s", want, out.String())
	}
}
