// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wantDir fails t unless path is a directory of mode mode, permission and
// sticky bits included.
func wantDir(t *testing.T, path string, mode fs.FileMode) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Errorf("%s: %v", path, err)
		return
	}
	if got := fi.Mode() & (fs.ModeType | fs.ModePerm | fs.ModeSticky | fs.ModeSetuid | fs.ModeSetgid); got != fs.ModeDir|mode {
		t.Errorf("%s: mode %v, want %v", path, got, fs.ModeDir|mode)
	}
}

// On an image without them, the act path, tool cache and temp directory are
// created with their modes, the temp directory's sticky bit included, which
// the umask can't drop, and their missing parents 0755.
func TestMakeDirCreatesWhatTheImageLacks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, d := range testGuestDirs() {
		if err := (kernel{}).makeDir(root, d.GetPath(), d.GetMode()); err != nil {
			t.Fatal(err)
		}
	}
	wantDir(t, filepath.Join(root, "tmp"), 0o777|fs.ModeSticky)
	wantDir(t, filepath.Join(root, "var/run/act"), 0o777)
	wantDir(t, filepath.Join(root, "opt/hostedtoolcache"), 0o777)
	for _, d := range []string{"var", "var/run", "opt"} {
		wantDir(t, filepath.Join(root, d), 0o755)
	}
}

// What the image has there is left as it is: a directory of another mode
// and a file.
func TestMakeDirLeavesWhatTheImageHas(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("image"), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/tmp", "/file"} {
		if err := (kernel{}).makeDir(root, p, 0o1777); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	wantDir(t, filepath.Join(root, "tmp"), 0o700)
	if b, err := os.ReadFile(filepath.Join(root, "file")); err != nil || string(b) != "image" {
		t.Errorf("/file: %q, %v", b, err)
	}
}

// A symlink to nothing in the image is followed, inside the job's root, and
// its target made, with its missing parents: an image's /var/run -> /run
// without /run puts the act path at /run/act. A relative target is taken
// from where the link's directory really is, a chain of links is followed
// to its end, and neither an absolute host path nor `..` leaves the root.
func TestMakeDirFollowsASymlinkToNothing(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	root := filepath.Join(outside, "root")
	if err := os.MkdirAll(filepath.Join(root, "var"), 0o755); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"var/run": "/run",
		// opt -> var/opt, a link in a real directory, -> ../srv/opt.
		"opt":     "var/opt",
		"var/opt": "../srv/opt",
		"host":    filepath.Join(outside, "new"),
		"up":      "../../../../escape",
	}
	for link, target := range links {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	for p, mode := range map[string]uint32{"/var/run/act": 0o777, "/opt/hostedtoolcache": 0o777, "/host": 0o1777, "/up/x": 0o777} {
		if err := (kernel{}).makeDir(root, p, mode); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	wantDir(t, filepath.Join(root, "run"), 0o755)
	wantDir(t, filepath.Join(root, "run/act"), 0o777)
	wantDir(t, filepath.Join(root, "srv/opt/hostedtoolcache"), 0o777)
	wantDir(t, filepath.Join(root, outside, "new"), 0o777|fs.ModeSticky)
	wantDir(t, filepath.Join(root, "escape/x"), 0o777)
	for link, target := range links {
		if got, err := os.Readlink(filepath.Join(root, link)); err != nil || got != target {
			t.Errorf("/%s: %q, %v, want the image's link to %s", link, got, err, target)
		}
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("made %v outside the root", entries)
	}
}

// A loop of symlinks fails, naming the directory.
func TestMakeDirRefusesASymlinkLoop(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Symlink("/b", filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/a", filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	err := (kernel{}).makeDir(root, "/a/x", 0o777)
	if err == nil || !strings.Contains(err.Error(), "directory /a/x") {
		t.Errorf("err = %v, want one naming /a/x", err)
	}
}

// A directory that lands in /proc, /sys or /dev through an image symlink, or
// would be created there, fails, naming where, and nothing is made: the
// agent never creates a cgroup or a file in the devtmpfs holding its
// staging directory. /devices is not in /dev.
func TestMakeDirRefusesTheAPIMounts(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		link, target, guestPath, want string
	}{
		"cgroup":   {"opt", "/sys/fs/cgroup", "/opt/hostedtoolcache", "directory /opt/hostedtoolcache: lands at /sys/fs/cgroup/hostedtoolcache, in /sys"},
		"dev":      {"var/run", "/dev", "/var/run/act", "directory /var/run/act: lands at /dev/act, in /dev"},
		"proc":     {"var/run", "../proc/1", "/var/run/act", "lands at /proc/1/act, in /proc"},
		"existing": {"tmp", "/dev/shm", "/tmp", "directory /tmp: lands at /dev/shm, in /dev"},
		"under":    {"var/run", "/sys/new", "/var/run/act", "directory /var/run/act: lands at /sys/new, in /sys"},
	} {
		root := t.TempDir()
		for _, d := range []string{"var", "sys/fs/cgroup", "dev/shm", "proc/1"} {
			if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(tc.target, filepath.Join(root, tc.link)); err != nil {
			t.Fatal(err)
		}
		err := (kernel{}).makeDir(root, tc.guestPath, 0o777)
		if err == nil || !strings.HasSuffix(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %s", name, err, tc.want)
		}
		for _, d := range []string{"sys/fs/cgroup/hostedtoolcache", "dev/act", "proc/1/act", "sys/new"} {
			if _, err := os.Lstat(filepath.Join(root, d)); !os.IsNotExist(err) {
				t.Errorf("%s: made /%s: %v", name, d, err)
			}
		}
	}
	root := t.TempDir()
	if err := os.Symlink("/devices", filepath.Join(root, "tmp")); err != nil {
		t.Fatal(err)
	}
	if err := (kernel{}).makeDir(root, "/tmp", 0o1777); err != nil {
		t.Errorf("/devices: %v", err)
	}
	wantDir(t, filepath.Join(root, "devices"), 0o777|fs.ModeSticky)
}

// A symlink in the image resolves inside the job's root: an image's
// /var/run -> /run puts the act path at /run/act in the root, never on the
// agent's own filesystem, even when the link names a host path.
func TestMakeDirStaysInTheRoot(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	root := filepath.Join(outside, "root")
	// The host path abs names exists in the root too.
	for _, d := range []string{"root/var", "root/run", "root" + outside} {
		if err := os.MkdirAll(filepath.Join(outside, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/run", filepath.Join(root, "var/run")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "abs")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/var/run/act", "/abs/x"} {
		if err := (kernel{}).makeDir(root, p, 0o777); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	wantDir(t, filepath.Join(root, "run/act"), 0o777)
	wantDir(t, filepath.Join(root, outside, "x"), 0o777)
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("made %v outside the root", entries)
	}
}

// A parent that is a file in the image fails, naming it.
func TestMakeDirRefusesAFileAbove(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "opt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := (kernel{}).makeDir(root, "/opt/hostedtoolcache", 0o777)
	if err == nil || !strings.Contains(err.Error(), "/opt/hostedtoolcache") || !strings.Contains(err.Error(), "/opt is not a directory") {
		t.Errorf("err = %v, want one naming /opt/hostedtoolcache and /opt", err)
	}
}
