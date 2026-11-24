// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// wantLibsLoaded fails the test unless p's startup log names dir's
// libkrun.so.1 and libkrunfw.so.5 as the files the job binary's libs
// loaded.
func wantLibsLoaded(t *testing.T, p *plugin, dir string) {
	t.Helper()
	line := regexp.MustCompile(`msg="the job binary loaded libkrun and libkrunfw" [^\n]* libkrun=` +
		regexp.QuoteMeta(filepath.Join(dir, "libkrun.so.1")) + ` libkrunfw=` + regexp.QuoteMeta(filepath.Join(dir, "libkrunfw.so.5")) + ` `)
	if !line.MatchString(p.log.String()) {
		t.Errorf("the startup log does not name %s's libkrun.so.1 and libkrunfw.so.5:\n%s", dir, p.log.String())
	}
}

// With lib_dir naming a directory without the libraries, startup fails
// naming the missing library and the directory, before the listen socket
// exists.
func TestEmptyLibDirStopsStartup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := spawnPlugin(t, buildPlugin(t), pluginSetup{settings: "lib_dir: " + dir + "\n"}, false)
	select {
	case <-p.exited:
	case <-time.After(time.Minute):
		t.Fatalf("the plugin did not stop:\n%s", p.log.String())
	}
	log := p.log.String()
	if p.exitStatus(t) == 0 {
		t.Errorf("the plugin exited 0:\n%s", log)
	}
	if want := "libkrunfw.so.5 in lib_dir " + dir + ": "; !strings.Contains(log, want) || !strings.Contains(log, `msg=stopped`) {
		t.Errorf("the log does not stop startup with %q:\n%s", want, log)
	}
	if strings.Contains(log, "msg=serving") {
		t.Errorf("the plugin served:\n%s", log)
	}
	if _, err := os.Lstat(p.sock); !os.IsNotExist(err) {
		t.Errorf("the listen socket %s exists: %v", p.sock, err)
	}
}

// With lib_dir unset, the libraries load by soname through the system's
// dynamic loader, here pointed at the dev shell's directory: a job starts,
// and the startup log names the files the loader picked.
func TestLibsBySoname(t *testing.T) {
	t.Parallel()
	dir := devLibDir(t)
	p := startPluginSetup(t, buildPlugin(t), pluginSetup{env: []string{"LD_LIBRARY_PATH=" + dir}})
	wantLibsLoaded(t, p, dir)
	if !strings.Contains(p.log.String(), `lib_dir="unset (by soname)"`) {
		t.Errorf("the startup log does not say lib_dir is unset:\n%s", p.log.String())
	}
	bootVM(t, p, "soname")
}

// libkrunDebug matches a libkrun debug line in a VM log: libkrun's logger
// writes the level after the time.
var libkrunDebug = regexp.MustCompile(`(?m)^\[\S+ DEBUG `)

// With libkrun_log_level: debug, a VM's log holds libkrun's debug lines;
// with the built-in, warn, none.
func TestLibkrunLogLevel(t *testing.T) {
	t.Parallel()
	bin := buildPlugin(t)
	for _, tc := range []struct {
		name, settings string
		debug          bool
	}{
		{"builtin", "", false},
		{"debug", "libkrun_log_level: debug\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := startPluginSetup(t, bin, pluginSetup{settings: "lib_dir: " + devLibDir(t) + "\n" + tc.settings})
			v := bootVM(t, p, "krunlog")
			b, err := os.ReadFile(filepath.Join(p.state, "jobs", v.id, "0", "vm.log"))
			if err != nil {
				t.Fatal(err)
			}
			if got := libkrunDebug.Match(b); got != tc.debug {
				t.Errorf("libkrun debug lines in the VM log: %t, want %t; its start:\n%s", got, tc.debug, b[:min(len(b), 4<<10)])
			}
		})
	}
}
