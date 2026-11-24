// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// The new mount API's system calls, which the syscall package doesn't name
// and the agent may not import golang.org/x/sys for. Since Linux 5.1 new
// system calls have one number on every architecture.
const (
	sysMoveMount = 429
	sysFsopen    = 430
	sysFsconfig  = 431
	sysFsmount   = 432

	fsopenCloexec        = 0x1
	fsconfigSetFlag      = 0
	fsconfigSetString    = 1
	fsconfigCmdCreate    = 6
	fsmountCloexec       = 0x1
	moveMountFEmptyPath  = 0x4
	atFdcwd              = -0x64
	fsContextMessageSize = 1024
)

// kernel is the real mounter.
type kernel struct{}

// mkdirAll makes mount points, also in the job's root, which every guest
// user must traverse.
func (kernel) mkdirAll(path string) error {
	return os.MkdirAll(path, 0o755) //nolint:gosec // G301: mount points every guest user traverses
}

func (kernel) mount(source, target, fstype string, flags uintptr, data string) error {
	if err := syscall.Mount(source, target, fstype, flags, data); err != nil {
		return fmt.Errorf("mount %s %s on %s: %w", fstype, source, target, err)
	}
	return nil
}

// overlay builds the overlay with fsopen and one lowerdir+ per layer: unlike
// mount(2)'s option string, it has no page-size limit on the layer list.
// lowerdir+ needs Linux 6.8 or later.
func (kernel) overlay(lowers []string, upper, work, target string) (err error) {
	fd, err := fsopen("overlay")
	if err != nil {
		return fmt.Errorf("fsopen overlay: %w", err)
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("overlay of %d layers on %s: %w%s", len(lowers), target, err, fsMessages(fd))
		}
		_ = syscall.Close(fd)
	}()
	for _, l := range lowers {
		if err := fsconfigString(fd, "lowerdir+", l); err != nil {
			return fmt.Errorf("lowerdir+ %s: %w", l, err)
		}
	}
	for _, kv := range [][2]string{{"upperdir", upper}, {"workdir", work}} {
		if err := fsconfigString(fd, kv[0], kv[1]); err != nil {
			return fmt.Errorf("%s %s: %w", kv[0], kv[1], err)
		}
	}
	if _, _, errno := syscall.Syscall6(sysFsconfig, uintptr(fd), fsconfigCmdCreate, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("create: %w", errno)
	}
	mfd, _, errno := syscall.Syscall(sysFsmount, uintptr(fd), fsmountCloexec, 0)
	if errno != 0 {
		return fmt.Errorf("fsmount: %w", errno)
	}
	defer func() { _ = syscall.Close(int(mfd)) }()
	empty, t, dfd := cString(""), cString(target), atFdcwd
	//nolint:gosec // G103: move_mount(2) takes C strings; both outlive the call.
	_, _, errno = syscall.Syscall6(sysMoveMount, mfd, uintptr(unsafe.Pointer(empty)), uintptr(dfd),
		uintptr(unsafe.Pointer(t)), moveMountFEmptyPath, 0)
	if errno != 0 {
		return fmt.Errorf("move_mount: %w", errno)
	}
	return nil
}

// moveRoot moves the mount at root over / and chroots into it. The mounts
// under it move with it; the boot root, the shares and the layers stay
// mounted beneath it, out of the job's reach.
func (kernel) moveRoot(root string) error {
	// A shared parent refuses MS_MOVE.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}
	if err := syscall.Chdir(root); err != nil {
		return err
	}
	if err := syscall.Mount(".", "/", "", syscall.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move %s to /: %w", root, err)
	}
	if err := syscall.Chroot("."); err != nil {
		return fmt.Errorf("chroot: %w", err)
	}
	return syscall.Chdir("/")
}

func fsopen(fstype string) (int, error) {
	p := cString(fstype)
	//nolint:gosec // G103: fsopen(2) takes a C string; p outlives the call.
	fd, _, errno := syscall.Syscall(sysFsopen, uintptr(unsafe.Pointer(p)), fsopenCloexec, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(fd), nil
}

func fsconfigString(fd int, key, value string) error {
	k, v := cString(key), cString(value)
	//nolint:gosec // G103: fsconfig(2) takes C strings; both outlive the call.
	_, _, errno := syscall.Syscall6(sysFsconfig, uintptr(fd), fsconfigSetString,
		uintptr(unsafe.Pointer(k)), uintptr(unsafe.Pointer(v)), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func fsconfigFlag(fd int, key string) error {
	k := cString(key)
	//nolint:gosec // G103: fsconfig(2) takes a C string; k outlives the call.
	_, _, errno := syscall.Syscall6(sysFsconfig, uintptr(fd), fsconfigSetFlag, uintptr(unsafe.Pointer(k)), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// fsMessages returns the messages the kernel queued on an fs context, which
// say why a step failed.
func fsMessages(fd int) string {
	var msgs []string
	buf := make([]byte, fsContextMessageSize)
	for range 16 {
		n, err := syscall.Read(fd, buf)
		if n <= 0 || err != nil {
			break
		}
		msgs = append(msgs, strings.TrimSpace(string(buf[:n])))
	}
	if len(msgs) == 0 {
		return ""
	}
	return " (" + strings.Join(msgs, "; ") + ")"
}

func cString(s string) *byte {
	p, err := syscall.BytePtrFromString(s)
	if err != nil {
		// Only a NUL byte fails, which no path from the boot config holds:
		// the kernel then sees an empty string and refuses it.
		return &[]byte{0}[0]
	}
	return p
}
