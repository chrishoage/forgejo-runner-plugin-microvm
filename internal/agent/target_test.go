// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A mount target resolves inside the job's root: one the image has is
// found, one it lacks is created with its missing parents, root-owned mode
// 0755, and one under an absolute symlink lands at the link's target in the
// root, never outside it (STO-5, STO-6).
func TestResolveTargetFindsOrCreatesInTheRoot(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	root := filepath.Join(outside, "root")
	for _, d := range []string{"root/opt/tools", "root/real", "root/run"} {
		if err := os.MkdirAll(filepath.Join(outside, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{"link": "/real", "var-run": "/run", "up": "../../..", "abs": outside, "dangling": "/made"} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	for guest, want := range map[string]string{
		"/opt/tools":      "/opt/tools",
		"/new/deep":       "/new/deep",
		"/link/sub":       "/real/sub",
		"/var-run/x":      "/run/x",
		"/up/escaped":     "/escaped",
		"/abs/root/inner": outside + "/root/inner",
		"/dangling/y":     "/made/y",
	} {
		place, err := kernel{}.resolveTarget(root, guest)
		if err != nil {
			t.Errorf("%s: %v", guest, err)
			continue
		}
		if place != want {
			t.Errorf("%s lands at %s, want %s", guest, place, want)
		}
		if fi, err := os.Stat(filepath.Join(root, place)); err != nil || !fi.IsDir() {
			t.Errorf("%s: %v, %v; want a directory at %s in the root", guest, fi, err, place)
		}
	}
	for _, d := range []string{"new", "new/deep", "real/sub", "made", "made/y"} {
		if fi, err := os.Stat(filepath.Join(root, d)); err != nil || fi.Mode().Perm() != 0o755 {
			t.Errorf("/%s: %v, %v; want mode 0755", d, fi, err)
		}
	}
	// The image's own directory keeps its mode.
	if fi, err := os.Stat(filepath.Join(root, "opt/tools")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("/opt/tools: %v, %v; want its own mode 0700", fi, err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("made %v outside the root", entries)
	}
}

// A target that is a file in the image, or under one, or a symlink to a
// file, fails, naming the part that is; so does one that lands in /proc,
// /sys or /dev through a symlink, before anything is made there.
func TestResolveTargetRefuses(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"filelink": "/file", "devlink": "/dev", "syslink": "/sys/fs"} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	for guest, want := range map[string]string{
		"/file":         "/file is not a directory in the image",
		"/file/x":       "/file is not a directory in the image",
		"/filelink":     "/filelink is not a directory in the image",
		"/devlink/shm2": "lands at /dev/shm2, in /dev",
		"/devlink":      "lands at /dev, in /dev",
		"/syslink/x":    "lands at /sys, in /sys",
	} {
		place, err := kernel{}.resolveTarget(root, guest)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %q, %v; want an error with %q", guest, place, err, want)
		}
	}
	for _, p := range []string{"dev/shm2", "sys"} {
		if _, err := os.Lstat(filepath.Join(root, p)); err == nil {
			t.Errorf("/%s was made", p)
		}
	}
}

// A place that lands somewhere else when it is looked up again at mount
// time, as through a symlink in a host directory mounted above it since
// (sub -> /), fails before any mount is made, naming both.
func TestMountShareRefusesAPlaceThatMoved(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "share"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(root, "share/sub")); err != nil {
		t.Fatal(err)
	}
	err := kernel{}.mountShare(root, "mount0", "/share/sub", true)
	if want := "/share/sub now lands at /"; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
}
