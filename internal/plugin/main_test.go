// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"bytes"
	"errors"
	"log/slog"
	"math"
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

// run opens the image cache with cache_max_gib in bytes as its cap: left
// out, the built-in 20 GiB; 0, a cap of 0; the largest value, the largest
// whole GiB an int64 of bytes holds (IMG-14). A failure to open it stops
// startup with that error, and releases the directory locks.
//
// It is not parallel: it sets PATH and RUNNER_MICROVM_LIBDIR.
func TestRunPassesCacheMaxGiBToTheImageCache(t *testing.T) {
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "mkfs.ext4"), nil, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools)
	t.Setenv(LibDirEnv, "")
	errOpen := errors.New("the fake image cache does not open")
	for content, want := range map[string]int64{
		"":                            20 << 30,
		"cache_max_gib: 0\n":          0,
		"cache_max_gib: 3\n":          3 << 30,
		"cache_max_gib: 8589934591\n": math.MaxInt64 >> 30 << 30,
	} {
		dirs := testDirs(filepath.Join(t.TempDir(), "plugin"))
		file := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(file, []byte("state_dir: "+dirs.State+"\ncache_dir: "+dirs.Cache+
			"\nruntime_dir: "+dirs.Runtime+"\nlisten: unix://"+dirs.Listen+"\n"+content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(func(k string) string {
			if k == config.PathEnv {
				return file
			}
			return ""
		})
		if err != nil {
			t.Fatal(err)
		}
		var calls []int64
		err = run(t.Context(), discardLog(), cfg, nil, func(_ *slog.Logger, dir string, maxSize int64) (ImageCache, error) {
			if dir != dirs.Cache {
				t.Errorf("%q: the image cache opened in %s, want %s", content, dir, dirs.Cache)
			}
			calls = append(calls, maxSize)
			return nil, errOpen
		})
		if !errors.Is(err, errOpen) {
			t.Errorf("%q: run returned %v, want the image cache's error", content, err)
		}
		if len(calls) != 1 || calls[0] != want {
			t.Errorf("%q: the image cache opened with caps %v, want one of %d bytes", content, calls, want)
		}
		_ = mustLock(t, dirs)
	}
}
