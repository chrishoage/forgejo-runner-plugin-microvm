// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// openRoot opens dir as resolveDir's root.
func openRoot(t *testing.T, dir string) int {
	t.Helper()
	fd, err := syscall.Open(dir, oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	return fd
}

// fdPath returns the path an O_PATH descriptor names, and closes it.
func fdPath(t *testing.T, fd int) string {
	t.Helper()
	defer func() { _ = syscall.Close(fd) }()
	p, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A scratch path the image has resolves to it; one it lacks is created with
// its missing parents, mode 0755 (GST-13).
func TestResolveDirFindsOrCreates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "var/lib/docker"), 0o711); err != nil {
		t.Fatal(err)
	}
	rfd := openRoot(t, root)
	fd, err := resolveDir(rfd, "/var/lib/docker", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fdPath(t, fd), filepath.Join(root, "var/lib/docker"); got != want {
		t.Errorf("resolved %s, want %s", got, want)
	}
	fd, err = resolveDir(rfd, "/opt/a/b", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fdPath(t, fd), filepath.Join(root, "opt/a/b"); got != want {
		t.Errorf("resolved %s, want %s", got, want)
	}
	for _, d := range []string{"opt", "opt/a", "opt/a/b"} {
		fi, err := os.Stat(filepath.Join(root, d))
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o755 {
			t.Errorf("/%s: %v, %v; want a directory of mode 0755", d, fi, err)
		}
	}
}

// Symlinks resolve inside the job's root: an absolute one lands at its
// target in the root, even one naming a host path, and `..` never climbs
// out of it.
func TestResolveDirStaysInTheRoot(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	root := filepath.Join(outside, "root")
	// The host path abs names exists in the root too.
	for _, d := range []string{"root/run", "root/var", "root" + outside} {
		if err := os.MkdirAll(filepath.Join(outside, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		"var/run": "/run",
		"up":      "../../..",
		"abs":     outside,
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	rfd := openRoot(t, root)
	for guest, want := range map[string]string{
		"/var/run/docker": "run/docker",
		"/up/escaped":     "escaped",
		"/abs/root/x":     outside[1:] + "/root/x",
	} {
		fd, err := resolveDir(rfd, guest, 0)
		if err != nil {
			t.Errorf("%s: %v", guest, err)
			continue
		}
		if got := fdPath(t, fd); got != filepath.Join(root, want) {
			t.Errorf("%s resolved to %s, want %s", guest, got, filepath.Join(root, want))
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

// A scratch path that is a file in the image, or under one, or a symlink to
// a file or to nothing, fails, naming the part that is.
func TestResolveDirRefusesANonDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"tofile": "/file", "dangling": "/gone/x"} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	rfd := openRoot(t, root)
	for guest, want := range map[string]string{
		"/file":     "/file is not a directory in the image",
		"/file/x":   "/file is not a directory in the image",
		"/tofile":   "/tofile is not a directory in the image",
		"/dangling": "create /dangling: file exists",
	} {
		fd, err := resolveDir(rfd, guest, 0)
		if err == nil {
			_ = syscall.Close(fd)
			t.Errorf("%s resolved", guest)
			continue
		}
		if err.Error() != want {
			t.Errorf("%s: %v, want %s", guest, err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "gone")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a dangling symlink's target was made: %v", err)
	}
}

// resolveScratch returns where a path lands in the root, symlinks
// resolved, with the image's owner and mode there, and names the path in
// its errors.
func TestResolveScratch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real/sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Chmod(filepath.Join(root, "real/sub"), 0o1750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/real", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for guest, want := range map[string]scratchTarget{
		"/link/sub": {resolved: "/real/sub", mode: 0o1750},
		"/real":     {resolved: "/real", mode: 0o755},
		"/new/deep": {resolved: "/new/deep", mode: 0o755},
	} {
		want.guestPath, want.uid, want.gid = guest, os.Getuid(), os.Getgid()
		got, err := kernel{}.resolveScratch(root, guest)
		if err != nil || got != want {
			t.Errorf("%s: %+v, %v; want %+v", guest, got, err, want)
		}
	}
	_, err := kernel{}.resolveScratch(root, "/file/x")
	if want := "scratch path /file/x: /file is not a directory in the image"; err == nil || err.Error() != want {
		t.Errorf("/file/x: %v, want %s", err, want)
	}
}

// A scratch path never crosses onto another mount, so no image symlink can
// take it into the API mounts: here /proc is one, in the host's root.
func TestResolveScratchStaysOnTheRootsMount(t *testing.T) {
	t.Parallel()
	_, err := kernel{}.resolveScratch("/", "/proc/x")
	if want := "scratch path /proc/x: /proc crosses a mount: a scratch path can't be in /proc, /sys or /dev"; err == nil || err.Error() != want {
		t.Errorf("/proc/x: %v, want %s", err, want)
	}
}

// The fresh directory gets the owner and every mode bit it is given, the
// umask notwithstanding, and must not exist already.
func TestFreshDir(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "0")
	if err := freshDir(dir, os.Getuid(), os.Getgid(), 0o1777); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		t.Fatal(err)
	}
	if st.Mode&0o7777 != 0o1777 || int(st.Uid) != os.Getuid() || int(st.Gid) != os.Getgid() {
		t.Errorf("mode %o, owner %d:%d; want 1777, %d:%d", st.Mode&0o7777, st.Uid, st.Gid, os.Getuid(), os.Getgid())
	}
	if err := freshDir(dir, os.Getuid(), os.Getgid(), 0o755); err == nil {
		t.Error("an existing directory was taken as fresh")
	}
}
