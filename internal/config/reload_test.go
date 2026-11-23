// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// rewrite replaces the config file at path with content.
func rewrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// mustReload reloads c and fails the test on an error.
func mustReload(t *testing.T, c *Config) (*Config, []Change) {
	t.Helper()
	next, changes, err := c.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return next, changes
}

// A reload reads the file again and returns its profiles, leaving the
// running config's own untouched (CFG-7).
func TestReloadReadsTheProfilesAgain(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, "profiles:\n  big: {cpus: 3}\n  gone: {}\n")
	c, err := load(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := Profiles{"big": {CPUs: 3, MemoryMiB: 1024, ScratchGiB: 10}, "gone": BuiltinProfile()}
	rewrite(t, path, "profiles:\n  big: {cpus: 5, memory_mib: 2048}\n  default: {cpus: 1}\n")

	next, changes := mustReload(t, c)
	want := Profiles{"big": {CPUs: 5, MemoryMiB: 2048, ScratchGiB: 10}, "default": {CPUs: 1, MemoryMiB: 1024, ScratchGiB: 10}}
	if !reflect.DeepEqual(next.Profiles, want) {
		t.Errorf("reloaded profiles %+v, want %+v", next.Profiles, want)
	}
	if len(changes) != 0 {
		t.Errorf("changes %+v, want none", changes)
	}
	if !reflect.DeepEqual(c.Profiles, old) {
		t.Errorf("the running config's profiles changed to %+v", c.Profiles)
	}
	// The next reload starts from the new config.
	rewrite(t, path, "")
	if again, _ := mustReload(t, next); len(again.Profiles) != 0 {
		t.Errorf("profiles after a reload of an empty file: %+v", again.Profiles)
	}
}

// A plugin-level key whose value changed is reported with both values, and
// keeps its running value; the profiles still reload.
func TestReloadKeepsPluginLevelValues(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	path := writeConfig(t, dirsConfig(base, "run/rm.sock", "state", "cache", "run/jobs")+"max_memory_mib: 4096\n")
	c, err := load(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	running := *c
	rewrite(t, path, dirsConfig(base, "run/other.sock", "state2", "cache", "run/jobs")+
		"max_memory_mib: 8192\nprofiles:\n  big: {cpus: 4}\n")

	next, changes := mustReload(t, c)
	want := []Change{
		{Key: "listen", Running: "unix://" + base + "/run/rm.sock", File: "unix://" + base + "/run/other.sock"},
		{Key: "state_dir", Running: base + "/state", File: base + "/state2"},
		{Key: "max_memory_mib", Running: "4096", File: "8192"},
	}
	if !reflect.DeepEqual(changes, want) {
		t.Errorf("changes %+v, want %+v", changes, want)
	}
	if next.Listen != running.Listen || next.StateDir != running.StateDir || next.CacheDir != running.CacheDir ||
		next.RuntimeDir != running.RuntimeDir || next.MaxMemoryMiB != running.MaxMemoryMiB {
		t.Errorf("a plugin-level value changed: %+v, running %+v", next, running)
	}
	if next.Profiles["big"].CPUs != 4 {
		t.Errorf("the profiles did not reload: %+v", next.Profiles)
	}
}

// A reload checks profiles against the max_memory_mib in effect, not one
// the file sets but a restart hasn't applied.
func TestReloadChecksProfilesAgainstTheRunningMaxMemory(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, "max_memory_mib: 2048\n")
	c, err := load(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	rewrite(t, path, "max_memory_mib: 8192\nprofiles:\n  big:\n    memory_mib: 4096\n")
	if _, _, err := c.Reload(); err == nil {
		t.Error("a profile above the running max_memory_mib reloaded")
	} else {
		wantErr(t, path, err.Error(), `line 4: profile "big": memory_mib: 4096 is more than max_memory_mib in effect, 2048`)
	}

	// A lower limit in the file applies only at a restart.
	rewrite(t, path, "max_memory_mib: 8192\n")
	c, err = load(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	rewrite(t, path, "max_memory_mib: 1024\nprofiles:\n  big: {memory_mib: 4096}\n")
	if next, _ := mustReload(t, c); next.MaxMemoryMiB != 8192 || next.Profiles["big"].MemoryMiB != 4096 {
		t.Errorf("got max_memory_mib %d and %+v", next.MaxMemoryMiB, next.Profiles)
	}
}

// A file that is invalid at reload is an error, with the same line, key and
// profile as at startup, and the running config is unchanged (CFG-8).
func TestFailedReload(t *testing.T) {
	t.Parallel()
	const valid = "profiles:\n  big: {cpus: 3}\n"
	for _, tc := range []struct {
		name    string
		content string // empty: the file is deleted
		want    string
	}{
		{"YAML error", "profiles:\n  big: {cpus: 3\n", "yaml: line 1: did not find expected ',' or '}'"},
		{"unknown key", "profiles:\n  big:\n    cpus: 5\n    gpus: 1\n", `line 4: profile "big": gpus: unknown key`},
		{"invalid value", "profiles:\n  big:\n    cpus: 0\n", `line 3: profile "big": cpus: must be from 1 to 255, not 0`},
		{"invalid plugin-level value", "state_dir: rel\nprofiles:\n  big: {cpus: 5}\n", `line 1: state_dir: must be an absolute path, not "rel"`},
		{"missing named file", "", "open @PATH@: no such file or directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeConfig(t, valid)
			c, err := load(path, true, nil)
			if err != nil {
				t.Fatal(err)
			}
			before := *c
			if tc.content == "" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				rewrite(t, path, tc.content)
			}
			next, changes, err := c.Reload()
			if err == nil {
				t.Fatalf("reloaded %+v, %+v; want an error", next, changes)
			}
			if next != nil || changes != nil {
				t.Errorf("a failed reload returned %+v, %+v", next, changes)
			}
			wantErr(t, path, err.Error(), strings.ReplaceAll(tc.want, "@PATH@", path))
			if !reflect.DeepEqual(*c, before) || c.Profiles["big"].CPUs != 3 {
				t.Errorf("the running config changed: %+v", c)
			}
		})
	}
}

// At the default path, a reload follows startup's rules: a file created
// since is read, and no file means built-in profiles again.
func TestReloadAtTheDefaultPath(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	c, err := load(path, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	rewrite(t, path, "profiles:\n  big: {cpus: 3}\n")
	next, _ := mustReload(t, c)
	if next.File != path || next.Profiles["big"].CPUs != 3 {
		t.Errorf("File %q, profiles %+v; want %s and big", next.File, next.Profiles, path)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	none, _ := mustReload(t, next)
	if none.File != "" || len(none.Profiles) != 0 {
		t.Errorf("File %q, profiles %+v; want no file and no profiles", none.File, none.Profiles)
	}
}

// A reload reads the absolute path startup resolved, whatever the working
// directory is by then.
//
// It is not parallel: it changes the working directory.
func TestReloadReadsTheResolvedPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rm.yaml")
	rewrite(t, path, "profiles:\n  big: {cpus: 3}\n")
	t.Chdir(dir)
	c, err := Load(func(k string) string {
		if k == PathEnv {
			return "rm.yaml"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	rewrite(t, path, "profiles:\n  big: {cpus: 4}\n")
	next, _ := mustReload(t, c)
	if next.File != path || next.Profiles["big"].CPUs != 4 {
		t.Errorf("File %q, profiles %+v", next.File, next.Profiles)
	}
}

// A FIFO at the config path, with no writer, fails startup and a reload
// at once, naming the path, instead of blocking in the read; the failed
// reload keeps the running config.
func TestFIFOFailsPromptly(t *testing.T) {
	t.Parallel()
	// within runs f under a deadline, so a read that blocks fails the test.
	within := func(f func() error) error {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- f() }()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("blocked reading a FIFO")
			return nil
		}
	}
	mkfifo := func(path string) {
		t.Helper()
		if err := unix.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const want = ": not a regular file (p---------)"

	fifo := filepath.Join(t.TempDir(), "config.yaml")
	mkfifo(fifo)
	for _, named := range []bool{true, false} {
		err := within(func() error { _, err := load(fifo, named, nil); return err })
		if err == nil || err.Error() != "config "+fifo+want {
			t.Errorf("load (named %v): %v, want config %s%s", named, err, fifo, want)
		}
	}

	path := writeConfig(t, "profiles:\n  big: {cpus: 3}\n")
	c, err := load(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := *c
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	mkfifo(path)
	err = within(func() error {
		next, changes, err := c.Reload()
		if next != nil || changes != nil {
			t.Errorf("a failed reload returned %+v, %+v", next, changes)
		}
		return err
	})
	if err == nil || err.Error() != "config "+path+want {
		t.Errorf("Reload: %v, want config %s%s", err, path, want)
	}
	if !reflect.DeepEqual(*c, before) || c.Profiles["big"].CPUs != 3 {
		t.Errorf("the running config changed: %+v", c)
	}
}
