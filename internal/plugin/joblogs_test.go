// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
)

// keepingServer returns a server on cache and launcher with keep_job_logs
// set to keep, its client, and its log, at info, which the test reads.
func keepingServer(t *testing.T, launcher *fakeLauncher, keep bool) (*Server, pluginv1alpha.BackendPluginClient, *logBuffer) {
	t.Helper()
	srv := newTestServer(t, &fakeCache{}, launcher)
	srv.cfg.KeepJobLogs = keep
	log := &logBuffer{}
	srv.log = slog.New(slog.NewTextHandler(log, nil))
	return srv, pluginv1alpha.NewBackendPluginClient(serve(t, srv)), log
}

// The job logs of a started job, by their path in its state directory, and
// what the test writes in each.
var jobLogFiles = map[string]string{
	"job.log":   "the job process log\n",
	"0/vm.log":  "the VM log\n",
	"0/console": "the console\n",
}

// startWithLogs creates and starts a job on c, then writes each of
// jobLogFiles' contents into its job log, and returns its id.
func startWithLogs(t *testing.T, srv *Server, c pluginv1alpha.BackendPluginClient) string {
	t.Helper()
	id := create(t, c)
	if _, err := start(testContext(t), c, id); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"job.log", "0/vm.log", "0/console"} {
		if err := os.WriteFile(filepath.Join(srv.jobStateDir(id), rel), []byte(jobLogFiles[rel]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// tree returns every path under dir, relative to it, sorted, with a
// directory's ending in /; a missing dir has none.
func tree(t *testing.T, dir string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if d.IsDir() {
			rel += "/"
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	slices.Sort(paths)
	return paths
}

// wantKept fails t unless <state>/logs/<id>/ holds exactly the job's job
// logs, in their layout and with what was written in them, and nothing else
// of the job is left in its state or runtime directory.
func wantKept(t *testing.T, srv *Server, id string) {
	t.Helper()
	kept := filepath.Join(srv.cfg.Dirs.State, "logs", id)
	if got, want := tree(t, kept), []string{"0/", "0/console", "0/vm.log", "job.log"}; !slices.Equal(got, want) {
		t.Fatalf("%s holds %q, want %q", kept, got, want)
	}
	for _, rel := range []string{"job.log", "0/vm.log", "0/console"} {
		b, err := os.ReadFile(filepath.Join(kept, rel))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != jobLogFiles[rel] {
			t.Errorf("the kept %s holds %q, want %q", rel, b, jobLogFiles[rel])
		}
	}
	wantJobGone(t, srv, id)
}

// wantJobGone fails t if anything of the job id is left in its state or
// runtime directory.
func wantJobGone(t *testing.T, srv *Server, id string) {
	t.Helper()
	for _, dir := range []string{srv.jobStateDir(id), filepath.Join(srv.cfg.Dirs.Runtime, id)} {
		if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is left: %v", dir, err)
		}
	}
}

// With keep_job_logs, Remove moves the job's logs to <state>/logs/<id>/,
// keeping their layout, deletes the rest of its files, and names the
// directory in its line. With the built-in value, it deletes them, and its
// line names none.
func TestRemoveKeepsJobLogs(t *testing.T) {
	t.Parallel()
	t.Run("kept", func(t *testing.T) {
		t.Parallel()
		srv, c, log := keepingServer(t, &fakeLauncher{}, true)
		id := startWithLogs(t, srv, c)
		if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
			t.Fatal(err)
		}
		wantKept(t, srv, id)
		kept := filepath.Join(srv.cfg.Dirs.State, "logs", id)
		wantLine(t, log.String(), "removed", `^time=\S+ level=INFO `, regexp.QuoteMeta("env="+id+" "), ` err=<nil> `,
			regexp.QuoteMeta(" kept_logs="+kept+"\n"))
		if strings.Contains(log.String(), "level=WARN") {
			t.Errorf("a warning:\n%s", log)
		}
	})
	t.Run("built-in", func(t *testing.T) {
		t.Parallel()
		srv, c, log := keepingServer(t, &fakeLauncher{}, false)
		id := startWithLogs(t, srv, c)
		if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
			t.Fatal(err)
		}
		wantJobGone(t, srv, id)
		if got := tree(t, filepath.Join(srv.cfg.Dirs.State, "logs")); len(got) != 0 {
			t.Errorf("<state>/logs/ holds %q, want nothing", got)
		}
		if line := wantLine(t, log.String(), "removed"); strings.Contains(line, "kept_logs") {
			t.Errorf("the Remove line names kept logs: %s", line)
		}
	})
}

// With keep_job_logs, a new environment id never names an earlier job's
// kept logs, which outlive the restarts the job table doesn't: Create skips
// an id whose <state>/logs/<id> is there, and that job's logs are left as
// they were by the new job's Remove, which keeps its own. With the
// built-in, kept logs don't count, and the id is taken.
func TestNewIDSkipsKeptLogs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		keep bool
		want string
	}{
		{"kept", true, "job-fresh"},
		{"built-in", false, "job-kept"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, c, _ := keepingServer(t, &fakeLauncher{}, tc.keep)
			ids := []string{"job-kept", "job-fresh"}
			srv.newID = func() (string, error) {
				id := ids[0]
				ids = ids[1:]
				return id, nil
			}
			earlier := filepath.Join(srv.cfg.Dirs.State, "logs", "job-kept", "job.log")
			writeTestFile(t, earlier)
			id := startWithLogs(t, srv, c)
			if id != tc.want {
				t.Fatalf("Create's id is %q, want %q", id, tc.want)
			}
			if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
				t.Fatal(err)
			}
			if tc.keep {
				wantKept(t, srv, id)
			}
			if got, want := tree(t, filepath.Join(srv.cfg.Dirs.State, "logs", "job-kept")), []string{"job.log"}; !slices.Equal(got, want) {
				t.Errorf("the earlier job's kept logs hold %q, want %q", got, want)
			}
		})
	}
}

// A job with no job logs, one never started, keeps nothing: no directory is
// made, and the Remove line names none.
func TestRemoveOfAJobWithNoLogsKeepsNothing(t *testing.T) {
	t.Parallel()
	srv, c, log := keepingServer(t, &fakeLauncher{}, true)
	id := create(t, c)
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(srv.cfg.Dirs.State, "logs")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("<state>/logs/ was made: %v", err)
	}
	if line := wantLine(t, log.String(), "removed"); strings.Contains(line, "kept_logs") {
		t.Errorf("the Remove line names kept logs: %s", line)
	}
}

// A failed Start's job keeps what it logged: here its job process log
// alone, since the job process failed before its VM began.
func TestRemoveKeepsAFailedStartsLogs(t *testing.T) {
	t.Parallel()
	srv, c, _ := keepingServer(t, &fakeLauncher{does: networkFails}, true)
	id := create(t, c)
	if _, err := start(testContext(t), c, id); err == nil {
		t.Fatal("Start succeeded")
	}
	// The fake job process writes nothing; the real one logs why.
	if err := os.WriteFile(filepath.Join(srv.jobStateDir(id), "job.log"), []byte("network failed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(srv.cfg.Dirs.State, "logs", id)
	if got, want := tree(t, kept), []string{"job.log"}; !slices.Equal(got, want) {
		t.Errorf("%s holds %q, want %q", kept, got, want)
	}
	wantJobGone(t, srv, id)
}

// Logs are kept whatever removed the job: the backstop, and Close, which a
// drain's second signal removes every job through.
func TestLogsAreKeptWhateverRemovedTheJob(t *testing.T) {
	t.Parallel()
	t.Run("backstop", func(t *testing.T) {
		t.Parallel()
		srv, c, log := keepingServer(t, &fakeLauncher{}, true)
		id := startWithLogs(t, srv, c)
		srv.removeForgotten(context.Background(), jobOf(t, srv, id), time.Hour)
		wantKept(t, srv, id)
		wantLine(t, log.String(), "removed", regexp.QuoteMeta(" kept_logs="+filepath.Join(srv.cfg.Dirs.State, "logs", id)+"\n"))
	})
	t.Run("close", func(t *testing.T) {
		t.Parallel()
		srv, c, log := keepingServer(t, &fakeLauncher{}, true)
		id := startWithLogs(t, srv, c)
		if err := srv.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		wantKept(t, srv, id)
		wantLine(t, log.String(), "removed", regexp.QuoteMeta(" kept_logs="+filepath.Join(srv.cfg.Dirs.State, "logs", id)+"\n"))
	})
}

// A move that fails is a warning naming the job, and Remove succeeds and
// deletes the job's logs with its files: whatever stops the move, keeping
// nothing of it, and never touching what was at <state>/logs/<id> before.
func TestFailedKeepWarnsAndRemoveSucceeds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// block makes the move of the job id's logs fail, and returns
		// what must be left under <state>/logs/ after it.
		block func(t *testing.T, srv *Server, id string) []string
	}{
		{"logs is a file", func(t *testing.T, srv *Server, _ string) []string {
			t.Helper()
			writeTestFile(t, filepath.Join(srv.cfg.Dirs.State, "logs"))
			return []string{}
		}},
		{"the job's directory is already there", func(t *testing.T, srv *Server, id string) []string {
			t.Helper()
			writeTestFile(t, filepath.Join(srv.cfg.Dirs.State, "logs", id))
			return []string{id}
		}},
		{"an empty directory is already there", func(t *testing.T, srv *Server, id string) []string {
			t.Helper()
			// A plain rename would replace it.
			if err := os.MkdirAll(filepath.Join(srv.cfg.Dirs.State, "logs", id), 0o700); err != nil {
				t.Fatal(err)
			}
			return []string{id + "/"}
		}},
		{"a VM's log can't be moved", func(t *testing.T, srv *Server, id string) []string {
			t.Helper()
			if os.Geteuid() == 0 {
				t.Skip("root renames out of a read-only directory")
			}
			// The job process log moves first; its VM's can't leave
			// a directory that isn't writable.
			vm := filepath.Join(srv.jobStateDir(id), "0")
			if err := os.Chmod(vm, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(vm, 0o700) })
			return []string{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, c, log := keepingServer(t, &fakeLauncher{}, true)
			id := startWithLogs(t, srv, c)
			want := tc.block(t, srv, id)
			if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
				t.Fatalf("Remove: %v", err)
			}
			wantJobGone(t, srv, id)
			if got := tree(t, filepath.Join(srv.cfg.Dirs.State, "logs")); !slices.Equal(got, want) {
				t.Errorf("<state>/logs/ holds %q, want %q", got, want)
			}
			wantLine(t, log.String(), "could not keep the job's logs: deleting them", `^time=\S+ level=WARN `,
				regexp.QuoteMeta("env="+id+" "), ` err=.+`)
			if line := wantLine(t, log.String(), "removed", `^time=\S+ level=INFO `); strings.Contains(line, "kept_logs") {
				t.Errorf("the Remove line names kept logs: %s", line)
			}
		})
	}
}

// writeTestFile writes an empty regular file at path, making its directory.
func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A crash at any point of keeping a job's logs leaves <state>/logs/ holding
// all of them or nothing of the job, and what it gathered so far is under
// <state>/jobs/, where startup cleanup deletes it. Each crash is a rename
// that fails, after which keepJobLogs does nothing more, as a killed plugin
// wouldn't.
func TestKeepJobLogsCrashLeavesAllOrNothing(t *testing.T) {
	t.Parallel()
	// Three job logs, then the gathered directory: four renames.
	for crashAt := range 5 {
		t.Run(strconv.Itoa(crashAt), func(t *testing.T) {
			t.Parallel()
			state, runtime := t.TempDir(), t.TempDir()
			const id = "crashed"
			for rel, content := range jobLogFiles {
				path := filepath.Join(state, "jobs", id, rel)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			n := 0
			_, err := keepJobLogs(state, id, func(oldpath, newpath string) error {
				if n++; n > crashAt {
					return errors.New("crashed")
				}
				return renameNoReplace(oldpath, newpath)
			})
			if crashed := crashAt < 4; crashed != (err != nil) {
				t.Fatalf("crashed after %d renames: err = %v", crashAt, err)
			}
			want := []string{}
			if crashAt == 4 {
				want = []string{id + "/", id + "/0/", id + "/0/console", id + "/0/vm.log", id + "/job.log"}
			}
			logs := filepath.Join(state, "logs")
			if got := tree(t, logs); !slices.Equal(got, want) {
				t.Fatalf("after %d renames <state>/logs/ holds %q, want %q", crashAt, got, want)
			}
			dirs := Dirs{State: state, Runtime: runtime, Listen: filepath.Join(runtime, "listen.sock")}
			if err := cleanStartup(discardLog(), dirs); err != nil {
				t.Fatal(err)
			}
			if got := tree(t, filepath.Join(state, "jobs")); len(got) != 0 {
				t.Errorf("after startup cleanup <state>/jobs/ holds %q", got)
			}
			if got := tree(t, logs); !slices.Equal(got, want) {
				t.Errorf("after startup cleanup <state>/logs/ holds %q, want %q", got, want)
			}
		})
	}
}

// A <state>/logs on another filesystem than the state directory can't take
// a rename, so keeping fails with an error saying so, and nothing is kept
// there.
func TestKeepJobLogsOnAnotherFilesystemFails(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	other, err := os.MkdirTemp("/dev/shm", "keep-job-logs-")
	if err != nil {
		t.Skipf("no /dev/shm: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(other) })
	var a, b unix.Stat_t
	if err := unix.Stat(state, &a); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(other, &b); err != nil {
		t.Fatal(err)
	}
	if a.Dev == b.Dev {
		t.Skip("the test's directory and /dev/shm are on one filesystem")
	}
	if err := os.Symlink(other, filepath.Join(state, "logs")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(state, "jobs", "env", "job.log"))
	_, err = keepJobLogs(state, "env", renameNoReplace)
	if !errors.Is(err, unix.EXDEV) || !strings.Contains(err.Error(), "on another filesystem than the state directory") {
		t.Errorf("err = %v, want EXDEV saying <state>/logs is on another filesystem", err)
	}
	if got := tree(t, other); len(got) != 0 {
		t.Errorf("%s holds %q", other, got)
	}
}
