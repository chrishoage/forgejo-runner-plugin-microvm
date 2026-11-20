// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
)

// extract writes a flattened image filesystem, as a tar, into dir, which it
// creates. The archive is untrusted: every entry and link target is
// resolved through an os.Root, so nothing lands outside dir, whatever the
// entries' names, symlinks or hard links say.
//
// SHORTCUT(#5): owners, special mode bits, extended attributes, device
// nodes and FIFOs are dropped, and sizes are taken on trust. The EROFS
// conversion keeps them all.
func extract(r io.Reader, dir string) error {
	if err := os.Mkdir(dir, 0o755); err != nil { //nolint:gosec // the VM's /, which every guest user must traverse
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	// Directories are made writable while extracting, and get their own
	// modes once everything under them is written.
	dirModes := map[string]fs.FileMode{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := entryPath(hdr.Name)
		if name == "." {
			continue
		}
		if err := extractEntry(root, tr, hdr, name, dirModes); err != nil {
			return fmt.Errorf("%s: %w", hdr.Name, err)
		}
	}
	// Deepest first, so a directory that loses its write bit has nothing
	// left to change under it.
	names := make([]string, 0, len(dirModes))
	for name := range dirModes {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range slices.Backward(names) {
		if err := root.Chmod(name, dirModes[name]); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// entryPath returns name as a path relative to the root, with every `..`
// that would leave it dropped, as an image's root has no parent.
func entryPath(name string) string {
	p := path.Clean("/" + name)
	if p == "/" {
		return "."
	}
	return p[1:]
}

func extractEntry(root *os.Root, tr *tar.Reader, hdr *tar.Header, name string, dirModes map[string]fs.FileMode) error {
	if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	perm := hdr.FileInfo().Mode().Perm()
	if fi, err := root.Lstat(name); err == nil {
		if fi.IsDir() && hdr.Typeflag == tar.TypeDir {
			dirModes[name] = perm
			return nil
		}
		// Whatever is there is replaced. It may be a directory only when
		// the entry is not one.
		if fi.IsDir() {
			for d := range dirModes {
				if d == name || strings.HasPrefix(d, name+"/") {
					delete(dirModes, d)
				}
			}
		}
		if err := root.RemoveAll(name); err != nil {
			return err
		}
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		dirModes[name] = perm
		return root.Mkdir(name, 0o755)
	case tar.TypeReg:
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		return errors.Join(err, f.Close())
	case tar.TypeSymlink:
		// The target is never resolved here, only stored.
		return root.Symlink(hdr.Linkname, name)
	case tar.TypeLink:
		return root.Link(entryPath(hdr.Linkname), name)
	default:
		return nil
	}
}
