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

// diskFiles makes n host files for disk sources, and returns their paths.
func diskFiles(t *testing.T, n int) []string {
	t.Helper()
	dir := t.TempDir()
	var paths []string
	for i := range n {
		p := filepath.Join(dir, fmt.Sprintf("disk%d.img", i))
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	return paths
}

// disks is a profile's disks key, each entry a source and a target.
func disks(pairs ...string) string {
	return strings.Replace(mounts(pairs...), "    mounts:\n", "    disks:\n", 1)
}

// blockDevice returns a block device in /dev, or skips t when there is
// none, saying so, unless CI is set.
func blockDevice(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir("/dev")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Type()&os.ModeDevice != 0 && e.Type()&os.ModeCharDevice == 0 {
			return filepath.Join("/dev", e.Name())
		}
	}
	if os.Getenv("CI") != "" {
		t.Fatal("CI is set, and /dev holds no block device")
	}
	t.Skip("SKIPPED: /dev holds no block device")
	return ""
}

// A profile's disks are host files at guest targets, read-only unless
// read_write is true: each source and target cleaned, in the list's order, a
// symlink to a file included. A profile without disks, or with an empty
// list, has none (STO-1, STO-2, STO-4).
func TestDisks(t *testing.T) {
	t.Parallel()
	src := diskFiles(t, 3)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(src[1], link); err != nil {
		t.Fatal(err)
	}
	cfg := mustLoad(t, fmt.Sprintf(`
profiles:
  data:
    disks:
      - source: %s
        target: /srv/data/
      - {source: %s, target: /a/./b, read_write: false}
      - {source: %s//, target: /var/lib/store, read_write: true}
  none: {}
  empty:
    disks: []
`, src[0], link, src[2]))
	want := []Disk{{Source: src[0], Target: "/srv/data"}, {Source: link, Target: "/a/b"}, {Source: src[2], Target: "/var/lib/store", ReadWrite: true}}
	if got := cfg.Profiles["data"].Disks; !reflect.DeepEqual(got, want) {
		t.Errorf("Disks = %+v, want %+v", got, want)
	}
	for _, name := range []string{"none", "empty"} {
		if got := cfg.Profiles[name].Disks; len(got) != 0 {
			t.Errorf("profile %s: Disks = %+v, want []", name, got)
		}
	}
	if got := BuiltinProfile().Disks; len(got) != 0 {
		t.Errorf("the built-in disks = %+v, want []", got)
	}
}

// A disk under a scratch path, or beside a mount, loads (STO-6).
func TestDisksBesideMountsLoad(t *testing.T) {
	t.Parallel()
	src, dirs := diskFiles(t, 2), sources(t, 1)
	cfg := mustLoad(t, "profiles:\n  docker:\n    scratch_paths: [/var/lib/docker]\n"+
		mounts(dirs[0], "/opt/tools")+disks(src[0], "/var/lib/docker/store", src[1], "/srv/data"))
	if got := cfg.Profiles["docker"]; len(got.Disks) != 2 || len(got.Mounts) != 1 {
		t.Errorf("%d disks and %d mounts, want 2 and 1", len(got.Disks), len(got.Mounts))
	}
}

// Each invalid disk fails at load, naming its line, the key and the
// profile, and so does a disk whose target a mount uses, or that a mount's
// target is under, whichever list comes first (STO-5, STO-6, CFG-2).
func TestInvalidDisksFail(t *testing.T) {
	t.Parallel()
	src, dirs := diskFiles(t, 2), sources(t, 2)
	missing := filepath.Join(t.TempDir(), "missing")
	dev := blockDevice(t)
	for _, tc := range []struct {
		name, keys, want string
	}{
		{"relative source", disks("srv/disk.img", "/srv"), `line 5: profile "big": disks: source: "srv/disk.img" is not an absolute path`},
		{"missing source", disks(missing, "/srv"), fmt.Sprintf(`line 5: profile "big": disks: source: %q does not exist`, missing)},
		{"directory source", disks(dirs[0], "/srv"), fmt.Sprintf(`line 5: profile "big": disks: source: %q is not a regular file`, dirs[0])},
		{"block device source", disks(dev, "/srv"), fmt.Sprintf(`line 5: profile "big": disks: source: %q is a block device: a disk's source must be a regular file`, dev)},
		{"character device source", disks("/dev/null", "/srv"), `line 5: profile "big": disks: source: "/dev/null" is not a regular file`},
		{"read_write not a boolean", "    disks:\n      - {source: " + src[0] + ", target: /srv, read_write: 1}\n", `line 5: profile "big": disks: read_write: must be true or false, not "1"`},
		{"root target", disks(src[0], "/"), `line 5: profile "big": disks: target: "/" is the root`},
		{"proc target", disks(src[0], "/proc/x"), `line 5: profile "big": disks: target: "/proc/x" is under /proc`},
		{"target twice", disks(src[0], "/srv", src[1], "/srv/"), `line 6: profile "big": disks: target: "/srv/" is already a target on line 5`},
		{"target a mount's", mounts(dirs[0], "/srv") + disks(src[0], "/srv"), `line 7: profile "big": disks: target: "/srv" is already a target on line 5`},
		{"target a disk's", disks(src[0], "/srv") + mounts(dirs[0], "/srv"), `line 7: profile "big": mounts: target: "/srv" is already a target on line 5`},
		{"target under a mount's", mounts(dirs[0], "/srv") + disks(src[0], "/srv/data"), `line 7: profile "big": disks: target: "/srv/data" is under the target "/srv" on line 5`},
		{"mount under a disk's", disks(src[0], "/srv") + mounts(dirs[0], "/srv/tools"), `line 7: profile "big": mounts: target: "/srv/tools" is under the target "/srv" on line 5`},
		{"target a scratch path", "    scratch_paths: [/srv]\n" + disks(src[0], "/srv"), `line 6: profile "big": disks: target: "/srv" is the scratch path on line 4: one would hide the other`},
		{"target above the tool cache", disks(src[0], "/opt"), `line 5: profile "big": disks: target: "/opt" is above the guest's tool cache, /opt/hostedtoolcache: it would hide it`},
		{"unknown entry key", "    disks:\n      - {source: " + src[0] + ", target: /srv, fs: ext4}\n", `line 5: profile "big": disks: fs: unknown key`},
		{"no source", "    disks:\n      - {target: /srv}\n", `line 5: profile "big": disks: an entry needs a source`},
		{"entry not a mapping", "    disks: [/srv]\n", `line 4: profile "big": disks: each entry must be a mapping of source, target and read_write`},
		{"no value", "    disks:\n", `line 4: profile "big": disks: has no value`},
	} {
		path, msg := loadErr(t, "profiles:\n  ok: {}\n  big:\n"+tc.keys)
		wantErr(t, path, msg, tc.want)
		if n := strings.Count(msg, "\n") + 1; n != 1 {
			t.Errorf("%s: %d errors, want 1:\n%s", tc.name, n, msg)
		}
	}
}

// One file may be the source of two disks only when both are read-only:
// listed twice with either entry read-write, by the same path, another
// spelling of it, a symlink or a hard link, it stops startup at the later
// entry's source, naming the line, the key and the profile; two read-only
// entries for one source load (STO-7, STO-8).
func TestDiskListedTwice(t *testing.T) {
	t.Parallel()
	src := diskFiles(t, 2)
	link, hard := filepath.Join(t.TempDir(), "link"), filepath.Join(filepath.Dir(src[0]), "hard.img")
	if err := os.Symlink(src[0], link); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(src[0], hard); err != nil {
		t.Fatal(err)
	}
	entry := func(source, target string, rw bool) string {
		return fmt.Sprintf("      - {source: %s, target: %s, read_write: %v}\n", source, target, rw)
	}
	for _, tc := range []struct {
		name, second      string
		firstRW, secondRW bool
		wantFails         bool
	}{
		{"both read-write", src[0], true, true, true},
		{"first read-write", src[0], true, false, true},
		{"second read-write", src[0], false, true, true},
		{"another spelling", src[0] + "/", true, false, true},
		{"by a symlink", link, false, true, true},
		{"by a hard link", hard, true, false, true},
		{"both read-only", src[0], false, false, false},
		{"both read-only by a symlink", link, false, false, false},
		{"another file", src[1], true, true, false},
	} {
		keys := "    disks:\n" + entry(src[0], "/srv/a", tc.firstRW) + entry(tc.second, "/srv/b", tc.secondRW)
		content := "profiles:\n  ok: {}\n  big:\n" + keys
		if !tc.wantFails {
			if got := len(mustLoad(t, content).Profiles["big"].Disks); got != 2 {
				t.Errorf("%s: %d disks, want 2", tc.name, got)
			}
			continue
		}
		path, msg := loadErr(t, content)
		wantErr(t, path, msg, fmt.Sprintf(`line 6: profile "big": disks: source: %q is the same file as the source on line 5: a disk listed twice must be read-only both times`, tc.second))
		if n := strings.Count(msg, "\n") + 1; n != 1 {
			t.Errorf("%s: %d errors, want 1:\n%s", tc.name, n, msg)
		}
	}
}

// Mounts and disks share the amd64 limit: 2 mounts plus 2 disks stop
// startup at the disks' line, and 2 plus 1 load (CFG-10).
func TestMountsAndDisksShareTheLimit(t *testing.T) {
	t.Parallel()
	src, dirs := diskFiles(t, 2), sources(t, 2)
	ms := mounts(dirs[0], "/a", dirs[1], "/b")
	three := "profiles:\n  three:\n" + ms + disks(src[0], "/c")
	if _, err := loadOn(writeConfig(t, three), true, nil, "amd64"); err != nil {
		t.Errorf("amd64, 2 mounts and 1 disk: %v", err)
	}
	four := "profiles:\n  four:\n" + ms + disks(src[0], "/c", src[1], "/d")
	if _, err := loadOn(writeConfig(t, four), true, nil, "arm64"); err != nil {
		t.Errorf("arm64, 2 mounts and 2 disks: %v", err)
	}
	path := writeConfig(t, four)
	_, err := loadOn(path, true, nil, "amd64")
	if err == nil {
		t.Fatal("amd64: 2 mounts and 2 disks loaded")
	}
	wantErr(t, path, err.Error(), `line 6: profile "four": disks: 4 mounts and disks, more than 3, the most a VM can have on amd64: `+
		`libkrun's x86_64 VMs have 11 virtio-mmio interrupt lines, and every VM already uses 8`)
}

// A reload fails on a disk source that is gone, and the old config stays
// (CFG-8).
func TestReloadChecksDiskSources(t *testing.T) {
	t.Parallel()
	src := diskFiles(t, 1)
	path := writeConfig(t, "profiles:\n  data:\n"+disks(src[0], "/srv/data"))
	c, err := load(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src[0]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Reload(); err == nil || !strings.Contains(err.Error(), fmt.Sprintf(`disks: source: %q does not exist`, src[0])) {
		t.Errorf("Reload with the source gone: %v", err)
	}
	if got := c.Profiles["data"].Disks; len(got) != 1 || got[0].Source != src[0] {
		t.Errorf("the running config's disks changed to %+v", got)
	}
}
