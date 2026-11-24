// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// log_level reaches every program: with debug, the job process log, the VM
// log and the console each hold a debug line of their program's, and the
// plugin's log an Exec's; with warn, the plugin logs no info line for a
// job that succeeds, from its start to its Remove.
func TestLogLevel(t *testing.T) {
	t.Parallel()
	bin := buildPlugin(t)
	t.Run("debug", func(t *testing.T) {
		p := startPluginSetup(t, bin, pluginSetup{settings: "lib_dir: " + devLibDir(t) + "\nlog_level: debug\n"})
		v := bootVM(t, p, "debug-log")
		if code, out := v.exec(t, "true"); code != 0 {
			t.Fatalf("true exited %d: %s", code, out)
		}
		// Read while the job runs: Remove deletes its logs (LOG-3).
		dir := filepath.Join(p.state, "jobs", v.id)
		for _, f := range []struct{ what, path, program string }{
			{"the job process log", filepath.Join(dir, "job.log"), "runner-microvm-job"},
			{"the VM log", filepath.Join(dir, "0", "vm.log"), "runner-microvm-vm"},
			{"the console", filepath.Join(dir, "0", "console"), "runner-microvm-agent"},
		} {
			b, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatal(err)
			}
			line := regexp.MustCompile(`(?m)^time=\S+ level=DEBUG msg=.* program=` + f.program + `\b`)
			if !line.Match(b) {
				t.Errorf("%s holds no debug line of %s:\n%s", f.what, f.program, b[:min(len(b), 8<<10)])
			}
		}
		exec := regexp.MustCompile(`level=DEBUG msg=exec program=runner-microvm env=` + regexp.QuoteMeta(v.id) + ` argv0=true exit_code=0 `)
		if !p.logMatches(exec) {
			t.Errorf("the plugin's log holds no debug line for the Exec:\n%s", p.log.String())
		}
	})
	t.Run("warn", func(t *testing.T) {
		p := startPluginSetup(t, bin, pluginSetup{settings: "lib_dir: " + devLibDir(t) + "\nlog_level: warn\n"})
		v := bootVM(t, p, "warn-log")
		if code, out := v.exec(t, "true"); code != 0 {
			t.Fatalf("true exited %d: %s", code, out)
		}
		v.remove(t)
		if log := p.log.String(); strings.Contains(log, "level=INFO") || strings.Contains(log, "level=DEBUG") {
			t.Errorf("the plugin logged below warn:\n%s", log)
		}
	})
}
