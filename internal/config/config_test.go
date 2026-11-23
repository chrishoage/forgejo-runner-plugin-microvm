// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// wantBuiltin is the config every setting's built-in value makes.
func wantBuiltin(file string) *Config {
	return &Config{
		File:       file,
		Listen:     "/run/runner-microvm/runner-microvm.sock",
		StateDir:   "/var/lib/runner-microvm",
		CacheDir:   "/var/cache/runner-microvm",
		RuntimeDir: "/run/runner-microvm/jobs",
		Profiles:   Profiles{},
	}
}

// writeConfig writes content to a fresh config file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// mustLoad loads content as a named config file and fails the test on any
// error.
func mustLoad(t *testing.T, content string) *Config {
	t.Helper()
	cfg, err := load(writeConfig(t, content), true)
	if err != nil {
		t.Fatalf("load:\n%s\nerror: %v", content, err)
	}
	return cfg
}

// loadErr loads content as a named config file, which must fail, and
// returns the file's path and the error's text.
func loadErr(t *testing.T, content string) (string, string) {
	t.Helper()
	path := writeConfig(t, content)
	cfg, err := load(path, true)
	if err == nil {
		t.Fatalf("load:\n%s\ngave %+v, want an error", content, cfg)
	}
	return path, err.Error()
}

// wantErr fails the test unless msg holds want, the part of an error after
// "config <path>: ".
func wantErr(t *testing.T, path, msg, want string) {
	t.Helper()
	if full := fmt.Sprintf("config %s: %s", path, want); !strings.Contains(msg, full) {
		t.Errorf("error:\n%s\nwant it to contain:\n%s", msg, full)
	}
}

// No file at the default path, an empty file, and a file with only comments
// or a null document each give every built-in value (CFG-1).
func TestNoFileOrEmptyFileGivesBuiltinValues(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := load(missing, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := wantBuiltin(""); !reflect.DeepEqual(cfg, want) {
		t.Errorf("no file: %+v, want %+v", cfg, want)
	}
	for _, content := range []string{"", "\n", "# nothing set\n", "~\n", "---\n"} {
		path := writeConfig(t, content)
		cfg, err := load(path, true)
		if err != nil {
			t.Errorf("%q: %v", content, err)
			continue
		}
		if want := wantBuiltin(path); !reflect.DeepEqual(cfg, want) {
			t.Errorf("%q: %+v, want %+v", content, cfg, want)
		}
	}
}

// A missing file that RUNNER_MICROVM_CONFIG names, and a file that can't be
// read wherever it is, stop startup with an error naming the path.
func TestMissingNamedOrUnreadableFileFails(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "config.yaml")
	// A directory can't be read as a file, even by root.
	unreadable := t.TempDir()
	for _, tc := range []struct {
		name  string
		path  string
		named bool
	}{
		{"named and missing", missing, true},
		{"named and unreadable", unreadable, true},
		{"unreadable at the default path", unreadable, false},
	} {
		cfg, err := load(tc.path, tc.named)
		if err == nil {
			t.Errorf("%s: %+v, want an error", tc.name, cfg)
			continue
		}
		if !strings.HasPrefix(err.Error(), "config "+tc.path+": ") {
			t.Errorf("%s: error %q does not name %s", tc.name, err, tc.path)
		}
	}
}

// Load reads the file RUNNER_MICROVM_CONFIG names, made absolute once.
//
// It is not parallel: it changes the working directory.
func TestLoadMakesTheNamedPathAbsolute(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rm.yaml"), []byte("max_memory_mib: 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	cfg, err := Load(func(k string) string {
		if k == PathEnv {
			return "rm.yaml"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "rm.yaml"); cfg.File != want || cfg.MaxMemoryMiB != 7 {
		t.Errorf("File = %q, MaxMemoryMiB = %d; want %q and 7", cfg.File, cfg.MaxMemoryMiB, want)
	}
}

// Every plugin-level key is read, and paths are cleaned.
func TestPluginLevelKeys(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	cfg := mustLoad(t, fmt.Sprintf(`
listen: unix://%[1]s/listen/rm.sock
state_dir: %[1]s/state/
cache_dir: %[1]s/x/../cache
runtime_dir: %[1]s/runtime
max_memory_mib: 16384
`, base))
	want := &Config{
		File:         cfg.File,
		Listen:       base + "/listen/rm.sock",
		StateDir:     base + "/state",
		CacheDir:     base + "/cache",
		RuntimeDir:   base + "/runtime",
		MaxMemoryMiB: 16384,
		Profiles:     Profiles{},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got %+v, want %+v", cfg, want)
	}
}

// An unknown key, at the top level or in a profile, fails naming its line,
// the key and, inside profiles, the profile (CFG-2), wherever it is
// written, after profiles: included.
func TestUnknownKeysFail(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, content, want string
	}{
		{"top level", "max_memory_mib: 1\ncache: /x\n", `line 2: cache: unknown key`},
		{"after profiles", "profiles:\n  default: {}\n  big: {}\nlisten_dir: /x\n", `line 4: listen_dir: unknown key`},
		{"compose-style x- key", "x-base: &b {}\nprofiles:\n  default: *b\n", `line 1: x-base: unknown key`},
		{"in a profile", "profiles:\n  default:\n  big:\n    disk_gib: 8\n", `line 4: profile "big": disk_gib: unknown key`},
		{"merged into a profile", "profiles:\n  a: &a\n    vcpus: 1\n  b:\n    <<: *a\n", `line 3: profile "a": vcpus: unknown key`},
		{"case-sensitive", "profiles:\n  a:\n    CPUs: 1\n", `line 3: profile "a": CPUs: unknown key`},
	} {
		path, msg := loadErr(t, tc.content)
		wantErr(t, path, msg, tc.want)
		if tc.name == "after profiles" && strings.Contains(msg, `profile "big"`) {
			t.Errorf("%s: error %q blames the last profile", tc.name, msg)
		}
	}
	// A key merged into two profiles fails in each, naming each.
	path, msg := loadErr(t, "profiles:\n  a: &a\n    vcpus: 1\n  b:\n    <<: *a\n")
	wantErr(t, path, msg, `line 3: profile "b": vcpus: unknown key`)
}

// Each plugin-level key's invalid values fail naming the line and the key
// (CFG-3).
func TestInvalidPluginLevelValuesFail(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		content, want string
	}{
		{"listen: /run/rm.sock\n", `line 1: listen: must be unix:// followed by an absolute path, not "/run/rm.sock"`},
		{"listen: unix://run/rm.sock\n", `line 1: listen: must be unix:// followed by an absolute path, not "unix://run/rm.sock"`},
		{"listen: tcp://127.0.0.1:1\n", `line 1: listen: must be unix:// followed by an absolute path`},
		{"listen: unix:///\n", `line 1: listen: must be unix:// followed by an absolute path`},
		{"listen: 3\n", `line 1: listen: must be a string`},
		{"state_dir: var/lib/rm\n", `line 1: state_dir: must be an absolute path, not "var/lib/rm"`},
		{"cache_dir: ./cache\n", `line 1: cache_dir: must be an absolute path, not "./cache"`},
		{"runtime_dir: \"\"\n", `line 1: runtime_dir: must be an absolute path, not ""`},
		{"state_dir:\n", `line 1: state_dir: has no value`},
		{"cache_dir: [/a]\n", `line 1: cache_dir: must be a string`},
		{"\n\nmax_memory_mib: -1\n", `line 3: max_memory_mib: must be 0 or more, not -1`},
		{"max_memory_mib: 1.5\n", `line 1: max_memory_mib: must be an integer`},
		{"max_memory_mib: \"1024\"\n", `line 1: max_memory_mib: must be an integer`},
		{"max_memory_mib: 99999999999999999999\n", `line 1: max_memory_mib: must be an integer`},
		{"max_memory_mib:\n", `line 1: max_memory_mib: has no value`},
		{"profiles: [default]\n", `line 1: profiles: must be a mapping of profile names to profiles`},
		{"profiles:\n  big: 8\n", `line 2: profile "big": must be a mapping of profile keys`},
		{"max_memory_mib: 1\nmax_memory_mib: 2\n", `line 2: max_memory_mib: is already set on line 1`},
		{"profiles:\n  a: {}\n  a: {}\n", `line 3: profile "a": is already defined on line 2`},
		{"[a, b]\n", `line 1: must be a mapping of keys`},
		{"listen: unix:///a.sock\n---\nlisten: unix:///b.sock\n", `line 2: a second YAML document: the config file holds one`},
		{"<<: 3\n", `line 1: <<: must be a mapping, an alias of one, or a list of them`},
		{"a: &a\n  <<: *a\n", `line 1: a: unknown key`},
	} {
		path, msg := loadErr(t, tc.content)
		wantErr(t, path, msg, tc.want)
	}
}

// A mapping that merges itself fails, rather than recursing forever.
func TestMergeCycleFails(t *testing.T) {
	t.Parallel()
	path, msg := loadErr(t, "profiles:\n  a: &a\n    <<: *a\n")
	wantErr(t, path, msg, `line 3: profile "a": <<: merges a mapping into itself`)
}

// Merge keys nested to expand exponentially fail, once, and quickly.
func TestMergeExpansionIsBounded(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("x0: &x0 {a: 1, b: 2}\n")
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&b, "x%d: &x%d {<<: [*x%d, *x%d]}\n", i, i, i-1, i-1)
	}
	b.WriteString("profiles:\n  p:\n    <<: *x30\n")
	_, msg := loadErr(t, b.String())
	if !strings.Contains(msg, `: profile "p": <<: merge keys expand to more than 100000 keys`) {
		t.Errorf("error does not say the merges expand too far:\n%s", msg)
	}
	if n := strings.Count(msg, "merge keys expand"); n != 1 {
		t.Errorf("reported %d times, want once", n)
	}
}

// A YAML syntax error stops startup with the library's line.
func TestSyntaxErrorFails(t *testing.T) {
	t.Parallel()
	path, msg := loadErr(t, "listen: unix:///a.sock\nprofiles: [\n")
	if !strings.HasPrefix(msg, "config "+path+": yaml: line ") {
		t.Errorf("error %q does not carry the library's line", msg)
	}
}

// Every error in a file is reported, not only the first.
func TestEveryErrorIsReported(t *testing.T) {
	t.Parallel()
	path, msg := loadErr(t, "state_dir: rel\nbogus: 1\nprofiles:\n  Big: {}\n  ok:\n    cpus: 0\n")
	for _, want := range []string{
		`line 1: state_dir: must be an absolute path`,
		`line 2: bogus: unknown key`,
		`line 4: profile "Big": the name must match [a-z0-9][a-z0-9_-]*`,
		`line 6: profile "ok": cpus: must be from 1 to 255, not 0`,
	} {
		wantErr(t, path, msg, want)
	}
	if n := strings.Count(msg, "\n") + 1; n != 4 {
		t.Errorf("%d errors, want 4:\n%s", n, msg)
	}
}

// A profile name outside [a-z0-9][a-z0-9_-]* fails, naming it; an empty
// profile decodes.
func TestProfileNames(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, "profiles:\n  default:\n  big-2_x: {}\n  '0': {}\n")
	b := BuiltinProfile()
	want := Profiles{"default": b, "big-2_x": b, "0": b}
	if !reflect.DeepEqual(cfg.Profiles, want) {
		t.Errorf("Profiles = %+v, want %+v", cfg.Profiles, want)
	}
	for _, name := range []string{`""`, "Big", "-big", "_big", "big;x", "big=x", `"big x"`, "big.x", "ä"} {
		path, msg := loadErr(t, "profiles:\n  "+name+": {}\n")
		shown := strings.Trim(name, `"`)
		wantErr(t, path, msg, fmt.Sprintf("line 2: profile %q: the name must match [a-z0-9][a-z0-9_-]*", shown))
	}
}

// max_memory_mib unset, 0 and positive are accepted: unset and 0 both mean
// no limit.
func TestMaxMemoryMiB(t *testing.T) {
	t.Parallel()
	for content, want := range map[string]uint64{
		"":                      0,
		"max_memory_mib: 0\n":   0,
		"max_memory_mib: 512\n": 512,
	} {
		if got := mustLoad(t, content).MaxMemoryMiB; got != want {
			t.Errorf("%q: MaxMemoryMiB = %d, want %d", content, got, want)
		}
	}
}

// Values shared between two profiles with an anchor on a profile and a merge
// key decode, with each profile the built-in values plus its own keys.
func TestAnchorsAndMergeKeys(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, `
profiles:
  default: &default {cpus: 4}
  big:
    <<: *default
    memory_mib: 2048
  both:
    <<: [*default, {cpus: 8, scratch_gib: 20}]
  plain: {}
`)
	b := BuiltinProfile()
	want := Profiles{
		"default": {CPUs: 4, MemoryMiB: b.MemoryMiB, ScratchGiB: b.ScratchGiB},
		"big":     {CPUs: 4, MemoryMiB: 2048, ScratchGiB: b.ScratchGiB},
		"both":    {CPUs: 4, MemoryMiB: b.MemoryMiB, ScratchGiB: 20},
		"plain":   b,
	}
	if !reflect.DeepEqual(cfg.Profiles, want) {
		t.Errorf("Profiles = %+v, want %+v", cfg.Profiles, want)
	}
	// A plugin-level value through an alias and a merge key.
	base := t.TempDir()
	cfg = mustLoad(t, fmt.Sprintf("<<: {state_dir: &s %s/state}\ncache_dir: %s/cache\n", base, base))
	if cfg.StateDir != base+"/state" {
		t.Errorf("StateDir = %q, want the merged %s/state", cfg.StateDir, base)
	}
	// An explicit key wins over a merged one.
	cfg = mustLoad(t, "<<: {max_memory_mib: 1}\nmax_memory_mib: 2\n")
	if cfg.MaxMemoryMiB != 2 {
		t.Errorf("MaxMemoryMiB = %d, want the explicit 2", cfg.MaxMemoryMiB)
	}
}

// Each profile is the built-in values plus its own keys, never default's
// (CFG-5), and a profile with no keys is every built-in value.
func TestProfileKeys(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, `
profiles:
  default:
    cpus: 4
    memory_mib: 4096
    scratch_gib: 40
  fast:
    cpus: 8
  empty:
  braces: {}
  max:
    cpus: 255
    memory_mib: 4294967295
    scratch_gib: 8589934591
  min: {cpus: 1, memory_mib: 1, scratch_gib: 1}
`)
	b := BuiltinProfile()
	if b != (Profile{CPUs: 2, MemoryMiB: 1024, ScratchGiB: 10}) {
		t.Errorf("BuiltinProfile = %+v, want 2 CPUs, 1024 MiB, 10 GiB", b)
	}
	want := Profiles{
		"default": {CPUs: 4, MemoryMiB: 4096, ScratchGiB: 40},
		"fast":    {CPUs: 8, MemoryMiB: 1024, ScratchGiB: 10},
		"empty":   b,
		"braces":  b,
		"max":     {CPUs: 255, MemoryMiB: 4294967295, ScratchGiB: 8589934591},
		"min":     {CPUs: 1, MemoryMiB: 1, ScratchGiB: 1},
	}
	if !reflect.DeepEqual(cfg.Profiles, want) {
		t.Errorf("Profiles = %+v, want %+v", cfg.Profiles, want)
	}
	if got := cfg.Profiles["max"].ScratchBytes(); got != 8589934591<<30 {
		t.Errorf("ScratchBytes = %d", got)
	}
}

// Lookup finds a defined profile, and default even when the file defines
// none (CFG-4), but no other undefined profile.
func TestLookup(t *testing.T) {
	t.Parallel()
	ps := Profiles{"big": {CPUs: 3, MemoryMiB: 1536, ScratchGiB: 4}}
	if p, ok := ps.Lookup("big"); !ok || p != ps["big"] {
		t.Errorf("Lookup(big) = %+v, %v", p, ok)
	}
	if p, ok := ps.Lookup("default"); !ok || p != BuiltinProfile() {
		t.Errorf("Lookup(default) with none defined = %+v, %v; want the built-in values", p, ok)
	}
	ps["default"] = Profile{CPUs: 1, MemoryMiB: 1, ScratchGiB: 1}
	if p, ok := ps.Lookup("default"); !ok || p != ps["default"] {
		t.Errorf("Lookup(default) = %+v, %v; want the defined one", p, ok)
	}
	for _, name := range []string{"small", "Big", ""} {
		if p, ok := ps.Lookup(name); ok {
			t.Errorf("Lookup(%q) = %+v, want none", name, p)
		}
	}
}

// A profile key's invalid value fails naming its line, the profile and the
// key (CFG-3, CFG-10). 0 is invalid, not the built-in value.
func TestInvalidProfileValuesFail(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value, want string
	}{
		{"cpus: 0", `cpus: must be from 1 to 255, not 0`},
		{"cpus: -1", `cpus: must be from 1 to 255, not -1`},
		{"cpus: 256", `cpus: must be from 1 to 255, not 256`},
		{"cpus: 1.5", `cpus: must be an integer, not "1.5"`},
		{`cpus: "2"`, `cpus: must be an integer, not "2"`},
		{"cpus: [2]", `cpus: must be an integer`},
		{"cpus:", `cpus: has no value`},
		{"memory_mib: 0", `memory_mib: must be from 1 to 4294967295, not 0`},
		{"memory_mib: -1024", `memory_mib: must be from 1 to 4294967295, not -1024`},
		{"memory_mib: 4294967296", `memory_mib: must be from 1 to 4294967295, not 4294967296`},
		{"scratch_gib: 0", `scratch_gib: must be from 1 to 8589934591, not 0`},
		{"scratch_gib: -10", `scratch_gib: must be from 1 to 8589934591, not -10`},
		{"scratch_gib: 8589934592", `scratch_gib: must be from 1 to 8589934591, not 8589934592`},
		{"scratch_gib: 17179869184", `scratch_gib: must be from 1 to 8589934591, not 17179869184`},
		{"scratch_gib: 18446744073709551615", `scratch_gib: must be from 1 to 8589934591, not 18446744073709551615`},
		{"scratch_gib: 99999999999999999999", `scratch_gib: must be from 1 to 8589934591, not 99999999999999999999`},
		{"scratch_gib: -99999999999999999999", `scratch_gib: must be from 1 to 8589934591, not -99999999999999999999`},
	} {
		path, msg := loadErr(t, "profiles:\n  ok: {}\n  big:\n    "+tc.value+"\n")
		wantErr(t, path, msg, `line 4: profile "big": `+tc.want)
	}
	// A profile with an invalid key is not kept, and its other errors are
	// still reported.
	path, msg := loadErr(t, "profiles:\n  big:\n    cpus: 0\n    memory_mib: 0\n")
	wantErr(t, path, msg, `line 3: profile "big": cpus:`)
	wantErr(t, path, msg, `line 4: profile "big": memory_mib:`)
}

// With max_memory_mib set, a profile whose memory_mib is more than it fails,
// naming both values, since no job on it could ever run. The implicit
// default profile isn't checked.
func TestProfileAboveMaxMemoryFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		content string
		want    []string
	}{
		{
			"profiles:\n  big:\n    memory_mib: 2048\n  default: {memory_mib: 512}\nmax_memory_mib: 1024\n",
			[]string{`line 3: profile "big": memory_mib: 2048 is more than max_memory_mib, 1024 (line 5)`},
		},
		// A profile that leaves memory_mib out has the built-in 1024 MiB,
		// and the error is on the profile's line.
		{
			"max_memory_mib: 1000\nprofiles:\n  default: {memory_mib: 1000}\n  small:\n",
			[]string{`line 4: profile "small": memory_mib: 1024 is more than max_memory_mib, 1000 (line 1)`},
		},
	} {
		path, msg := loadErr(t, tc.content)
		for _, want := range tc.want {
			wantErr(t, path, msg, want)
		}
		if n := strings.Count(msg, "\n") + 1; n != len(tc.want) {
			t.Errorf("%q: %d errors, want %d:\n%s", tc.content, n, len(tc.want), msg)
		}
	}
	// Equal is fine, and so is any profile when max_memory_mib is 0 or
	// unset, or invalid (its own error is enough).
	for _, content := range []string{
		"max_memory_mib: 1024\nprofiles:\n  big: {memory_mib: 1024}\n",
		"max_memory_mib: 0\nprofiles:\n  big: {memory_mib: 4294967295}\n",
		"profiles:\n  big: {memory_mib: 4294967295}\n",
		"max_memory_mib: 512\nprofiles:\n  small: {memory_mib: 512}\n",
	} {
		mustLoad(t, content)
	}
	path, msg := loadErr(t, "max_memory_mib: -1\nprofiles:\n  big: {memory_mib: 4096}\n")
	if strings.Contains(msg, "more than") {
		t.Errorf("an invalid max_memory_mib is compared:\n%s", msg)
	}
	wantErr(t, path, msg, `line 1: max_memory_mib: must be 0 or more`)
}

// dirsConfig is a config file setting the four paths under base.
func dirsConfig(base, listen, state, cache, runtime string) string {
	return fmt.Sprintf("listen: unix://%s/%s\nstate_dir: %s/%s\ncache_dir: %s/%s\nruntime_dir: %s/%s\n",
		base, listen, base, state, base, cache, base, runtime)
}

// Distinct directories, and a listen socket in the runtime directory or in
// a directory holding it, as the built-in values have it, are accepted.
func TestDistinctDirectoriesAreAccepted(t *testing.T) {
	t.Parallel()
	for _, tc := range [][4]string{
		{"listen/rm.sock", "state", "cache", "runtime"},
		{"runtime/rm.sock", "state", "cache", "runtime"},
		{"rm.sock", "../s", "../c", "jobs"},
		{"run/rm.sock", "lib/rm", "cache/rm", "run/jobs"},
		{"run/rm.sock", "state", "state2", "run/jobs"},
	} {
		base := filepath.Join(t.TempDir(), "rm")
		if _, err := load(writeConfig(t, dirsConfig(base, tc[0], tc[1], tc[2], tc[3])), true); err != nil {
			t.Errorf("%v: %v", tc, err)
		}
	}
	// Nothing on disk is written.
	if cfg, err := load(filepath.Join(t.TempDir(), "none"), false); err != nil {
		t.Errorf("built-in values: %v", err)
	} else if _, err := os.Stat(cfg.StateDir); err == nil && cfg.StateDir != wantBuiltin("").StateDir {
		t.Errorf("load created %s", cfg.StateDir)
	}
}

// Each pair of equal or nested state, cache, runtime and listen-socket
// directories fails, naming both keys.
func TestEqualOrNestedDirectoriesFail(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		listen, state, cache, runtime string
		want                          string
	}{
		{"l/s.sock", "a", "a", "r", `line 3: cache_dir: is the same directory as state_dir (line 2)`},
		{"l/s.sock", "a", "c", "a", `line 4: runtime_dir: is the same directory as state_dir (line 2)`},
		{"l/s.sock", "s", "a", "a", `line 4: runtime_dir: is the same directory as cache_dir (line 3)`},
		{"l/s.sock", "a", "a/c", "r", `line 3: cache_dir: is inside state_dir (line 2)`},
		{"l/s.sock", "a/s", "a", "r", `line 3: cache_dir: contains state_dir (line 2)`},
		{"l/s.sock", "s", "c", "s/r", `line 4: runtime_dir: is inside state_dir (line 2)`},
		{"l/s.sock", "s", "c/r/x", "c/r", `line 4: runtime_dir: contains cache_dir (line 3)`},
		{"s/s.sock", "s", "c", "r", `line 1: listen: the socket's directory is the same directory as state_dir (line 2)`},
		{"c/s.sock", "s", "c", "r", `line 1: listen: the socket's directory is the same directory as cache_dir (line 3)`},
		{"s/l/s.sock", "s", "c", "r", `line 1: listen: the socket's directory is inside state_dir (line 2)`},
		{"r/l/s.sock", "s", "c", "r", `line 1: listen: the socket's directory is inside runtime_dir (line 4)`},
		{"x/s.sock", "x/s", "c", "r", `line 1: listen: the socket's directory contains state_dir (line 2)`},
		{"x/r", "s", "c", "x/r", `line 1: listen: the socket is runtime_dir (line 4)`},
	} {
		base := filepath.Join(t.TempDir(), "rm")
		path, msg := loadErr(t, dirsConfig(base, tc.listen, tc.state, tc.cache, tc.runtime))
		wantErr(t, path, msg, tc.want)
	}
}

// A built-in directory that a set key collides with is named as built-in.
func TestCollisionWithABuiltinDirectoryNamesIt(t *testing.T) {
	t.Parallel()
	path, msg := loadErr(t, "cache_dir: /var/lib/runner-microvm/cache\n")
	wantErr(t, path, msg, `line 1: cache_dir: is inside state_dir (built-in value /var/lib/runner-microvm)`)
}

// A symlink that makes two directories the same is caught, also when the
// directories don't exist yet below it.
func TestSymlinkedDirectoriesFail(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "resolved"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("resolved", filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(base, "resolved"))
	if err != nil {
		t.Fatal(err)
	}
	path, msg := loadErr(t, dirsConfig(base, "l/s.sock", "resolved", "link", "r"))
	wantErr(t, path, msg, `line 3: cache_dir: is the same directory as state_dir (line 2), `+resolved+` with symlinks resolved`)
	path, msg = loadErr(t, dirsConfig(base, "l/s.sock", "resolved/x/state", "c", "link/x"))
	wantErr(t, path, msg, `line 4: runtime_dir: contains state_dir (line 2)`)
}

// A path whose resolution fails for a reason other than a missing directory
// fails naming the key.
func TestUnresolvablePathFails(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	loop := filepath.Join(base, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	path, msg := loadErr(t, dirsConfig(base, "l/s.sock", "loop/state", "c", "r"))
	wantErr(t, path, msg, `line 2: state_dir: resolve symlinks in `+loop+`/state`)
	if !strings.Contains(msg, "too many links") {
		t.Errorf("error %q does not say why", msg)
	}
}

// The listen socket's directory may hold the runtime directory only as its
// direct parent: further up, it is a system directory such as /run, whose
// lock file serve would create and hold.
func TestListenDirectoryFurtherUpThanTheRuntimeDirectoryFails(t *testing.T) {
	t.Parallel()
	base := filepath.Join(t.TempDir(), "rm")
	path, msg := loadErr(t, dirsConfig(base, "x.sock", "../s", "../c", "run/jobs"))
	wantErr(t, path, msg, `line 1: listen: the socket's directory contains runtime_dir (line 4)`)
}

// A symlink whose target doesn't exist yet is resolved through to its
// target, so it can't hide two directories being the same.
func TestDanglingSymlinkedDirectoriesFail(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	if err := os.Symlink("r2", filepath.Join(base, "dang")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "missing", "..", "r3"), filepath.Join(base, "dang2")); err != nil {
		t.Fatal(err)
	}
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	path, msg := loadErr(t, dirsConfig(base, "l/s.sock", "r2", "c", "dang"))
	wantErr(t, path, msg, `line 4: runtime_dir: is the same directory as state_dir (line 2), `+resolvedBase+`/r2 with symlinks resolved`)
	path, msg = loadErr(t, dirsConfig(base, "l/s.sock", "s", "r3/c", "dang2"))
	wantErr(t, path, msg, `line 4: runtime_dir: contains cache_dir (line 3)`)
}

// The socket may not be any locked directory's lock file: serve would fail
// to bind it, with an error that leads the operator to delete the lock.
func TestSocketAtALockFileFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		listen, want string
	}{
		{"r/lock", `line 1: listen: the socket is the lock file of runtime_dir (line 4)`},
		{"l/lock", `line 1: listen: the socket is the lock file of its own directory`},
	} {
		base := filepath.Join(t.TempDir(), "rm")
		path, msg := loadErr(t, dirsConfig(base, tc.listen, "s", "c", "r"))
		wantErr(t, path, msg, tc.want)
	}
}

// No directory may be a locked directory's lock file, also the runtime
// directory when the socket's directory is its direct parent, which the
// other directory checks skip: serve would create the directory and then
// fail opening it as the lock file.
func TestDirectoryAtALockFileFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		listen, state, cache, runtime, want string
	}{
		{"rm.sock", "../s", "../c", "lock", `line 4: runtime_dir: is the lock file of the directory of listen (line 1), `},
		{"l/rm.sock", "s", "s/lock", "r", `line 3: cache_dir: is the lock file of state_dir (line 2), `},
		{"r/lock/rm.sock", "s", "c", "r", `line 1: listen: the socket's directory is the lock file of runtime_dir (line 4), `},
	} {
		base := filepath.Join(t.TempDir(), "rm")
		path, msg := loadErr(t, dirsConfig(base, tc.listen, tc.state, tc.cache, tc.runtime))
		wantErr(t, path, msg, tc.want)
		if n := strings.Count(msg, "\n") + 1; n != 1 {
			t.Errorf("%v: %d errors, want 1:\n%s", tc, n, msg)
		}
	}
}

// A file with decoding errors also gets the directory checks, on the
// directory keys that decoded validly, so check prints every error.
func TestDirectoryErrorsAreReportedWithDecodingErrors(t *testing.T) {
	t.Parallel()
	base := filepath.Join(t.TempDir(), "rm")
	path, msg := loadErr(t, fmt.Sprintf("state_dir: %s/a\ncache_dir: %s/a\nruntime_dir: rel\nbogus: 1\n", base, base))
	for _, want := range []string{
		`line 2: cache_dir: is the same directory as state_dir (line 1)`,
		`line 3: runtime_dir: must be an absolute path`,
		`line 4: bogus: unknown key`,
	} {
		wantErr(t, path, msg, want)
	}
	if n := strings.Count(msg, "\n") + 1; n != 3 {
		t.Errorf("%d errors, want 3:\n%s", n, msg)
	}
}

// A dangling symlink at the default path, or on the way to it, is a file
// that can't be read, not a missing one: it stops startup.
func TestDanglingSymlinkAtTheDefaultPathFails(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	file := filepath.Join(base, "config.yaml")
	if err := os.Symlink("gone.yaml", file); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "etc")
	if err := os.Symlink("gone", dir); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, filepath.Join(dir, "config.yaml")} {
		cfg, err := load(path, false)
		if err == nil {
			t.Errorf("%s: %+v, want an error", path, cfg)
			continue
		}
		if !strings.HasPrefix(err.Error(), "config "+path+": ") {
			t.Errorf("%s: error %q does not name it", path, err)
		}
	}
	// A symlinked directory that exists, with no file in it, is no file.
	if err := os.Mkdir(filepath.Join(base, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := load(filepath.Join(base, "link", "config.yaml"), false); err != nil {
		t.Errorf("no file behind a symlinked directory: %v", err)
	}
}
