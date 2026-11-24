// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"fmt"
	"syscall"
	"unsafe"
)

// fsmount(2)'s mount attributes, which the syscall package doesn't name.
const (
	mountAttrRdonly = 0x1
	mountAttrNosuid = 0x2
	mountAttrNodev  = 0x4
)

// resolveTarget finds where the mount or disk target guestPath lands in the
// job's root at root, with root as the boundary: an absolute symlink in the
// image, such as /var/run -> /run, lands at its target inside the root,
// never on the agent's own filesystem, and `..` never climbs out. A target
// the image lacks is created with any missing parents, root-owned, mode
// 0755; one the image has as anything but a directory fails, naming it, and
// so does one that lands in /proc, /sys or /dev. It crosses mounts, so a
// target under a scratch path lands on the scratch path's fresh directory
// (STO-5, STO-6).
func (kernel) resolveTarget(root, guestPath string) (string, error) {
	rootFd, err := syscall.Open(root, oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("open the root %s: %w", root, err)
	}
	defer func() { _ = syscall.Close(rootFd) }()
	fd, place, err := walkDir(rootFd, guestPath, 0o755, true)
	if err != nil {
		return "", err
	}
	_ = syscall.Close(fd)
	return place, nil
}

// mountShare mounts the virtiofs share tag, nosuid and nodev, and read-only
// when readOnly, at place in the job's root at root, where a target landed.
// It looks place up again, inside root, and mounts onto the directory it
// finds by descriptor (fsmount and move_mount), never on a path looked up
// again. A lookup that lands anywhere but place fails, naming both: no
// mount lands under another (makeMounts), so none should, but a symlink in
// a share mounted above it, such as sub -> /, would otherwise redirect the
// mount past the checks place passed. A host directory's files carry
// host owners and modes, so the guest gains no privilege from them; the host
// enforces read-only too, so remounting the share read-write, or mounting
// the tag again, can't write it (STO-2).
func (kernel) mountShare(root, tag, place string, readOnly bool) (err error) {
	rootFd, err := syscall.Open(root, oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open the root %s: %w", root, err)
	}
	defer func() { _ = syscall.Close(rootFd) }()
	target, at, err := walkDir(rootFd, place, 0o755, true)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(target) }()
	if at != place {
		return fmt.Errorf("%s now lands at %s", place, at)
	}
	fd, err := fsopen("virtiofs")
	if err != nil {
		return fmt.Errorf("fsopen virtiofs: %w", err)
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("virtiofs %s: %w%s", tag, err, fsMessages(fd))
		}
		_ = syscall.Close(fd)
	}()
	if err := fsconfigString(fd, "source", tag); err != nil {
		return fmt.Errorf("source: %w", err)
	}
	if _, _, errno := syscall.Syscall6(sysFsconfig, uintptr(fd), fsconfigCmdCreate, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("create: %w", errno)
	}
	attrs := uintptr(mountAttrNosuid | mountAttrNodev)
	if readOnly {
		attrs |= mountAttrRdonly
	}
	mfd, _, errno := syscall.Syscall(sysFsmount, uintptr(fd), fsmountCloexec, attrs)
	if errno != 0 {
		return fmt.Errorf("fsmount: %w", errno)
	}
	defer func() { _ = syscall.Close(int(mfd)) }()
	empty := cString("")
	//nolint:gosec // G103: move_mount(2) takes C strings; empty outlives the call.
	_, _, errno = syscall.Syscall6(sysMoveMount, mfd, uintptr(unsafe.Pointer(empty)), uintptr(target),
		uintptr(unsafe.Pointer(empty)), moveMountFEmptyPath|moveMountTEmptyPath, 0)
	if errno != 0 {
		return fmt.Errorf("move_mount: %w", errno)
	}
	return nil
}
