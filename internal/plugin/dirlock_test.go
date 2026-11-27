// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/config"
	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/joblaunch"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/testbin"
)

// testDirs returns directories under base: <base>/state, <base>/cache and
// <base>/runtime, with the listen socket in the runtime directory.
func testDirs(base string) Dirs {
	runtime := filepath.Join(base, "runtime")
	return Dirs{
		State:   filepath.Join(base, "state"),
		Cache:   filepath.Join(base, "cache"),
		Runtime: runtime,
		Listen:  filepath.Join(runtime, "runner-microvm.sock"),
	}
}

// configOf returns a config whose directories are dirs.
func configOf(dirs Dirs) *config.Config {
	return &config.Config{
		Listen: dirs.Listen, StateDir: dirs.State, CacheDir: dirs.Cache, RuntimeDir: dirs.Runtime,
		Profiles: map[string]config.Profile{},
	}
}

// mustLock takes dirs' locks, which the test holds until it ends.
func mustLock(t *testing.T, dirs Dirs) *dirLocks {
	t.Helper()
	l, err := lockDirs(dirs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return l
}

// listDir returns the names in dir, or nil when it doesn't exist.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// A second plugin on directories the first one uses fails at startup with an
// error naming the first directory in the lock order that it shares, and
// leaves no lock of its own behind.
func TestSecondPluginFailsNamingTheSharedDirectory(t *testing.T) {
	t.Parallel()
	listenDir := func(d Dirs) string { return filepath.Dir(d.Listen) }
	for _, tc := range []struct {
		name string
		// share makes the second plugin's directories share one with the
		// first's, and returns the directory that is shared.
		share func(first Dirs, second *Dirs) string
	}{
		{"all", func(first Dirs, second *Dirs) string { *second = first; return first.State }},
		{"state", func(first Dirs, second *Dirs) string { second.State = first.State; return first.State }},
		{"cache", func(first Dirs, second *Dirs) string { second.Cache = first.Cache; return first.Cache }},
		{"runtime", func(first Dirs, second *Dirs) string {
			// The second plugin's listen socket stays in its own
			// directory, so only the runtime directory is shared.
			second.Runtime = first.Runtime
			return first.Runtime
		}},
		{"listen", func(first Dirs, second *Dirs) string {
			second.Listen = first.Listen
			return listenDir(first)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			first := testDirs(filepath.Join(tmp, "first"))
			if tc.name == "listen" {
				// A listen-socket directory of its own, to share.
				first.Listen = filepath.Join(tmp, "first", "listen", "runner-microvm.sock")
			}
			second := testDirs(filepath.Join(tmp, "second"))
			second.Listen = filepath.Join(tmp, "second", "listen", "runner-microvm.sock")
			shared := tc.share(first, &second)
			mustLock(t, first)
			before := listDir(t, shared)

			l, err := lockDirs(second)
			if err == nil {
				_ = l.Close()
				t.Fatal("the second plugin took every lock")
			}
			if !strings.Contains(err.Error(), shared) {
				t.Errorf("error %q does not name %s", err, shared)
			}
			for _, d := range []string{first.State, first.Cache, first.Runtime, listenDir(first)} {
				if d != shared && strings.Contains(err.Error(), d) {
					t.Errorf("error %q names %s, which is not shared", err, d)
				}
			}
			if after := listDir(t, shared); strings.Join(after, "/") != strings.Join(before, "/") {
				t.Errorf("the second plugin touched %s: %v, then %v", shared, before, after)
			}
			// The locks the second plugin took before it failed are
			// released: once the first plugin stops, it starts.
			if tc.name != "all" && tc.name != "state" {
				if l, err := lockDirs(second.withoutShared(shared)); err != nil {
					t.Errorf("a lock the failed startup took is still held: %v", err)
				} else {
					_ = l.Close()
				}
			}
		})
	}
}

// The plugin's startup takes the locks before anything else writes to the
// directories: a second startup on directories in use fails naming the state
// directory, and writes nothing to them.
func TestSecondStartupFailsBeforeWriting(t *testing.T) {
	t.Parallel()
	base := filepath.Join(t.TempDir(), "plugin")
	dirs := testDirs(base)
	mustLock(t, dirs)
	before := map[string][]string{}
	for _, d := range []string{dirs.State, dirs.Cache, dirs.Runtime} {
		before[d] = listDir(t, d)
	}

	err := run(t.Context(), discardLog(), configOf(dirs), nil, nil, embeddedPayload, newImageCache, joblaunch.New)
	if err == nil {
		t.Fatal("the second startup ran")
	}
	if !strings.Contains(err.Error(), dirs.State) || !strings.Contains(err.Error(), "in use") {
		t.Errorf("error %q does not say %s is in use", err, dirs.State)
	}
	for d, names := range before {
		if after := listDir(t, d); strings.Join(after, "/") != strings.Join(names, "/") {
			t.Errorf("the second startup wrote to %s: %v, then %v", d, names, after)
		}
	}
}

// The plugin refuses to start without mkfs.ext4, naming it, before it
// writes anything (VM-5).
//
// It is not parallel: it sets PATH.
func TestStartupNeedsMkfsExt4(t *testing.T) {
	base := filepath.Join(t.TempDir(), "plugin")
	t.Setenv("PATH", t.TempDir())

	err := run(t.Context(), discardLog(), configOf(testDirs(base)), nil, nil, embeddedPayload, newImageCache, joblaunch.New)
	if err == nil || !strings.Contains(err.Error(), "mkfs.ext4") {
		t.Fatalf("err = %v, want one naming mkfs.ext4", err)
	}
	if _, err := os.Lstat(base); !os.IsNotExist(err) {
		t.Errorf("the failed startup wrote %s: %v", base, err)
	}
}

// withoutShared returns d with the directory shared replaced by a fresh one
// of its own, so a test can retake the rest of d's locks.
func (d Dirs) withoutShared(shared string) Dirs {
	fresh := shared + "-own"
	switch shared {
	case d.State:
		d.State = fresh
	case d.Cache:
		d.Cache = fresh
	case d.Runtime:
		d.Runtime = fresh
	case filepath.Dir(d.Listen):
		d.Listen = filepath.Join(fresh, filepath.Base(d.Listen))
	}
	return d
}

// Each directory gets its own lock file, and the listen-socket directory
// takes none of its own when it is the runtime directory. Releasing the locks
// lets the next plugin start, and never deletes a lock file.
func TestLockDirsLayout(t *testing.T) {
	t.Parallel()
	dirs := testDirs(filepath.Join(t.TempDir(), "plugin"))
	l, err := lockDirs(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(l.files); n != 3 {
		t.Errorf("took %d locks, want 3: the listen socket is in the runtime directory", n)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{dirs.State, dirs.Cache, dirs.Runtime} {
		if _, err := os.Stat(filepath.Join(d, lockFile)); err != nil {
			t.Errorf("lock file: %v", err)
		}
	}
	mustLock(t, dirs)

	// With a listen-socket directory of its own, it is locked too.
	tmp := t.TempDir()
	other := testDirs(filepath.Join(tmp, "other"))
	other.Listen = filepath.Join(tmp, "listen", "runner-microvm.sock")
	l2 := mustLock(t, other)
	if n := len(l2.files); n != 4 {
		t.Errorf("took %d locks, want 4", n)
	}
	if _, err := os.Stat(filepath.Join(tmp, "listen", lockFile)); err != nil {
		t.Errorf("listen-socket directory lock file: %v", err)
	}
}

// A listen-socket directory that a symlink makes the runtime directory, as
// config.Load sees it, takes no lock of its own either: locking the runtime
// directory's lock file again would fail against the plugin's own lock.
func TestLockDirsListenSymlinkedToRuntime(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// link places a symlink under base and returns dirs that reach the
		// runtime directory through it.
		link func(t *testing.T, base string) Dirs
	}{
		{"listen through a link", func(t *testing.T, base string) Dirs {
			dirs := testDirs(base)
			if err := os.Symlink(dirs.Runtime, filepath.Join(base, "link")); err != nil {
				t.Fatal(err)
			}
			dirs.Listen = filepath.Join(base, "link", "runner-microvm.sock")
			return dirs
		}},
		{"runtime through a link", func(t *testing.T, base string) Dirs {
			dirs := testDirs(base)
			if err := os.Mkdir(dirs.Runtime, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(dirs.Runtime, filepath.Join(base, "link")); err != nil {
				t.Fatal(err)
			}
			dirs.Runtime = filepath.Join(base, "link")
			return dirs
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			dirs := tc.link(t, base)
			l := mustLock(t, dirs)
			if n := len(l.files); n != 3 {
				t.Errorf("took %d locks, want 3: the listen socket is in the runtime directory", n)
			}
			// The runtime directory is still locked against another plugin.
			other := testDirs(filepath.Join(t.TempDir(), "other"))
			other.Runtime = filepath.Join(base, "runtime")
			if l2, err := lockDirs(other); err == nil {
				_ = l2.Close()
				t.Error("another plugin locked the runtime directory")
			}
		})
	}
}

// A job process inherits no lock file descriptor, so it can't keep a
// directory locked after the plugin is gone.
//
// It is not parallel: between fork and exec, a child shares every
// descriptor, close-on-exec ones too, so a fork here would briefly hold the
// locks another test releases and takes again. The plugin takes its locks
// once and holds them for its whole life, so there the window is harmless.
func TestJobProcessInheritsNoLock(t *testing.T) {
	tmp := t.TempDir()
	dirs := testDirs(filepath.Join(tmp, "plugin"))
	dirs.Listen = filepath.Join(tmp, "listen", "runner-microvm.sock")
	l, err := lockDirs(dirs)
	if err != nil {
		t.Fatal(err)
	}
	locks := map[string]bool{}
	for _, f := range l.files {
		// /proc shows a descriptor's path with symlinks resolved.
		path, err := filepath.EvalSymlinks(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		locks[path] = true
	}

	// The real launcher, with a shell script standing in for the job
	// binary: no test runs its own binary.
	bin := filepath.Join(tmp, "runner-microvm-job")
	testbin.Write(t, bin, "#!/bin/sh\ncat >/dev/null\necho $$ >&2\nexec sleep 1000\n")
	launcher, err := joblaunch.New(discardLog())
	if err != nil {
		_ = l.Close()
		t.Fatal(err)
	}
	t.Cleanup(launcher.Close)
	logPath := filepath.Join(tmp, "job.log")
	p, err := launcher.Launch(t.Context(), &jobv1.Spec{Job: "job-1", JobBinary: bin}, logPath)
	if err != nil {
		_ = l.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := launcher.Kill(ctx, p); err != nil {
			t.Errorf("Kill: %v", err)
		}
	})
	var pid string
	deadline := time.Now().Add(30 * time.Second)
	for {
		if b, _ := os.ReadFile(logPath); strings.HasSuffix(string(b), "\n") {
			pid = strings.TrimSpace(string(b))
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the job process never started")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// None of the job process's descriptors is a lock file.
	fdDir := filepath.Join("/proc", pid, "fd")
	ents, err := os.ReadDir(fdDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil {
			continue // a descriptor closed meanwhile
		}
		if locks[target] {
			t.Errorf("the job process holds %s on fd %s", target, e.Name())
		}
	}

	// So once the plugin's locks are released, the next plugin starts
	// while the job process still runs.
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	mustLock(t, dirs)
}
