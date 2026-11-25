// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	pluginv1alpha "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
)

// TestExecEnvStaysSecret is LOG-2: an Exec env value appears in no readable
// host process's command line or environment while the Exec runs, and after
// Remove in neither the plugin's stderr nor any file under its state, cache
// and runtime directories, with logging at debug, libkrun's at trace, and
// every job log kept.
// A second Exec echoing the value shows it reaches the job's output, and
// the scan reads the plugin's and its descendants' command lines and
// environments, so finding nothing proves something.
func TestExecEnvStaysSecret(t *testing.T) {
	t.Parallel()
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	value := hex.EncodeToString(b)
	env := map[string]string{"CANARY": value}
	p := startPluginSetup(t, buildPlugin(t), pluginSetup{
		settings: "lib_dir: " + devLibDir(t) + "\nlog_level: debug\nlibkrun_log_level: trace\nkeep_job_logs: true\n",
	})
	v := bootVM(t, p, "secret-env")

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	type result struct {
		code int32
		out  string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, out, err := v.runRequest(ctx, &pluginv1alpha.ExecRequest{Command: []string{"sleep", "2"}, Env: env, Workdir: "/"})
		done <- result{code, out, err}
	}()
	// Count only the scans that ended while the Exec still ran. Each file
	// holding the value is reported once, however many scans find it, and
	// after the first scan that missed a process the rest aren't checked.
	scans := 0
	var r result
	leaked := map[string]bool{}
	readFailed := false
	for running := true; running; {
		found, read := scanProcs([]byte(value))
		for _, f := range found {
			if !leaked[f] {
				leaked[f] = true
				t.Errorf("the value is in %s while the Exec runs", f)
			}
		}
		select {
		case r = <-done:
			running = false
		default:
			scans++
			if !readFailed {
				readFailed = !wantRead(t, p, read)
			}
		}
	}
	if r.err != nil || r.code != 0 {
		t.Fatalf("sleep 2 exited %d, %v: %s", r.code, r.err, r.out)
	}
	if scans == 0 {
		t.Fatal("no scan of /proc ended while the Exec ran")
	}

	// The positive control: the value reaches the job's output.
	code, out, err := v.runRequest(ctx, &pluginv1alpha.ExecRequest{Command: []string{"sh", "-c", `echo "$CANARY"`}, Env: env, Workdir: "/"})
	if err != nil || code != 0 || !strings.Contains(out, value) {
		t.Fatalf("echo of the value exited %d, %v, printing %q; want the value", code, err, out)
	}
	v.remove(t)

	if !p.logMatches(regexp.MustCompile(regexp.QuoteMeta(`msg=removed program=runner-microvm env=` + v.id + ` `))) {
		t.Fatalf("the plugin's log holds no Remove line:\n%s", p.log.String())
	}
	if strings.Contains(p.log.String(), value) {
		t.Errorf("the plugin's stderr holds the value:\n%s", p.log.String())
	}
	kept := map[string]bool{}
	for _, dir := range []string{"state", "cache", "runtime"} {
		root := filepath.Join(p.base, dir)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			if rel, err := filepath.Rel(filepath.Join(p.state, "logs", v.id), path); err == nil && !strings.HasPrefix(rel, "..") {
				kept[rel] = true
			}
			has, err := fileContains(path, []byte(value))
			if err != nil {
				return err
			}
			if has {
				t.Errorf("%s holds the value", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// The kept logs were among the files searched.
	for _, rel := range []string{"job.log", "0/vm.log", "0/console"} {
		if !kept[rel] {
			t.Errorf("the kept %s was not searched: kept %v", rel, kept)
		}
	}
}

// scanProcs reads the command line and environment of every process in
// /proc it can, and returns those holding value, and the pids whose both it
// read. A process that ends or can't be read is passed over.
func scanProcs(value []byte) (found []string, read map[int]bool) {
	read = map[int]bool{}
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		both := true
		for _, f := range []string{"cmdline", "environ"} {
			path := "/proc/" + e.Name() + "/" + f
			b, err := os.ReadFile(path)
			if err != nil {
				both = false
				continue
			}
			if bytes.Contains(b, value) {
				found = append(found, path)
			}
		}
		read[pid] = both
	}
	return found, read
}

// wantRead fails t, and returns false, unless a scan read the command line
// and environment of p's plugin and of each of its descendants, the job
// process and VM child among them.
func wantRead(t *testing.T, p *plugin, read map[int]bool) (ok bool) {
	t.Helper()
	ok = true
	pids := []int{p.pid}
	comms := []string{}
	for _, d := range descendants(p.pid) {
		pids = append(pids, d.pid)
		comms = append(comms, d.comm)
	}
	if len(pids) < 3 {
		t.Errorf("the plugin has %d descendants (%v), want the job process and its VM child", len(pids)-1, comms)
		ok = false
	}
	for _, pid := range pids {
		if !read[pid] {
			t.Errorf("the scan did not read pid %d's command line and environment", pid)
			ok = false
		}
	}
	return ok
}

// fileContains reports whether the file at path holds value, reading it a
// MiB at a time, so a large cached layer is never read whole.
func fileContains(path string, value []byte) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 0, 1<<20+len(value))
	for {
		n, err := io.ReadFull(f, buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if bytes.Contains(buf, value) {
			return true, nil
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		// Keep the tail, so a value across two blocks is found.
		tail := len(value) - 1
		buf = buf[:copy(buf, buf[len(buf)-tail:])]
	}
}
