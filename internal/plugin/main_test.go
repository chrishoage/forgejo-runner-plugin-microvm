// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/config"
)

// runMain runs Main with RUNNER_MICROVM_CONFIG set to file, and returns its
// exit code, stdout and stderr.
func runMain(file string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Main(args, &stdout, &stderr, func(k string) string {
		if k == config.PathEnv {
			return file
		}
		return ""
	})
	return code, stdout.String(), stderr.String()
}

// check exits 0 on a valid file, saying so, and 1 on an invalid one,
// printing every error.
func TestCheck(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.yaml")
	invalid := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(valid, []byte("state_dir: "+dir+"/state\nprofiles:\n  default: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalid, []byte("state_dir: rel\nbogus: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runMain(valid, "check")
	if code != 0 || stdout != "config "+valid+": valid\n" || stderr != "" {
		t.Errorf("valid: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = runMain(invalid, "check")
	want := "config " + invalid + ": line 1: state_dir: must be an absolute path, not \"rel\"\n" +
		"config " + invalid + ": line 2: bogus: unknown key\n"
	if code != 1 || stdout != "" || stderr != want {
		t.Errorf("invalid: exit %d, stdout %q, stderr %q; want 1 and %q", code, stdout, stderr, want)
	}
	code, _, stderr = runMain(filepath.Join(dir, "missing.yaml"), "check")
	if code != 1 || !strings.Contains(stderr, "missing.yaml") {
		t.Errorf("missing: exit %d, stderr %q", code, stderr)
	}
}

// No subcommand, an unknown one, or an argument prints the usage and exits
// 2, before reading any config file.
func TestUsage(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	for _, args := range [][]string{nil, {"run"}, {"check", "extra"}, {"serve", "/etc/x.yaml"}, {"--help"}} {
		code, stdout, stderr := runMain(missing, args...)
		if code != 2 || stdout != "" || !strings.HasPrefix(stderr, "usage: runner-microvm <command>") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
}

// serve stops at once on an invalid config file, before writing anything.
func TestServeStopsOnAnInvalidConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	state := filepath.Join(dir, "state")
	if err := os.WriteFile(file, []byte("state_dir: "+state+"\ncache_dir: "+state+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runMain(file, "serve"); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if _, err := os.Lstat(state); !os.IsNotExist(err) {
		t.Errorf("serve wrote %s: %v", state, err)
	}
}
