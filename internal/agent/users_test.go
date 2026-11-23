// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// An image's users and groups, for the tests that resolve them.
const (
	imagePasswd = "root:x:0:0:root:/root:/bin/sh\n" +
		"builder:x:1000:1000:Builder:/home/builder:/bin/sh\n" +
		"nohome:x:1001:1001::::\n"
	imageGroup = "root:x:0:root\n" +
		"wheel:x:10:root,builder\n" +
		"staff:x:50:builder\n" +
		"builder:x:1000:\n"
)

// Users resolve by runc's rules against the image's /etc/passwd and
// /etc/group (GST-5).
func TestResolveUser(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeUsers(t, root, imagePasswd, imageGroup)
	for _, tt := range []struct {
		spec string
		want identity
	}{
		// Root gets its supplementary groups.
		{"", identity{uid: 0, gid: 0, groups: []uint32{0, 10}, home: "/root"}},
		{"root", identity{uid: 0, gid: 0, groups: []uint32{0, 10}, home: "/root"}},
		{"builder", identity{uid: 1000, gid: 1000, groups: []uint32{10, 50}, home: "/home/builder"}},
		{"1000", identity{uid: 1000, gid: 1000, groups: []uint32{10, 50}, home: "/home/builder"}},
		// An explicit group replaces the user's own and its supplementary
		// groups.
		{"builder:staff", identity{uid: 1000, gid: 50, home: "/home/builder"}},
		{"1000:50", identity{uid: 1000, gid: 50, home: "/home/builder"}},
		{"builder:4343", identity{uid: 1000, gid: 4343, home: "/home/builder"}},
		// A numeric uid with no passwd entry gets gid 0 and home /.
		{"4242", identity{uid: 4242, gid: 0, home: "/"}},
		{"4242:staff", identity{uid: 4242, gid: 50, home: "/"}},
		// An entry with no home gets /.
		{"nohome", identity{uid: 1001, gid: 1001, home: "/"}},
	} {
		t.Run(tt.spec, func(t *testing.T) {
			got, err := resolveUser(root, tt.spec)
			if err != nil {
				t.Fatal(err)
			}
			if got.uid != tt.want.uid || got.gid != tt.want.gid || !slices.Equal(got.groups, tt.want.groups) || got.home != tt.want.home {
				t.Errorf("resolveUser(%q) = %+v, want %+v", tt.spec, got, tt.want)
			}
		})
	}
	for _, spec := range []string{"nobody", "builder:nogroup", "-1"} {
		if got, err := resolveUser(root, spec); err == nil {
			t.Errorf("resolveUser(%q) = %+v, want an error", spec, got)
		}
	}
}

// An image with no /etc/passwd or /etc/group runs as root, with home /, or
// as a numeric id; a name can't resolve.
func TestResolveUserWithoutPasswd(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	got, err := resolveUser(root, "")
	if err != nil || got.uid != 0 || got.gid != 0 || len(got.groups) != 0 || got.home != "/" {
		t.Errorf("resolveUser(\"\") = %+v, %v, want root with home /", got, err)
	}
	got, err = resolveUser(root, "4242:7")
	if err != nil || got.uid != 4242 || got.gid != 7 || got.home != "/" {
		t.Errorf("resolveUser(\"4242:7\") = %+v, %v", got, err)
	}
	if _, err := resolveUser(root, "builder"); err == nil || !strings.Contains(err.Error(), "builder") {
		t.Errorf("resolveUser(\"builder\") = %v, want an error naming builder", err)
	}
}

// A missing working directory is created with its missing parents, mode
// 0755 despite the umask, owned by the exec user; an existing one keeps its
// mode. Something on the path that is not a directory fails (GST-6).
func TestMakeWorkDir(t *testing.T) {
	t.Parallel()
	me := identity{uid: uint32(os.Getuid()), gid: uint32(os.Getgid())}
	base := t.TempDir()
	existing := filepath.Join(base, "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(existing, "a", "b")
	if err := makeWorkDir(dir, me); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(existing, "a"), dir} {
		wantOwnedDir(t, p, 0o755, me.uid, me.gid)
	}
	wantOwnedDir(t, existing, 0o700, me.uid, me.gid)
	if err := makeWorkDir(existing, me); err != nil {
		t.Fatal(err)
	}
	wantOwnedDir(t, existing, 0o700, me.uid, me.gid)

	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{file, filepath.Join(file, "dir"), "relative/dir"} {
		if err := makeWorkDir(d, me); err == nil {
			t.Errorf("makeWorkDir(%s) succeeded", d)
		}
	}
}

// A directory makeWorkDir made, swapped for a symlink before it is re-owned,
// as a step's background process could, fails the call, and the symlink is
// never followed: its target keeps its mode, and nothing is made in it.
// An existing symlink on the path is followed.
func TestMakeWorkDirDoesNotFollowASwappedDirectory(t *testing.T) {
	t.Parallel()
	me := identity{uid: uint32(os.Getuid()), gid: uint32(os.Getgid())}
	base := t.TempDir()
	secret := filepath.Join(base, "secret")
	if err := os.WriteFile(secret, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	swap := func(target string) func(string) {
		return func(p string) {
			if err := os.Remove(p); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(target, p); err != nil {
				t.Error(err)
			}
		}
	}
	// The last directory, swapped for a link to a file.
	if err := makeDirs(filepath.Join(base, "work"), me, swap(secret)); err == nil {
		t.Error("makeDirs succeeded with the directory swapped for a link to a file")
	}
	if fi, err := os.Stat(secret); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the link's target: %v, %v, want mode 0600", fi, err)
	}
	// A parent, swapped for a link to a directory.
	if err := makeDirs(filepath.Join(base, "parent", "child"), me, swap(elsewhere)); err == nil {
		t.Error("makeDirs succeeded with a parent swapped for a link to a directory")
	}
	wantOwnedDir(t, elsewhere, 0o700, me.uid, me.gid)
	if entries, err := os.ReadDir(elsewhere); err != nil || len(entries) != 0 {
		t.Errorf("the link's target holds %v, %v, want nothing", entries, err)
	}
	// A symlink that was there before is the image's, and followed.
	if err := os.Symlink(elsewhere, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if err := makeWorkDir(filepath.Join(base, "link", "made"), me); err != nil {
		t.Fatal(err)
	}
	wantOwnedDir(t, filepath.Join(elsewhere, "made"), 0o755, me.uid, me.gid)
}

// wantOwnedDir fails t unless p is a directory with mode and owner uid:gid.
func wantOwnedDir(t *testing.T, p string, mode os.FileMode, uid, gid uint32) {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s: no owner", p)
	}
	if !fi.IsDir() || fi.Mode().Perm() != mode || st.Uid != uid || st.Gid != gid {
		t.Errorf("%s: %v, owner %d:%d, want a directory %v, owner %d:%d", p, fi.Mode(), st.Uid, st.Gid, mode, uid, gid)
	}
}

// requireRoot skips t unless the test runs as root: changing ids needs it.
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("changes ids: runs only as root")
	}
}

// Exec runs as the request's user, else the image's, with that user's ids,
// supplementary groups and HOME, and creates a missing working directory
// owned by that user, leaving the existing directories above it as they are
// (GST-5, GST-6).
func TestExecAsUser(t *testing.T) {
	t.Parallel()
	requireRoot(t)
	s := testServer(t)
	writeUsers(t, s.root, imagePasswd, imageGroup)
	s.image = &agentv1.Image{User: "builder", DefaultPath: os.Getenv("PATH")}
	c := startServer(t, s)
	// Every user the test runs as must reach base: the test's temporary
	// directories are 0700.
	base := t.TempDir()
	for _, d := range []string{filepath.Dir(base), base} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const script = `echo "$(id -u) $(id -g) $(id -G | tr ' ' '\n' | sort -n | tr '\n' ' ')$HOME $(pwd)"`
	for _, tt := range []struct {
		name, user, dir string
		want            string
		owner           [2]uint32
	}{
		{"the image's user", "", filepath.Join(base, "image", "work"), "1000 1000 10 50 1000 /home/builder", [2]uint32{1000, 1000}},
		{"the request's user", "4242", filepath.Join(base, "request", "work"), "4242 0 0 /", [2]uint32{4242, 0}},
		{"a group", "builder:staff", filepath.Join(base, "group", "work"), "1000 50 50 /home/builder", [2]uint32{1000, 50}},
		{"root", "root", filepath.Join(base, "root", "work"), "0 0 0 10 /root", [2]uint32{0, 0}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, err := runExec(testContext(t), t, c, &agentv1.ExecRequest{Argv: []string{"sh", "-c", script}, User: tt.user, WorkingDir: tt.dir})
			if err != nil {
				t.Fatal(err)
			}
			if want := tt.want + " " + tt.dir + "\n"; r.stdout != want || r.code != 0 {
				t.Errorf("Exec = %+v, want stdout %q", r, want)
			}
			wantOwnedDir(t, tt.dir, 0o755, tt.owner[0], tt.owner[1])
			wantOwnedDir(t, filepath.Dir(tt.dir), 0o755, tt.owner[0], tt.owner[1])
		})
	}
	// An existing directory is never re-owned.
	wantOwnedDir(t, base, 0o755, 0, 0)
	r, err := runExec(testContext(t), t, c, &agentv1.ExecRequest{Argv: []string{"true"}, WorkingDir: base})
	if err != nil || r.code != 0 || r.startError != "" {
		t.Fatalf("Exec in an existing directory = %+v, %v", r, err)
	}
	wantOwnedDir(t, base, 0o755, 0, 0)
}
