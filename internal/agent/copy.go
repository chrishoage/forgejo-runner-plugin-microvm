// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"archive/tar"
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// copyChunk is the size of CopyOut's chunks.
const copyChunk = 256 << 10

// CopyIn extracts a tar at the destination, creating it and its missing
// parents, also for an empty archive, and keeping each entry's mode, owner,
// times and extended attributes (GST-10). No entry or link lands outside
// the destination, and a directory is never replaced by a non-directory.
func (s *server) CopyIn(stream agentv1.AgentService_CopyInServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	dest := first.GetDestination()
	if !filepath.IsAbs(dest) {
		return status.Errorf(codes.InvalidArgument, "destination %q is not absolute", dest)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil { //nolint:gosec // G301: the job's directories, for every user
		return status.Errorf(codes.FailedPrecondition, "destination %s: %v", dest, err)
	}
	r := &chunkReader{next: first.GetChunk(), recv: func() ([]byte, error) {
		req, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		if req.GetDestination() != "" {
			return nil, status.Error(codes.InvalidArgument, "destination set after the first request")
		}
		return req.GetChunk(), nil
	}}
	if err := extract(r, dest, s.log); err != nil {
		if _, ok := status.FromError(err); ok {
			return err
		}
		return status.Errorf(codes.InvalidArgument, "copy into %s: %v", dest, err)
	}
	// The tar reader stops at the end-of-archive marker, but GNU tar and
	// bsdtar pad an archive to a whole record, 10 KiB by default, and the
	// runner sends all of it: ending the call first fails the runner's next
	// send. So the rest of the stream is read to its end, as long as the
	// call runs, and discarded, as Docker does.
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	return stream.SendAndClose(&agentv1.CopyInResponse{})
}

// chunkReader reads a stream of chunks as one byte stream.
type chunkReader struct {
	next []byte
	recv func() ([]byte, error)
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for len(r.next) == 0 {
		b, err := r.recv()
		if err != nil {
			return 0, err
		}
		r.next = b
	}
	n := copy(p, r.next)
	r.next = r.next[n:]
	return n, nil
}

// extract writes the tar in r under dest, through an os.Root, so no entry,
// symlink or hard link reaches outside it, whatever the archive or dest
// already holds. A directory's mode and times are set once the whole
// archive is written: a write into a directory changes its modification
// time, and a read-only one must still take what it holds. They are set
// for the directories made so far when the copy fails, too, so none is
// left at the 0700 it is made with.
func extract(r io.Reader, dest string, log *slog.Logger) error {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	x := &extractor{root: root, dirs: map[string]*tar.Header{}}
	if err := x.entries(tar.NewReader(r)); err != nil {
		_ = x.finishDirs() // best effort: the copy's own error is the one to return
		return err
	}
	if err := x.finishDirs(); err != nil {
		return err
	}
	if x.skipped > 0 {
		log.Warn("extended attributes not kept", "dest", dest, "count", x.skipped, "first", x.firstSkip)
	}
	return nil
}

// extractor writes one archive's entries under root.
type extractor struct {
	root *os.Root
	// dirs is each directory's last header, by name, and order the names
	// in the order they first came, for finishDirs.
	dirs  map[string]*tar.Header
	order []string
	// skipped counts the extended attributes the kernel refused as
	// unsupported or not permitted, and firstSkip is the first's error.
	skipped   int
	firstSkip error
}

// entries writes each of tr's entries.
func (x *extractor) entries(tr *tar.Reader) error {
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue // a comment for the whole archive, such as git archive's commit
		}
		name, err := inside(hdr.Name)
		if err != nil {
			return fmt.Errorf("%s: %w", hdr.Name, err)
		}
		if name == "" {
			continue
		}
		if err := x.entry(tr, hdr, name); err != nil {
			return fmt.Errorf("%s: %w", hdr.Name, err)
		}
	}
}

// inside returns name, cleaned, as a path relative to the destination, ""
// for the destination itself, or an error when it is absolute or `..` takes
// it out of the destination. A name whose `..` stays inside, such as
// a/../b, is that path, as Docker takes it.
func inside(name string) (string, error) {
	if path.IsAbs(name) {
		return "", errors.New("is absolute")
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("leaves the destination")
	}
	if clean == "." {
		return "", nil
	}
	return clean, nil
}

// entry writes one entry, name being its header's name, cleaned. A hard
// link's target is checked before anything is written.
func (x *extractor) entry(tr *tar.Reader, hdr *tar.Header, name string) error {
	var target string
	if hdr.Typeflag == tar.TypeLink {
		var err error
		if target, err = inside(hdr.Linkname); err != nil {
			return fmt.Errorf("hard link target %q: %w", hdr.Linkname, err)
		}
		if target == "" {
			return errors.New("hard link to the destination itself")
		}
	}
	if err := x.root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	fi, err := x.root.Lstat(name)
	exists := err == nil
	if exists && fi.IsDir() {
		if hdr.Typeflag != tar.TypeDir {
			return errors.New("would replace a directory")
		}
		return x.dir(hdr, name)
	}
	if exists {
		if err := x.root.Remove(name); err != nil {
			return err
		}
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		// Writable by its owner until finishDirs sets its own mode.
		if err := x.root.Mkdir(name, 0o700); err != nil {
			return err
		}
		return x.dir(hdr, name)
	case tar.TypeReg:
		return x.file(tr, hdr, name)
	case tar.TypeSymlink:
		if err := x.root.Symlink(hdr.Linkname, name); err != nil {
			return err
		}
		return x.symlink(hdr, name)
	case tar.TypeLink:
		// A hard link shares its target's inode, and so its metadata.
		return x.root.Link(target, name)
	default:
		return fmt.Errorf("entry type %q is not supported", hdr.Typeflag)
	}
}

// mode is the header's permission and special bits, which the umask never
// narrows because they are set explicitly, after the owner, whose change
// would clear the set-id bits.
func mode(hdr *tar.Header) fs.FileMode {
	return hdr.FileInfo().Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

// accessTime is the header's access time, or its modification time when
// the archive carries none.
func accessTime(hdr *tar.Header) time.Time {
	if hdr.AccessTime.IsZero() {
		return hdr.ModTime
	}
	return hdr.AccessTime
}

// file writes a regular file and sets its owner, mode, extended attributes
// and times, all through its own descriptor: in that order, since a change
// of owner clears the set-id bits and file capabilities.
func (x *extractor) file(tr *tar.Reader, hdr *tar.Header, name string) error {
	f, err := x.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = func() error {
		if _, err := io.Copy(f, tr); err != nil {
			return err
		}
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		if !ownedBy(fi, hdr) {
			if err := f.Chown(hdr.Uid, hdr.Gid); err != nil {
				return err
			}
		}
		if err := f.Chmod(mode(hdr)); err != nil {
			return err
		}
		if err := x.xattrs(hdr, func(k string, v []byte) error { return fsetxattr(f.Fd(), k, v) }); err != nil {
			return err
		}
		return utimensat(f.Fd(), "", accessTime(hdr), hdr.ModTime)
	}()
	return errors.Join(err, f.Close())
}

// dir sets a directory's owner and extended attributes, and records its
// header for finishDirs.
func (x *extractor) dir(hdr *tar.Header, name string) error {
	if _, ok := x.dirs[name]; !ok {
		x.order = append(x.order, name)
	}
	x.dirs[name] = hdr
	d, err := x.root.Open(name)
	if err != nil {
		return err
	}
	err = func() error {
		fi, err := d.Stat()
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return errors.New("is no longer a directory")
		}
		if !ownedBy(fi, hdr) {
			if err := d.Chown(hdr.Uid, hdr.Gid); err != nil {
				return err
			}
		}
		return x.xattrs(hdr, func(k string, v []byte) error { return fsetxattr(d.Fd(), k, v) })
	}()
	return errors.Join(err, d.Close())
}

// finishDirs sets each directory's mode and times, deepest first, so a
// parent's modification time is set after its children's. Each is reached
// from the root one component at a time, following no symlink: a later
// entry may have replaced a symlink on its path, and its mode and times
// must land on no other directory. One no longer reachable that way is
// skipped. A failure doesn't stop the rest; the first is returned.
func (x *extractor) finishDirs() error {
	top, err := x.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = top.Close() }()
	var first error
	for _, name := range slices.Backward(x.order) {
		hdr := x.dirs[name]
		if err := finishDir(top, name, hdr); err != nil && first == nil {
			first = fmt.Errorf("%s: %w", hdr.Name, err)
		}
	}
	return first
}

// finishDir sets the mode and times of the directory name under top, or
// nothing when a component of name is now missing, a symlink or not a
// directory.
func finishDir(top *os.File, name string, hdr *tar.Header) error {
	d, err := openDirNoFollow(top, name)
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
		return nil
	}
	if err != nil {
		return err
	}
	err = d.Chmod(mode(hdr))
	if err == nil {
		err = utimensat(d.Fd(), "", accessTime(hdr), hdr.ModTime)
	}
	return errors.Join(err, d.Close())
}

// openDirNoFollow opens the directory name, relative to top, one component
// at a time through openat with O_NOFOLLOW and O_DIRECTORY, as makeDirs
// does, holding no more than two descriptors of its own at once.
func openDirNoFollow(top *os.File, name string) (*os.File, error) {
	const flags = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	fd, owned := int(top.Fd()), false
	for part := range strings.SplitSeq(name, "/") {
		next, err := syscall.Openat(fd, part, flags, 0)
		if owned {
			_ = syscall.Close(fd)
		}
		if err != nil {
			return nil, &os.PathError{Op: "openat", Path: name, Err: err}
		}
		fd, owned = next, true
	}
	return os.NewFile(uintptr(fd), name), nil
}

// symlink sets a symlink's owner, extended attributes and times, never
// following it: through its parent's descriptor, opened in the root.
func (x *extractor) symlink(hdr *tar.Header, name string) error {
	fi, err := x.root.Lstat(name)
	if err != nil {
		return err
	}
	if !ownedBy(fi, hdr) {
		if err := x.root.Lchown(name, hdr.Uid, hdr.Gid); err != nil {
			return err
		}
	}
	parent, err := x.root.Open(path.Dir(name))
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	base := path.Base(name)
	if err := x.xattrs(hdr, func(k string, v []byte) error { return lsetxattr(inDir(parent.Fd(), base), k, v) }); err != nil {
		return err
	}
	return utimensat(parent.Fd(), base, accessTime(hdr), hdr.ModTime)
}

// xattrs sets the header's extended attributes with set. One the kernel
// refuses as unsupported or not permitted, such as a trusted attribute
// without the privilege or any on a filesystem without them, is counted
// and skipped, as Docker does: it says nothing about the files.
func (x *extractor) xattrs(hdr *tar.Header, set func(name string, value []byte) error) error {
	for k, v := range hdr.PAXRecords {
		name, ok := cutXattr(k)
		if !ok {
			continue
		}
		err := set(name, []byte(v))
		if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EPERM) {
			if x.skipped == 0 {
				x.firstSkip = fmt.Errorf("%s: %s: %w", hdr.Name, name, err)
			}
			x.skipped++
			continue
		}
		if err != nil {
			return fmt.Errorf("extended attribute %s: %w", name, err)
		}
	}
	return nil
}

// ownedBy says whether fi already has the header's owner, so CopyIn changes
// ids only when they differ: in the VM the agent is root and takes any, and
// on the host its tests run unprivileged.
func ownedBy(fi fs.FileInfo, hdr *tar.Header) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == hdr.Uid && int(st.Gid) == hdr.Gid
}

// CopyOut returns the path as a tar, entries named relative to its parent,
// with hard links and extended attributes kept. A missing path returns an
// empty tar, two zero blocks (GST-11).
func (s *server) CopyOut(req *agentv1.CopyOutRequest, stream agentv1.AgentService_CopyOutServer) error {
	src := req.GetPath()
	if !filepath.IsAbs(src) {
		return status.Errorf(codes.InvalidArgument, "path %q is not absolute", src)
	}
	w := bufio.NewWriterSize(sendChunks(func(b []byte) error {
		return stream.Send(&agentv1.CopyOutResponse{Chunk: b})
	}), copyChunk)
	tw := tar.NewWriter(w)
	if _, err := os.Lstat(src); err == nil {
		skipped, firstSkip, err := archive(tw, src)
		if err != nil {
			return copyOutError(src, err)
		}
		if skipped > 0 {
			s.log.Warn("extended attributes not kept", "path", src, "count", skipped, "first", firstSkip)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return status.Errorf(codes.FailedPrecondition, "copy out %s: %v", src, err)
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return w.Flush()
}

// copyOutError is the status of a CopyOut of src that failed with err: the
// stream's own status when a send failed; FailedPrecondition when the
// guest's files failed to read, by a permission or I/O error or a file cut
// short while it was copied, which are the job's doing, not the agent's;
// else Internal, a bug in the agent.
func copyOutError(src string, err error) error {
	code := codes.Internal
	if s, ok := status.FromError(err); ok {
		code = s.Code()
	} else if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.As(err, new(*fs.PathError)) || errors.As(err, new(syscall.Errno)) {
		code = codes.FailedPrecondition
	}
	return status.Errorf(code, "copy out %s: %v", src, err)
}

// sendChunks is a writer that sends a copy of each write, in chunks of at
// most copyChunk: a buffered writer passes a large write straight through.
type sendChunks func([]byte) error

func (f sendChunks) Write(p []byte) (int, error) {
	for n := 0; n < len(p); n += copyChunk {
		if err := f(append([]byte(nil), p[n:min(n+copyChunk, len(p))]...)); err != nil {
			return n, err
		}
	}
	return len(p), nil
}

// fileID is a file's identity, which its hard links share.
type fileID struct{ dev, ino uint64 }

// archive writes src and, for a directory, everything under it to tw. A
// regular file reached again through another hard link is written as a
// link to the first name, and each entry's extended attributes go in its
// PAX records. Times are kept to the second, with no access or change
// time, as Docker's archives do: they would otherwise change every read.
// A socket has no tar form and is left out. So is an extended attribute
// whose name holds "=", which Linux allows and a PAX record can't: archive
// counts them, and returns the first.
func archive(tw *tar.Writer, src string) (skipped int, firstSkip error, err error) {
	parent := filepath.Dir(src)
	links := map[fileID]string{}
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSocket != 0 {
			return nil
		}
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(parent, p)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		hdr.ModTime = hdr.ModTime.Truncate(time.Second)
		hdr.AccessTime, hdr.ChangeTime = time.Time{}, time.Time{}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && fi.Mode().IsRegular() && st.Nlink > 1 {
			id := fileID{dev: st.Dev, ino: st.Ino}
			if first, ok := links[id]; ok {
				hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeLink, first, 0
			} else {
				links[id] = hdr.Name
			}
		}
		attrs, err := lxattrs(p)
		if err != nil {
			return fmt.Errorf("extended attributes of %s: %w", p, err)
		}
		for k, v := range attrs {
			if strings.Contains(k, "=") {
				if skipped == 0 {
					firstSkip = fmt.Errorf("%s: %q: a PAX record's key can't hold \"=\"", hdr.Name, k)
				}
				skipped++
				continue
			}
			if hdr.PAXRecords == nil {
				hdr.PAXRecords = map[string]string{}
			}
			hdr.PAXRecords[paxXattr+k] = v
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil
		}
		f, err := os.Open(p) //nolint:gosec // G304: the path the runner asked for, inside the guest
		if err != nil {
			return err
		}
		// A file that grows meanwhile is cut at the size in its header.
		_, err = io.CopyN(tw, f, hdr.Size)
		return errors.Join(err, f.Close())
	})
	return skipped, firstSkip, err
}

// paxXattr prefixes an extended attribute's name in a tar's PAX records.
const paxXattr = "SCHILY.xattr."

// cutXattr returns the extended attribute a PAX record's key names.
func cutXattr(key string) (string, bool) {
	return strings.CutPrefix(key, paxXattr)
}
