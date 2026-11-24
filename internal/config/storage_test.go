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

// sources makes n host directories for mount sources, and returns their
// paths.
func sources(t *testing.T, n int) []string {
	t.Helper()
	dir := t.TempDir()
	var paths []string
	for i := range n {
		p := filepath.Join(dir, fmt.Sprintf("src%d", i))
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	return paths
}

// mounts is a profile's mounts key, each entry a source and a target.
func mounts(pairs ...string) string {
	var b strings.Builder
	b.WriteString("    mounts:\n")
	for i := 0; i+1 < len(pairs); i += 2 {
		fmt.Fprintf(&b, "      - {source: %s, target: %s}\n", pairs[i], pairs[i+1])
	}
	return b.String()
}

// A profile's mounts are host directories at guest targets, read-only
// unless read_write is true: each source and target cleaned, in the list's
// order. A profile without mounts, or with an empty list, has none (STO-1,
// STO-2, STO-3).
func TestMounts(t *testing.T) {
	t.Parallel()
	src := sources(t, 3)
	cfg := mustLoad(t, fmt.Sprintf(`
profiles:
  tools:
    mounts:
      - source: %s/
        target: /opt/tools/
      - {source: %s, target: /srv//data, read_write: false}
      - {source: %s/../src2, target: /a/./b, read_write: true}
  none: {}
  empty:
    mounts: []
`, src[0], src[1], src[1]))
	want := []Mount{
		{Source: src[0], Target: "/opt/tools"},
		{Source: src[1], Target: "/srv/data"},
		{Source: src[2], Target: "/a/b", ReadWrite: true},
	}
	if got := cfg.Profiles["tools"].Mounts; !reflect.DeepEqual(got, want) {
		t.Errorf("Mounts = %+v, want %+v", got, want)
	}
	for _, name := range []string{"none", "empty"} {
		if got := cfg.Profiles[name].Mounts; len(got) != 0 {
			t.Errorf("profile %s: Mounts = %+v, want []", name, got)
		}
	}
	if got := BuiltinProfile().Mounts; len(got) != 0 {
		t.Errorf("the built-in mounts = %+v, want []", got)
	}
}

// A mount whose target is under one of the same profile's scratch paths
// loads: it is mounted after the scratch path (STO-6). A target only sharing
// a prefix with another isn't under it.
func TestMountUnderAScratchPathLoads(t *testing.T) {
	t.Parallel()
	src := sources(t, 2)
	cfg := mustLoad(t, "profiles:\n  docker:\n    scratch_paths: [/var/lib/docker]\n"+
		mounts(src[0], "/var/lib/docker/tools", src[1], "/var/lib/dockerx"))
	if got := len(cfg.Profiles["docker"].Mounts); got != 2 {
		t.Errorf("%d mounts, want 2", got)
	}
}

// A mount under one of the guest's own directories loads, and so does one
// only sharing a prefix with one: neither hides it.
func TestMountUnderAGuestDirectoryLoads(t *testing.T) {
	t.Parallel()
	src := sources(t, 3)
	cfg := mustLoad(t, "profiles:\n  tools:\n"+mounts(src[0], "/opt/hostedtoolcache/go", src[1], "/tmpdata", src[2], "/var/run/actions"))
	if got := len(cfg.Profiles["tools"].Mounts); got != 3 {
		t.Errorf("%d mounts, want 3", got)
	}
}

// Each invalid mount fails at load, naming its line, the key and the
// profile (STO-5, STO-6, CFG-2, CFG-3).
func TestInvalidMountsFail(t *testing.T) {
	t.Parallel()
	src := sources(t, 2)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	for _, tc := range []struct {
		name, keys, want string
	}{
		{"relative source", mounts("srv/tools", "/srv"), `line 5: profile "big": mounts: source: "srv/tools" is not an absolute path`},
		{"missing source", mounts(missing, "/srv"), fmt.Sprintf(`line 5: profile "big": mounts: source: %q does not exist`, missing)},
		{"file source", mounts(file, "/srv"), fmt.Sprintf(`line 5: profile "big": mounts: source: %q is not a directory`, file)},
		{"relative target", mounts(src[0], "opt"), `line 5: profile "big": mounts: target: "opt" is not an absolute path`},
		{"root target", mounts(src[0], "/"), `line 5: profile "big": mounts: target: "/" is the root`},
		{"unclean root target", mounts(src[0], "/a/.."), `line 5: profile "big": mounts: target: "/a/.." is the root`},
		{"proc target", mounts(src[0], "/proc"), `line 5: profile "big": mounts: target: "/proc" is under /proc`},
		{"sys target", mounts(src[0], "/sys/fs/cgroup"), `line 5: profile "big": mounts: target: "/sys/fs/cgroup" is under /sys`},
		{"dev target", mounts(src[0], "/x/../dev/shm"), `line 5: profile "big": mounts: target: "/x/../dev/shm" is under /dev`},
		{"NUL target", "    mounts: [{source: " + src[0] + `, target: "/a\0b"}]` + "\n", `line 4: profile "big": mounts: target: "/a\x00b" holds a NUL byte`},
		{"target twice", mounts(src[0], "/srv", src[1], "/srv/"), `line 6: profile "big": mounts: target: "/srv/" is already a target on line 5`},
		{"target a scratch path", "    scratch_paths: [/srv]\n" + mounts(src[0], "/srv/."), `line 6: profile "big": mounts: target: "/srv/." is the scratch path on line 4: one would hide the other`},
		{"target above a scratch path", "    scratch_paths: [/work/cache]\n" + mounts(src[0], "/work/"), `line 6: profile "big": mounts: target: "/work/" is above the scratch path "/work/cache" on line 4: it would hide it`},
		{"target the act path", mounts(src[0], "/var/run/act"), `line 5: profile "big": mounts: target: "/var/run/act" is the guest's act path, /var/run/act: it would hide it`},
		{"target above the act path", mounts(src[0], "/var"), `line 5: profile "big": mounts: target: "/var" is above the guest's act path, /var/run/act: it would hide it`},
		{"target above the tool cache", mounts(src[0], "/opt/"), `line 5: profile "big": mounts: target: "/opt/" is above the guest's tool cache, /opt/hostedtoolcache: it would hide it`},
		{"target the temp directory", mounts(src[0], "/x/../tmp"), `line 5: profile "big": mounts: target: "/x/../tmp" is the guest's temp directory, /tmp: it would hide it`},
		{"target under another", mounts(src[0], "/srv", src[1], "/srv/tools"), `line 6: profile "big": mounts: target: "/srv/tools" is under the target "/srv" on line 5`},
		{"target above another", mounts(src[0], "/srv/tools", src[1], "/srv"), `line 5: profile "big": mounts: target: "/srv/tools" is under the target "/srv" on line 6`},
		{"unknown entry key", "    mounts:\n      - {source: " + src[0] + ", target: /srv, readonly: true}\n", `line 5: profile "big": mounts: readonly: unknown key`},
		{"entry key twice", "    mounts:\n      - source: " + src[0] + "\n        source: " + src[1] + "\n        target: /srv\n", `line 6: profile "big": mounts: source: is already set on line 5`},
		{"no source", "    mounts:\n      - {target: /srv}\n", `line 5: profile "big": mounts: an entry needs a source`},
		{"no target", "    mounts:\n      - {source: " + src[0] + "}\n", `line 5: profile "big": mounts: an entry needs a target`},
		{"read_write not a boolean", "    mounts:\n      - {source: " + src[0] + ", target: /srv, read_write: yes}\n", `line 5: profile "big": mounts: read_write: must be true or false, not "yes"`},
		{"source not a string", "    mounts:\n      - {source: 1, target: /srv}\n", `line 5: profile "big": mounts: source: must be a string`},
		{"entry not a mapping", "    mounts: [/srv]\n", `line 4: profile "big": mounts: each entry must be a mapping of source, target and read_write`},
		{"not a list", "    mounts: {source: /srv}\n", `line 4: profile "big": mounts: must be a list of entries`},
		{"no value", "    mounts:\n", `line 4: profile "big": mounts: has no value`},
	} {
		path, msg := loadErr(t, "profiles:\n  ok: {}\n  big:\n"+tc.keys)
		wantErr(t, path, msg, tc.want)
		if n := strings.Count(msg, "\n") + 1; n != 1 {
			t.Errorf("%s: %d errors, want 1:\n%s", tc.name, n, msg)
		}
	}
}

// Every invalid mount is reported, each at its own line, and a profile with
// one is not kept.
func TestEveryInvalidMountIsReported(t *testing.T) {
	t.Parallel()
	path, msg := loadErr(t, "profiles:\n  big:\n"+mounts("rel", "/proc", "/also-rel", "x"))
	for _, want := range []string{
		`line 4: profile "big": mounts: source: "rel" is not an absolute path`,
		`line 4: profile "big": mounts: target: "/proc" is under /proc`,
		`line 5: profile "big": mounts: target: "x" is not an absolute path`,
	} {
		wantErr(t, path, msg, want)
	}
}

// On amd64, a profile has at most 3 mounts plus disks, whatever the host
// running the tests: libkrun's x86_64 interrupt lines run out (CFG-10). On
// arm64 there is no limit.
func TestMountsPlusDisksLimit(t *testing.T) {
	t.Parallel()
	src := sources(t, 4)
	three := "profiles:\n  three:\n" + mounts(src[0], "/a", src[1], "/b", src[2], "/c")
	four := "profiles:\n  four:\n" + mounts(src[0], "/a", src[1], "/b", src[2], "/c", src[3], "/d")
	for _, content := range []string{three, four} {
		if _, err := loadOn(writeConfig(t, content), true, nil, "arm64"); err != nil {
			t.Errorf("arm64: %v", err)
		}
	}
	if _, err := loadOn(writeConfig(t, three), true, nil, "amd64"); err != nil {
		t.Errorf("amd64, three: %v", err)
	}
	path := writeConfig(t, four)
	_, err := loadOn(path, true, nil, "amd64")
	if err == nil {
		t.Fatal("amd64: four mounts loaded")
	}
	wantErr(t, path, err.Error(), `line 3: profile "four": mounts: 4 mounts and disks, more than 3, the most a VM can have on amd64: `+
		`libkrun's x86_64 VMs have 11 virtio-mmio interrupt lines, and every VM already uses 8`)
}

// Sources are checked when the file is read, so a reload fails on a source
// that is gone, and the old config stays (CFG-8, #88).
func TestReloadChecksMountSources(t *testing.T) {
	t.Parallel()
	src := sources(t, 1)
	content := "profiles:\n  tools:\n" + mounts(src[0], "/opt/tools")
	path := writeConfig(t, content)
	c, err := load(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src[0]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Reload(); err == nil || !strings.Contains(err.Error(), fmt.Sprintf(`mounts: source: %q does not exist`, src[0])) {
		t.Errorf("Reload with the source gone: %v", err)
	}
	if got := c.Profiles["tools"].Mounts; len(got) != 1 || got[0].Source != src[0] {
		t.Errorf("the running config's mounts changed to %+v", got)
	}
}
