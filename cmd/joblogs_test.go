// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// A job's job logs exist on the host while it runs; with the built-in
// keep_job_logs, Remove leaves none of them anywhere under the state
// directory; with keep_job_logs: true, it moves them to <state>/logs/<id>/,
// the job process log at the top and its VM's log and console under 0/, and
// leaves nothing else of the job (LOG-3). The kept ones are the job's own
// files, each still holding what it held before Remove.
func TestJobLogs(t *testing.T) {
	t.Parallel()
	bin := buildPlugin(t)
	for _, tc := range []struct {
		name string
		keep bool
	}{{"built-in", false}, {"kept", true}} {
		t.Run(tc.name, func(t *testing.T) {
			settings := "lib_dir: " + devLibDir(t) + "\n"
			if tc.keep {
				// At debug each of the three programs writes its own
				// file before the job is used: at info the VM log holds
				// only what libkrun warns of, which is nothing on most
				// boots.
				settings += "keep_job_logs: true\nlog_level: debug\n"
			}
			p := startPluginSetup(t, bin, pluginSetup{settings: settings})
			v := bootVM(t, p, "job-logs-"+tc.name)
			if code, out := v.exec(t, "true"); code != 0 {
				t.Fatalf("true exited %d: %s", code, out)
			}
			jobDir := filepath.Join(p.state, "jobs", v.id)
			held := map[string][]byte{}
			for _, rel := range []string{"job.log", "0/vm.log", "0/console"} {
				path := filepath.Join(jobDir, rel)
				if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
					t.Errorf("while the job runs, its %s is not a file: %v", rel, err)
					continue
				}
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				held[rel] = b
			}
			v.remove(t)

			logs := filepath.Join(p.state, "logs", v.id)
			var want []string
			if tc.keep {
				want = []string{"0/", "0/console", "0/vm.log", "job.log"}
				removed := regexp.MustCompile(`(?m)msg=removed program=runner-microvm env=` + regexp.QuoteMeta(v.id) + ` .* kept_logs=` + regexp.QuoteMeta(logs) + `$`)
				if !p.logMatches(removed) {
					t.Errorf("the Remove line does not name %s:\n%s", logs, p.log.String())
				}
			}
			if got := walk(t, logs); !slices.Equal(got, want) {
				t.Errorf("%s holds %q, want %q", logs, got, want)
			}
			if tc.keep {
				// Each program goes on writing its file until Remove
				// has reaped it, so the kept file is what the job's held
				// before Remove and then perhaps more.
				for rel, before := range held {
					if len(before) == 0 {
						t.Errorf("before Remove, the job's %s is empty", rel)
					}
					kept, err := os.ReadFile(filepath.Join(logs, rel))
					if err == nil && !bytes.HasPrefix(kept, before) {
						t.Errorf("the kept %s does not start with what the job's held before Remove:\n%s\nkept:\n%s", rel, before, kept)
					}
				}
			}
			// Nothing else of the job is left, under the state directory
			// or anywhere else the plugin keeps it.
			_ = filepath.WalkDir(p.state, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					t.Error(err)
					return nil
				}
				if path == logs || strings.HasPrefix(path, logs+"/") {
					return nil
				}
				switch d.Name() {
				case v.id, "job.log", "vm.log", "console":
					t.Errorf("%s is left after Remove", path)
				}
				return nil
			})
			for _, dir := range []string{
				jobDir, filepath.Join(p.base, "runtime", v.id), filepath.Join(p.base, "cache", "jobs", v.id),
			} {
				if _, err := os.Lstat(dir); !os.IsNotExist(err) {
					t.Errorf("%s is left after Remove: %v", dir, err)
				}
			}
			wantSetting := "keep_job_logs=false"
			if tc.keep {
				wantSetting = "keep_job_logs=true"
			}
			if !strings.Contains(p.log.String(), "msg=settings ") || !strings.Contains(p.log.String(), " "+wantSetting) {
				t.Errorf("the settings line does not say %s:\n%s", wantSetting, p.log.String())
			}
		})
	}
}

// walk returns every path under dir, relative to it, sorted, with a
// directory's ending in /; a missing dir has none.
func walk(t *testing.T, dir string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if d.IsDir() {
			rel += "/"
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	slices.Sort(paths)
	return paths
}
