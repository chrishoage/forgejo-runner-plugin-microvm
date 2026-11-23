// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

// maxLinks is how many symlinks makeDir follows to nothing in one path, as
// many as the kernel follows in one lookup.
const maxLinks = 40

// makeDir creates guestPath in the job's root at root, root-owned, with
// mode, when the image has nothing there, and leaves whatever the image has
// as it is: a directory of its own mode, a file. Its missing parents are
// created 0755. It resolves inside root, as a scratch path does, so a
// symlink in the image, such as /var/run -> /run, lands at its target in
// the job's root, never on the agent's own filesystem; a symlink to nothing
// is followed, and its target created, with its missing parents. A
// directory that lands in /proc, /sys or /dev, or would be created there,
// fails, naming where.
func (kernel) makeDir(root, guestPath string, mode uint32) error {
	rootFd, err := syscall.Open(root, oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("directory %s: open the root %s: %w", guestPath, root, err)
	}
	defer func() { _ = syscall.Close(rootFd) }()
	if err := makeDirIn(rootFd, guestPath, mode); err != nil {
		return fmt.Errorf("directory %s: %w", guestPath, err)
	}
	return nil
}

// makeDirIn is makeDir in the directory rootFd. It walks p a part at a
// time, each looked up from rootFd inside it, so every symlink resolves
// inside the root. A missing part is created; a symlink to nothing starts
// the walk again at its target, which, when relative, is taken from where
// the link's directory really is.
func makeDirIn(rootFd int, p string, mode uint32) error {
	parts := pathParts(p)
	parent := rootFd
	closeParent := func() {
		if parent != rootFd {
			_ = syscall.Close(parent)
		}
		parent = rootFd
	}
	defer closeParent()
	for i, links := 0, 0; i < len(parts); {
		prefix := "/" + strings.Join(parts[:i+1], "/")
		last := i == len(parts)-1
		fd, err := openInRoot(rootFd, prefix[1:], 0)
		if errors.Is(err, syscall.ENOENT) {
			target, isLink, err := readlinkIn(parent, parts[i])
			if err != nil {
				return fmt.Errorf("%s: %w", prefix, err)
			}
			if isLink {
				if links++; links > maxLinks {
					return fmt.Errorf("%s: %w", prefix, syscall.ELOOP)
				}
				if !strings.HasPrefix(target, "/") {
					dir, err := placeOf(rootFd, parent)
					if err != nil {
						return fmt.Errorf("%s: %w", prefix, err)
					}
					target = dir + "/" + target
				}
				parts = append(pathParts(target), parts[i+1:]...)
				closeParent()
				i = 0
				continue
			}
			if err := createIn(rootFd, parent, parts[i], last, mode); err != nil {
				return err
			}
			// Open what was just made on the next turn.
			continue
		}
		if err != nil {
			return fmt.Errorf("%s: %w", prefix, err)
		}
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			_ = syscall.Close(fd)
			return fmt.Errorf("%s: %w", prefix, err)
		}
		if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			_ = syscall.Close(fd)
			if last {
				// The image has something else there.
				return nil
			}
			return fmt.Errorf("%s is not a directory in the image", prefix)
		}
		closeParent()
		parent = fd
		i++
	}
	// The image has the directory, or the walk made it: where it lands
	// decides.
	at, err := placeOf(rootFd, parent)
	if err != nil {
		return err
	}
	if api, ok := inAPIMount(at); ok {
		return fmt.Errorf("lands at %s, in %s", at, api)
	}
	return nil
}

// createIn creates name in the directory parent, root-owned: with mode when
// it is the last part, else 0755. It refuses when parent lands in /proc,
// /sys or /dev. Its errors name where it creates.
func createIn(rootFd, parent int, name string, last bool, mode uint32) error {
	dir, err := placeOf(rootFd, parent)
	if err != nil {
		return err
	}
	at := strings.TrimSuffix(dir, "/") + "/" + name
	if api, ok := inAPIMount(at); ok {
		return fmt.Errorf("lands at %s, in %s", at, api)
	}
	if !last {
		mode = 0o755
	}
	if err := syscall.Mkdirat(parent, name, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", at, err)
	}
	// Set explicitly, so the umask can't drop a bit such as the temp
	// directory's sticky bit.
	if err := syscall.Fchmodat(parent, name, mode, 0); err != nil {
		return fmt.Errorf("create %s: chmod %o: %w", at, mode, err)
	}
	return nil
}

// placeOf returns where the directory fd is in the directory rootFd, "/"
// for rootFd itself.
func placeOf(rootFd, fd int) (string, error) {
	if fd == rootFd {
		return "/", nil
	}
	return pathIn(rootFd, fd)
}

// inAPIMount returns the API mount root, /proc, /sys or /dev, that p, an
// absolute, clean path, is in or names.
func inAPIMount(p string) (string, bool) {
	for _, api := range apiMountRoots {
		if p == api || strings.HasPrefix(p, api+"/") {
			return api, true
		}
	}
	return "", false
}

// readlinkIn reads name in the directory dirFd, an O_PATH descriptor, and
// reports whether it is a symlink. A name that isn't there is not one.
func readlinkIn(dirFd int, name string) (string, bool, error) {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return "", false, err
	}
	buf := make([]byte, syscall.PathMax)
	//nolint:gosec // G103: readlinkat(2) takes a C string and a buffer; both outlive the call.
	n, _, errno := syscall.Syscall6(syscall.SYS_READLINKAT, uintptr(dirFd), uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
	switch {
	case errno == syscall.ENOENT:
		return "", false, nil
	case errno != 0:
		return "", false, fmt.Errorf("readlink: %w", errno)
	case int(n) == len(buf):
		return "", false, fmt.Errorf("readlink: %w", syscall.ENAMETOOLONG)
	}
	return string(buf[:n]), true, nil
}

// pathParts splits p into its parts, dropping empty ones and ".", keeping
// "..", which the kernel resolves where it stands.
func pathParts(p string) []string {
	var parts []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return parts
}
