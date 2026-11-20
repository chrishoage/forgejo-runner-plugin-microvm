// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"archive/tar"
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// copyChunk is the size of CopyOut's chunks.
const copyChunk = 256 << 10

// CopyIn extracts a tar at the destination, creating it and its missing
// parents, also for an empty archive (GST-10). No entry or link lands
// outside the destination, and a directory is never replaced by a
// non-directory.
//
// SHORTCUT(#6): modes are kept, owners and times are not.
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
	if err := extract(r, dest); err != nil {
		if _, ok := status.FromError(err); ok {
			return err
		}
		return status.Errorf(codes.InvalidArgument, "copy into %s: %v", dest, err)
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

// extract writes the tar in r under dest, through an os.Root.
func extract(r io.Reader, dest string) error {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean("/" + hdr.Name)[1:]
		if name == "" {
			continue
		}
		if err := extractEntry(root, tr, hdr, name); err != nil {
			return fmt.Errorf("%s: %w", hdr.Name, err)
		}
	}
}

func extractEntry(root *os.Root, tr *tar.Reader, hdr *tar.Header, name string) error {
	if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	perm := hdr.FileInfo().Mode().Perm()
	fi, err := root.Lstat(name)
	exists := err == nil
	if exists && fi.IsDir() {
		if hdr.Typeflag != tar.TypeDir {
			return errors.New("would replace a directory")
		}
		return root.Chmod(name, perm)
	}
	if exists {
		if err := root.Remove(name); err != nil {
			return err
		}
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		return root.Mkdir(name, perm)
	case tar.TypeReg:
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		err = errors.Join(err, f.Close())
		if err == nil {
			// The umask must not narrow a mode the runner chose.
			err = root.Chmod(name, perm)
		}
		return err
	case tar.TypeSymlink:
		return root.Symlink(hdr.Linkname, name)
	case tar.TypeLink:
		return root.Link(path.Clean("/" + hdr.Linkname)[1:], name)
	default:
		return fmt.Errorf("entry type %q is not supported", hdr.Typeflag)
	}
}

// CopyOut returns the path as a tar, entries named relative to its parent.
// A missing path returns an empty tar (GST-11).
//
// SHORTCUT(#6): hard links and extended attributes are not kept.
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
		if err := archive(tw, src); err != nil {
			return status.Errorf(codes.Internal, "copy out %s: %v", src, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return status.Errorf(codes.FailedPrecondition, "copy out %s: %v", src, err)
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return w.Flush()
}

// sendChunks is a writer that sends a copy of each write.
type sendChunks func([]byte) error

func (f sendChunks) Write(p []byte) (int, error) {
	if err := f(append([]byte(nil), p...)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// archive writes src and, for a directory, everything under it to tw.
func archive(tw *tar.Writer, src string) error {
	parent := filepath.Dir(src)
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
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
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
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
}
