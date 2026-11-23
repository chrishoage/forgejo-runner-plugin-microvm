// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"errors"
	"fmt"
	"path"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

// The system calls and flags a scratch path needs that the syscall package
// doesn't name. Since Linux 5.1 new system calls have one number on every
// architecture, and O_PATH is the same on x86_64 and aarch64.
const (
	sysOpenTree = 428
	sysOpenat2  = 437

	oPath               = 0x200000
	openTreeClone       = 0x1
	moveMountTEmptyPath = 0x40
	resolveNoXdev       = 0x01
	resolveNoMagiclinks = 0x02
	resolveInRoot       = 0x10
)

// openHow is openat2(2)'s struct open_how.
type openHow struct {
	flags, mode, resolve uint64
}

// resolveScratch resolves guestPath inside the job's root at root, with
// root as the boundary, creating it and any missing parents root-owned, mode
// 0755, when the image lacks it. It never leaves the root's own mount, so no
// symlink can take it into the API mounts (GST-13).
func (kernel) resolveScratch(root, guestPath string) (scratchTarget, error) {
	rootFd, err := syscall.Open(root, oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return scratchTarget{}, fmt.Errorf("scratch path %s: open the root %s: %w", guestPath, root, err)
	}
	defer func() { _ = syscall.Close(rootFd) }()
	fd, err := resolveDir(rootFd, guestPath, resolveNoXdev)
	if err != nil {
		return scratchTarget{}, fmt.Errorf("scratch path %s: %w", guestPath, err)
	}
	defer func() { _ = syscall.Close(fd) }()
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return scratchTarget{}, fmt.Errorf("scratch path %s: %w", guestPath, err)
	}
	resolved, err := pathIn(rootFd, fd)
	if err != nil {
		return scratchTarget{}, fmt.Errorf("scratch path %s: %w", guestPath, err)
	}
	return scratchTarget{
		guestPath: guestPath, resolved: resolved,
		uid: int(st.Uid), gid: int(st.Gid), mode: st.Mode & 0o7777,
	}, nil
}

// bindScratch makes dir, which must not exist, with t's owner and mode, and
// bind-mounts it over t's place in the job's root at root, hiding what the
// image has there. It looks the place up again, inside root, since a scratch
// path bound above it since it was resolved now hides the image's copy; a
// missing part of it is created, mode 0755.
func (kernel) bindScratch(root, dir string, t scratchTarget) error {
	rootFd, err := syscall.Open(root, oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("scratch path %s: open the root %s: %w", t.guestPath, root, err)
	}
	defer func() { _ = syscall.Close(rootFd) }()
	target, err := resolveDir(rootFd, t.resolved, 0)
	if err != nil {
		return fmt.Errorf("scratch path %s: at %s: %w", t.guestPath, t.resolved, err)
	}
	defer func() { _ = syscall.Close(target) }()
	if err := freshDir(dir, t.uid, t.gid, t.mode); err != nil {
		return fmt.Errorf("scratch path %s: %w", t.guestPath, err)
	}
	if err := bindOnto(dir, target); err != nil {
		return fmt.Errorf("scratch path %s: bind %s over it: %w", t.guestPath, dir, err)
	}
	return nil
}

// resolveDir returns an O_PATH descriptor of the directory guestPath, an
// absolute, clean path, resolved inside the directory rootFd with that as
// the boundary: an absolute symlink, or `..`, never leaves it. resolve adds
// openat2 RESOLVE_ flags. A missing part of the path is created, mode 0755.
// Anything on the path that isn't a directory is an error naming that part.
func resolveDir(rootFd int, guestPath string, resolve uint64) (int, error) {
	rel := strings.TrimPrefix(path.Clean("/"+guestPath), "/")
	if rel == "" {
		return -1, fmt.Errorf("is the root")
	}
	parts := strings.Split(rel, "/")
	parent := rootFd
	closeParent := func() {
		if parent != rootFd {
			_ = syscall.Close(parent)
		}
	}
	defer closeParent()
	for i, name := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		fd, err := openInRoot(rootFd, prefix, resolve)
		if errors.Is(err, syscall.ENOENT) {
			if err := syscall.Mkdirat(parent, name, 0o755); err != nil {
				return -1, fmt.Errorf("create /%s: %w", prefix, err)
			}
			// The umask must not narrow the mode.
			if err := syscall.Fchmodat(parent, name, 0o755, 0); err != nil {
				return -1, fmt.Errorf("create /%s: %w", prefix, err)
			}
			fd, err = openInRoot(rootFd, prefix, resolve)
		}
		if errors.Is(err, syscall.EXDEV) {
			return -1, fmt.Errorf("/%s crosses a mount: a scratch path can't be in /proc, /sys or /dev", prefix)
		}
		if err != nil {
			return -1, fmt.Errorf("/%s: %w", prefix, err)
		}
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			_ = syscall.Close(fd)
			return -1, fmt.Errorf("/%s: %w", prefix, err)
		}
		if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			_ = syscall.Close(fd)
			return -1, fmt.Errorf("/%s is not a directory in the image", prefix)
		}
		closeParent()
		parent = fd
	}
	fd := parent
	parent = rootFd // the deferred close leaves fd open
	return fd, nil
}

// pathIn returns the path of the directory fd inside the directory rootFd,
// with symlinks resolved, as getcwd(2) names them: on a thread of its own,
// with its own working directory, so no other goroutine's relative path
// moves.
func pathIn(rootFd, fd int) (string, error) {
	type result struct {
		path string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		// Never unlocked: the thread, with its own working directory,
		// ends with the goroutine.
		runtime.LockOSThread()
		cwdOf := func(fd int) (string, error) {
			if err := syscall.Fchdir(fd); err != nil {
				return "", err
			}
			return syscall.Getwd()
		}
		var r result
		r.err = syscall.Unshare(syscall.CLONE_FS)
		var root string
		if r.err == nil {
			root, r.err = cwdOf(rootFd)
		}
		if r.err == nil {
			r.path, r.err = cwdOf(fd)
		}
		switch {
		case r.err != nil:
			r.err = fmt.Errorf("find its place in the root: %w", r.err)
		case root == "/":
		case r.path == root:
			r.path = "/"
		case strings.HasPrefix(r.path, root+"/"):
			r.path = strings.TrimPrefix(r.path, root)
		default:
			r.err = fmt.Errorf("lands at %s, outside the root %s", r.path, root)
		}
		done <- r
	}()
	r := <-done
	return r.path, r.err
}

// openInRoot opens rel, relative to rootFd, as an O_PATH descriptor,
// resolving every symlink inside rootFd (openat2's RESOLVE_IN_ROOT), with
// the extra RESOLVE_ flags resolve.
func openInRoot(rootFd int, rel string, resolve uint64) (int, error) {
	how := openHow{flags: oPath | syscall.O_CLOEXEC, resolve: resolveInRoot | resolveNoMagiclinks | resolve}
	p, err := syscall.BytePtrFromString(rel)
	if err != nil {
		return -1, err
	}
	for {
		//nolint:gosec // G103: openat2(2) takes a C string and a struct; both outlive the call.
		fd, _, errno := syscall.Syscall6(sysOpenat2, uintptr(rootFd), uintptr(unsafe.Pointer(p)),
			uintptr(unsafe.Pointer(&how)), unsafe.Sizeof(how), 0, 0)
		// EAGAIN: a rename elsewhere raced the lookup; nothing else runs
		// at boot, so it can't repeat for long.
		if errno == syscall.EINTR || errno == syscall.EAGAIN {
			continue
		}
		if errno != 0 {
			return -1, errno
		}
		return int(fd), nil
	}
}

// freshDir makes dir, which must not exist, owned by uid and gid, with mode:
// set after the owner, since a chown clears the set-id bits, and explicitly,
// so the umask can't narrow it.
func freshDir(dir string, uid, gid int, mode uint32) error {
	if err := syscall.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("make %s: %w", dir, err)
	}
	if err := syscall.Chown(dir, uid, gid); err != nil {
		return fmt.Errorf("chown %s: %w", dir, err)
	}
	if err := syscall.Chmod(dir, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	return nil
}

// bindOnto bind-mounts dir over the directory target, an O_PATH descriptor,
// with open_tree and move_mount, so the mount lands on the directory
// resolved inside the job's root, never on a path looked up again.
func bindOnto(dir string, target int) error {
	d, empty, cwd := cString(dir), cString(""), atFdcwd
	//nolint:gosec // G103: open_tree(2) takes a C string; d outlives the call.
	tree, _, errno := syscall.Syscall(sysOpenTree, uintptr(cwd), uintptr(unsafe.Pointer(d)),
		openTreeClone|syscall.O_CLOEXEC)
	if errno != 0 {
		return fmt.Errorf("open_tree: %w", errno)
	}
	defer func() { _ = syscall.Close(int(tree)) }()
	//nolint:gosec // G103: move_mount(2) takes C strings; both outlive the call.
	_, _, errno = syscall.Syscall6(sysMoveMount, tree, uintptr(unsafe.Pointer(empty)), uintptr(target),
		uintptr(unsafe.Pointer(empty)), moveMountFEmptyPath|moveMountTEmptyPath, 0)
	if errno != 0 {
		return fmt.Errorf("move_mount: %w", errno)
	}
	return nil
}
