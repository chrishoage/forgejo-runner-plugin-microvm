// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// owners returns the ids the tests give copied entries: ids of no user's
// when the test runs as root, which only root can give, and the test's own
// otherwise, which CopyIn then needs no privilege to keep.
func owners(t *testing.T) (uid, gid int) {
	t.Helper()
	if os.Geteuid() == 0 {
		return 4242, 4243
	}
	t.Log("not root: owners are the test's own ids, so changing them is not tested")
	return os.Getuid(), os.Getgid()
}

// userXattrs says whether dir's filesystem takes user extended attributes,
// which tmpfs has only since Linux 6.6.
func userXattrs(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, ".xattr-probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(probe) }()
	err := lsetxattr(probe, "user.probe", []byte("x"))
	if errors.Is(err, syscall.ENOTSUP) {
		t.Logf("%s takes no user xattrs: extended attributes are not tested", dir)
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

// writeTar returns the tar of hdrs, each regular file's body its name.
// A header naming no owner gets the test's own ids, so an unprivileged
// CopyIn can keep them.
func writeTar(t *testing.T, hdrs ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, hdr := range hdrs {
		if hdr.Uid == 0 && hdr.Gid == 0 && hdr.Typeflag != tar.TypeXGlobalHeader {
			hdr.Uid, hdr.Gid = os.Getuid(), os.Getgid()
		}
		body := ""
		if hdr.Typeflag == tar.TypeReg {
			body = hdr.Name
			hdr.Size = int64(len(body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// metadataTar is a tree of every entry type CopyIn takes, with modes the
// umask would narrow and special bits, owners, times, hard links and
// extended attributes, each time a distinct whole second. Its directory ro/
// is read-only and holds a file, so CopyIn can set a directory's mode only
// after what it holds is written.
func metadataTar(t *testing.T, uid, gid int, xattrs bool) []*tar.Header {
	t.Helper()
	at := func(s int64) time.Time { return time.Unix(1_700_000_000+s, 0) }
	pax := func(kv ...string) map[string]string {
		if !xattrs {
			return nil
		}
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m["SCHILY.xattr."+kv[i]] = kv[i+1]
		}
		return m
	}
	return []*tar.Header{
		{Name: "tree/", Typeflag: tar.TypeDir, Mode: 0o750, Uid: uid, Gid: gid, ModTime: at(1), PAXRecords: pax("user.dir", "d")},
		{Name: "tree/tool", Typeflag: tar.TypeReg, Mode: 0o4755, Uid: uid, Gid: gid, ModTime: at(2), PAXRecords: pax("user.one", "1", "user.bin", "a\x00b")},
		{Name: "tree/data", Typeflag: tar.TypeReg, Mode: 0o666, Uid: uid, Gid: gid, ModTime: at(3)},
		{Name: "tree/again", Typeflag: tar.TypeLink, Linkname: "tree/tool", Uid: uid, Gid: gid, ModTime: at(2)},
		{Name: "tree/link", Typeflag: tar.TypeSymlink, Linkname: "data", Mode: 0o777, Uid: uid, Gid: gid, ModTime: at(4)},
		{Name: "tree/ro/", Typeflag: tar.TypeDir, Mode: 0o555, Uid: uid, Gid: gid, ModTime: at(5)},
		{Name: "tree/ro/inside", Typeflag: tar.TypeReg, Mode: 0o444, Uid: uid, Gid: gid, ModTime: at(6)},
	}
}

// writable gives dir's tree back the write bit a read-only directory took,
// so the test's temporary directory can be removed.
func writable(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
}

// wantMetadata fails t unless the entry at p has hdr's type, mode, owner,
// modification time and extended attributes.
func wantMetadata(t *testing.T, p string, hdr *tar.Header) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Error(err)
		return
	}
	want := hdr.FileInfo().Mode()
	if hdr.Typeflag == tar.TypeSymlink {
		want = fs.ModeSymlink | 0o777 // a symlink's own mode is always 0777
	}
	if fi.Mode() != want {
		t.Errorf("%s: mode %v, want %v", hdr.Name, fi.Mode(), want)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if int(st.Uid) != hdr.Uid || int(st.Gid) != hdr.Gid {
		t.Errorf("%s: owner %d:%d, want %d:%d", hdr.Name, st.Uid, st.Gid, hdr.Uid, hdr.Gid)
	}
	if !fi.ModTime().Equal(hdr.ModTime) {
		t.Errorf("%s: mtime %v, want %v", hdr.Name, fi.ModTime(), hdr.ModTime)
	}
	got, err := lxattrs(p)
	if err != nil {
		t.Error(err)
		return
	}
	for k, v := range hdr.PAXRecords {
		name, ok := cutXattr(k)
		if ok && got[name] != v {
			t.Errorf("%s: xattr %s = %q, want %q (all: %q)", hdr.Name, name, got[name], v, got)
		}
	}
}

// CopyIn keeps each entry's mode, special bits included, its owner, its
// modification time and its extended attributes (GST-10). A directory's
// mode and time are its header's though entries were written into it
// after, and a read-only directory still gets its contents.
func TestCopyInKeepsMetadata(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "dest")
	writable(t, dest)
	uid, gid := owners(t)
	hdrs := metadataTar(t, uid, gid, userXattrs(t, filepath.Dir(dest)))
	if err := copyIn(t, startAgent(t), dest, writeTar(t, hdrs...)); err != nil {
		t.Fatal(err)
	}
	for _, hdr := range hdrs {
		if hdr.Typeflag == tar.TypeLink {
			continue // the link's target's metadata, checked under its own name
		}
		wantMetadata(t, filepath.Join(dest, hdr.Name), hdr)
	}
	tool, err := os.Lstat(filepath.Join(dest, "tree/tool"))
	if err != nil {
		t.Fatal(err)
	}
	if again, err := os.Lstat(filepath.Join(dest, "tree/again")); err != nil || !os.SameFile(tool, again) {
		t.Errorf("tree/again is not a hard link to tree/tool: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "tree/ro/inside")); err != nil || string(b) != "tree/ro/inside" {
		t.Errorf("tree/ro/inside = %q, %v", b, err)
	}
}

// An access time in the archive is kept; without one, the access time is
// the modification time.
func TestCopyInAccessTime(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "dest")
	mtime, atime := time.Unix(1_700_000_000, 0), time.Unix(1_700_000_500, 0)
	err := copyIn(t, startAgent(t), dest, writeTar(t,
		&tar.Header{Name: "with", Typeflag: tar.TypeReg, Mode: 0o644, ModTime: mtime, AccessTime: atime, Format: tar.FormatPAX},
		&tar.Header{Name: "without", Typeflag: tar.TypeReg, Mode: 0o644, ModTime: mtime},
	))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]time.Time{"with": atime, "without": mtime} {
		fi, err := os.Stat(filepath.Join(dest, name))
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if got := time.Unix(st.Atim.Unix()); !got.Equal(want) {
			t.Errorf("%s: atime %v, want %v", name, got, want)
		}
	}
}

// A PAX global header, which git archive writes first, carries no entry.
func TestCopyInGlobalHeader(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "dest")
	err := copyIn(t, startAgent(t), dest, writeTar(t,
		&tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "a commit"}},
		&tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0o644},
	))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "file")); err != nil {
		t.Error(err)
	}
}

// recordedCopyIn is the plugin's side of a CopyIn: Recv returns reqs, then
// io.EOF, and SendAndClose records whether Recv had reached it.
type recordedCopyIn struct {
	grpc.ServerStream
	ctx          context.Context
	reqs         []*agentv1.CopyInRequest
	atEOF        bool
	closedAtEOF  bool
	closedCalled bool
}

func (s *recordedCopyIn) Context() context.Context { return s.ctx }

func (s *recordedCopyIn) Recv() (*agentv1.CopyInRequest, error) {
	if len(s.reqs) == 0 {
		s.atEOF = true
		return nil, io.EOF
	}
	req := s.reqs[0]
	s.reqs = s.reqs[1:]
	return req, nil
}

func (s *recordedCopyIn) SendAndClose(*agentv1.CopyInResponse) error {
	s.closedCalled, s.closedAtEOF = true, s.atEOF
	return nil
}

// An archive padded after its end-of-archive marker to a whole 10 KiB
// record, as GNU tar and bsdtar write it, with the padding in a later
// chunk, is read to its end before CopyIn answers: the runner sends all of
// it, and an answer before its last send fails that send.
func TestCopyInReadsPaddingAfterTheArchive(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "dest")
	archive := writeTar(t, &tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0o644})
	const record = 10 << 10
	stream := &recordedCopyIn{ctx: t.Context(), reqs: []*agentv1.CopyInRequest{
		{Destination: dest, Chunk: archive},
		{Chunk: make([]byte, record-len(archive)%record)},
	}}
	if err := testServer(t).CopyIn(stream); err != nil {
		t.Fatal(err)
	}
	if !stream.closedCalled || !stream.closedAtEOF {
		t.Errorf("CopyIn answered before reading the padding to the stream's end (answered: %v)", stream.closedCalled)
	}
	if _, err := os.Stat(filepath.Join(dest, "file")); err != nil {
		t.Error(err)
	}
}

// A name or hard-link target whose `..` stays inside the destination is
// the path it cleans to, and a ./ prefix names the destination's own.
func TestCopyInTakesDotDotThatStaysInside(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "dest")
	err := copyIn(t, startAgent(t), dest, writeTar(t,
		&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "a/../b", Typeflag: tar.TypeReg, Mode: 0o644},
		&tar.Header{Name: "./c", Typeflag: tar.TypeLink, Linkname: "d/../b"},
	))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(filepath.Join(dest, "b"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := os.Stat(filepath.Join(dest, "c"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(b, c) {
		t.Error("c is not a hard link to b")
	}
}

// CopyIn refuses an entry or a hard link that would land outside the
// destination, through `..`, an absolute name, or a symlink in the archive
// or one already at the destination, relative or absolute, and leaves
// nothing outside it, nor writes the refused entry. It
// never replaces a directory, in the archive or already at the
// destination, with a non-directory (GST-10).
func TestCopyInRefusesEscapesAndReplacingDirectories(t *testing.T) {
	t.Parallel()
	c := startAgent(t)
	for name, tt := range map[string]struct {
		// existing makes what the destination dest, inside top, holds
		// before the copy.
		existing func(t *testing.T, top, dest string)
		hdrs     func(top string) []*tar.Header
		// kept is a path under dest that must be as existing left it.
		kept string
		// absent is a path under dest the copy must not make.
		absent string
	}{
		"file named ../x": {hdrs: func(string) []*tar.Header {
			return []*tar.Header{{Name: "../x", Typeflag: tar.TypeReg, Mode: 0o644}}
		}, absent: "x"},
		"file named a/../../b": {hdrs: func(string) []*tar.Header {
			return []*tar.Header{{Name: "a/../../b", Typeflag: tar.TypeReg, Mode: 0o644}}
		}, absent: "b"},
		"file with an absolute name": {hdrs: func(string) []*tar.Header {
			return []*tar.Header{{Name: "/x", Typeflag: tar.TypeReg, Mode: 0o644}}
		}, absent: "x"},
		"hard link to ../../secret, with a secret in the destination": {
			existing: func(t *testing.T, _, dest string) { writeFile(t, filepath.Join(dest, "secret")) },
			hdrs: func(string) []*tar.Header {
				return []*tar.Header{{Name: "escaped", Typeflag: tar.TypeLink, Linkname: "../../secret"}}
			},
		},
		"hard link to an absolute target, with a secret in the destination": {
			existing: func(t *testing.T, _, dest string) { writeFile(t, filepath.Join(dest, "secret")) },
			hdrs: func(string) []*tar.Header {
				return []*tar.Header{{Name: "escaped", Typeflag: tar.TypeLink, Linkname: "/secret"}}
			},
		},
		"hard link over an existing file, to ../../secret": {
			existing: func(t *testing.T, _, dest string) {
				writeFile(t, filepath.Join(dest, "secret"))
				writeFile(t, filepath.Join(dest, "f"))
			},
			hdrs: func(string) []*tar.Header {
				return []*tar.Header{{Name: "f", Typeflag: tar.TypeLink, Linkname: "../../secret"}}
			},
			kept: "f",
		},
		"file through the archive's relative symlink": {hdrs: func(string) []*tar.Header {
			return []*tar.Header{
				{Name: "up", Typeflag: tar.TypeSymlink, Linkname: "../.."},
				{Name: "up/escaped", Typeflag: tar.TypeReg, Mode: 0o644},
			}
		}},
		"file through the archive's absolute symlink": {hdrs: func(top string) []*tar.Header {
			return []*tar.Header{
				{Name: "abs", Typeflag: tar.TypeSymlink, Linkname: top},
				{Name: "abs/escaped", Typeflag: tar.TypeReg, Mode: 0o644},
			}
		}},
		// An absolute symlink is refused even when it points inside the
		// destination: os.Root takes every absolute link as leaving it.
		"file through the archive's absolute symlink into the destination": {hdrs: func(top string) []*tar.Header {
			return []*tar.Header{
				{Name: "real/", Typeflag: tar.TypeDir, Mode: 0o755},
				{Name: "in", Typeflag: tar.TypeSymlink, Linkname: filepath.Join(top, "a", "dest", "real")},
				{Name: "in/escaped", Typeflag: tar.TypeReg, Mode: 0o644},
			}
		}, absent: "real/escaped"},
		"file through an absolute symlink at the destination into it": {
			existing: func(t *testing.T, _, dest string) {
				mkdir(t, filepath.Join(dest, "real"))
				symlink(t, filepath.Join(dest, "real"), filepath.Join(dest, "in"))
			},
			hdrs: func(string) []*tar.Header {
				return []*tar.Header{{Name: "in/escaped", Typeflag: tar.TypeReg, Mode: 0o644}}
			},
			absent: "real/escaped",
		},
		"directory through the archive's symlink": {hdrs: func(string) []*tar.Header {
			return []*tar.Header{
				{Name: "up", Typeflag: tar.TypeSymlink, Linkname: "../.."},
				{Name: "up/escaped/", Typeflag: tar.TypeDir, Mode: 0o755},
			}
		}},
		"symlink through the archive's symlink": {hdrs: func(string) []*tar.Header {
			return []*tar.Header{
				{Name: "up", Typeflag: tar.TypeSymlink, Linkname: "../.."},
				{Name: "up/escaped", Typeflag: tar.TypeSymlink, Linkname: "anything"},
			}
		}},
		"file through a symlink at the destination": {
			existing: func(t *testing.T, _, dest string) { symlink(t, "../..", filepath.Join(dest, "up")) },
			hdrs: func(string) []*tar.Header {
				return []*tar.Header{{Name: "up/escaped", Typeflag: tar.TypeReg, Mode: 0o644}}
			},
		},
		"file under a symlink at the destination, its parent missing": {
			existing: func(t *testing.T, _, dest string) { symlink(t, "../..", filepath.Join(dest, "up")) },
			hdrs: func(string) []*tar.Header {
				return []*tar.Header{{Name: "up/escaped/deeper", Typeflag: tar.TypeReg, Mode: 0o644}}
			},
		},
		"hard link to a file outside": {hdrs: func(string) []*tar.Header {
			return []*tar.Header{{Name: "escaped", Typeflag: tar.TypeLink, Linkname: "../../secret"}}
		}},
		"hard link through the archive's symlink": {hdrs: func(string) []*tar.Header {
			return []*tar.Header{
				{Name: "up", Typeflag: tar.TypeSymlink, Linkname: "../.."},
				{Name: "escaped", Typeflag: tar.TypeLink, Linkname: "up/secret"},
			}
		}},
		"hard link through a symlink at the destination": {
			existing: func(t *testing.T, _, dest string) { symlink(t, "../..", filepath.Join(dest, "up")) },
			hdrs: func(string) []*tar.Header {
				return []*tar.Header{{Name: "escaped", Typeflag: tar.TypeLink, Linkname: "up/secret"}}
			},
		},
		"directory by a file in the archive": {hdrs: func(string) []*tar.Header {
			return []*tar.Header{
				{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755},
				{Name: "d", Typeflag: tar.TypeReg, Mode: 0o644},
			}
		}},
		"directory at the destination by a file": {
			existing: func(t *testing.T, _, dest string) { mkdir(t, filepath.Join(dest, "d", "keep")) },
			hdrs: func(string) []*tar.Header {
				return []*tar.Header{{Name: "d", Typeflag: tar.TypeReg, Mode: 0o644}}
			},
			kept: "d/keep",
		},
		"directory at the destination by a symlink": {
			existing: func(t *testing.T, _, dest string) { mkdir(t, filepath.Join(dest, "d", "keep")) },
			hdrs: func(string) []*tar.Header {
				return []*tar.Header{{Name: "d", Typeflag: tar.TypeSymlink, Linkname: "elsewhere"}}
			},
			kept: "d/keep",
		},
		"directory at the destination by a hard link": {
			existing: func(t *testing.T, _, dest string) {
				mkdir(t, filepath.Join(dest, "d", "keep"))
				if err := os.WriteFile(filepath.Join(dest, "f"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			hdrs: func(string) []*tar.Header {
				return []*tar.Header{{Name: "d", Typeflag: tar.TypeLink, Linkname: "f"}}
			},
			kept: "d/keep",
		},
	} {
		t.Run(name, func(t *testing.T) {
			top := t.TempDir()
			dest := filepath.Join(top, "a", "dest")
			mkdir(t, dest)
			if err := os.WriteFile(filepath.Join(top, "secret"), []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tt.existing != nil {
				tt.existing(t, top, dest)
			}
			err := copyIn(t, c, dest, writeTar(t, tt.hdrs(top)...))
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("CopyIn = %v, want InvalidArgument", err)
			}
			entries, err := os.ReadDir(top)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Name() != "a" && e.Name() != "secret" {
					t.Errorf("%s landed outside the destination", e.Name())
				}
			}
			if _, err := os.Lstat(filepath.Join(dest, "escaped")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("escaped is in the destination: %v", err)
			}
			if tt.kept != "" {
				if _, err := os.Stat(filepath.Join(dest, tt.kept)); err != nil {
					t.Errorf("the destination's %s is gone: %v", tt.kept, err)
				}
			}
			if tt.absent != "" {
				if _, err := os.Lstat(filepath.Join(dest, tt.absent)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s is in the destination: %v", tt.absent, err)
				}
			}
		})
	}
}

// An absolute symlink on the destination's own path, above or at it, is
// followed, as the job's root resolves it; one the archive holds is written
// with its target as it is; and one already at the destination that an
// entry names is replaced, never followed. Only an absolute symlink inside
// the destination on an entry's way is refused
// (TestCopyInRefusesEscapesAndReplacingDirectories).
func TestCopyInAbsoluteSymlinks(t *testing.T) {
	t.Parallel()
	c := startAgent(t)
	top := t.TempDir()
	mkdir(t, filepath.Join(top, "real"))
	symlink(t, filepath.Join(top, "real"), filepath.Join(top, "link"))
	writeFile(t, filepath.Join(top, "secret"))
	dest := filepath.Join(top, "link", "dest")
	mkdir(t, dest)
	symlink(t, filepath.Join(top, "secret"), filepath.Join(dest, "replaced"))
	err := copyIn(t, c, dest, writeTar(t,
		&tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0o644},
		&tar.Header{Name: "abs", Typeflag: tar.TypeSymlink, Linkname: "/nowhere"},
		&tar.Header{Name: "replaced", Typeflag: tar.TypeReg, Mode: 0o644},
	))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(top, "real", "dest", "file")); err != nil {
		t.Errorf("the destination's absolute symlink was not followed: %v", err)
	}
	if got, err := os.Readlink(filepath.Join(dest, "abs")); err != nil || got != "/nowhere" {
		t.Errorf("the archive's absolute symlink is %q, %v; want /nowhere", got, err)
	}
	if fi, err := os.Lstat(filepath.Join(dest, "replaced")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("the symlink an entry names was not replaced by a file: %v, %v", fi, err)
	}
	if b, err := os.ReadFile(filepath.Join(top, "secret")); err != nil || string(b) != filepath.Join(top, "secret") {
		t.Errorf("the replaced symlink's target was written: %q, %v", b, err)
	}
}

// A directory's mode and times, set once the whole archive is written, go
// to the directory its entry made, through no symlink in the destination: a
// later entry that replaces a symlink on its path leaves it unreachable, and
// it is skipped, never set elsewhere or failing the copy.
func TestCopyInDirectoryModesFollowNoSymlink(t *testing.T) {
	t.Parallel()
	c := startAgent(t)
	for name, tt := range map[string]struct {
		hdrs []*tar.Header
		// untouched is a directory whose mode must not be the entry's.
		untouched string
	}{
		"symlink replaced by a file": {hdrs: []*tar.Header{
			{Name: "a/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "a"},
			{Name: "l/x/", Typeflag: tar.TypeDir, Mode: 0o751},
			{Name: "l", Typeflag: tar.TypeReg, Mode: 0o644},
		}},
		"symlink replaced by a symlink elsewhere": {hdrs: []*tar.Header{
			{Name: "a/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "b/x/f", Typeflag: tar.TypeReg, Mode: 0o644},
			{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "a"},
			{Name: "l/x/", Typeflag: tar.TypeDir, Mode: 0o751},
			{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "b"},
		}, untouched: "b/x"},
	} {
		t.Run(name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "dest")
			if err := copyIn(t, c, dest, writeTar(t, tt.hdrs...)); err != nil {
				t.Fatalf("CopyIn = %v", err)
			}
			if tt.untouched == "" {
				return
			}
			fi, err := os.Stat(filepath.Join(dest, tt.untouched))
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() == 0o751 {
				t.Errorf("%s got l/x's mode %v", tt.untouched, fi.Mode())
			}
		})
	}
}

// A CopyIn that fails partway still gives the directories it made their
// own mode and times, not the 0700 they are made with.
func TestCopyInFailedSetsDirectoryModes(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "dest")
	mtime := time.Unix(1_700_000_000, 0)
	err := copyIn(t, startAgent(t), dest, writeTar(t,
		&tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: mtime},
		&tar.Header{Name: "d/f", Typeflag: tar.TypeReg, Mode: 0o644, ModTime: mtime},
		&tar.Header{Name: "d/fifo", Typeflag: tar.TypeFifo, Mode: 0o644},
	))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CopyIn = %v, want InvalidArgument", err)
	}
	fi, err := os.Stat(filepath.Join(dest, "d"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 || !fi.ModTime().Equal(mtime) {
		t.Errorf("d: mode %v, mtime %v; want 0755, %v", fi.Mode(), fi.ModTime(), mtime)
	}
}

func symlink(t *testing.T, target, p string) {
	t.Helper()
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(p), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}
