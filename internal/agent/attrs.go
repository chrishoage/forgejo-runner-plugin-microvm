// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// The extended attribute and time calls the copies need on a file's
// descriptor or on a name that must not be followed, which the syscall
// package doesn't offer and the agent may not import golang.org/x/sys for.

// inDir names the entry base in the directory open as dirfd, for the calls
// that take only a path: the kernel resolves the descriptor's magic link to
// that directory, so the lookup starts where the os.Root that opened it
// ended, and base, one name, is not followed by an l-call.
func inDir(dirfd uintptr, base string) string {
	return "/proc/self/fd/" + strconv.FormatUint(uint64(dirfd), 10) + "/" + base
}

// fsetxattr sets the extended attribute name of the file open as fd.
func fsetxattr(fd uintptr, name string, value []byte) error {
	n, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	//nolint:gosec // G103: fsetxattr(2) takes a C string and a buffer; both outlive the call.
	_, _, errno := syscall.Syscall6(syscall.SYS_FSETXATTR, fd, uintptr(unsafe.Pointer(n)),
		uintptr(unsafe.Pointer(bytePtr(value))), uintptr(len(value)), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// lsetxattr sets the extended attribute name of path, not following a final
// symlink.
func lsetxattr(path, name string, value []byte) error {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	n, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	//nolint:gosec // G103: lsetxattr(2) takes C strings and a buffer; all outlive the call.
	_, _, errno := syscall.Syscall6(syscall.SYS_LSETXATTR, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(n)),
		uintptr(unsafe.Pointer(bytePtr(value))), uintptr(len(value)), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// lxattrs returns path's extended attributes, not following a final
// symlink: none on a filesystem without them.
func lxattrs(path string) (map[string]string, error) {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return nil, err
	}
	names, err := sizedCall(func(buf []byte) (uintptr, syscall.Errno) {
		//nolint:gosec // G103: llistxattr(2) takes a C string and a buffer; both outlive the call.
		n, _, errno := syscall.Syscall(syscall.SYS_LLISTXATTR, uintptr(unsafe.Pointer(p)),
			uintptr(unsafe.Pointer(bytePtr(buf))), uintptr(len(buf)))
		return n, errno
	})
	if errors.Is(err, syscall.ENOTSUP) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var attrs map[string]string
	for name := range strings.SplitSeq(string(bytes.TrimSuffix(names, []byte{0})), "\x00") {
		if name == "" {
			continue
		}
		n, err := syscall.BytePtrFromString(name)
		if err != nil {
			return nil, err
		}
		value, err := sizedCall(func(buf []byte) (uintptr, syscall.Errno) {
			//nolint:gosec // G103: lgetxattr(2) takes C strings and a buffer; all outlive the call.
			v, _, errno := syscall.Syscall6(syscall.SYS_LGETXATTR, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(n)),
				uintptr(unsafe.Pointer(bytePtr(buf))), uintptr(len(buf)), 0, 0)
			return v, errno
		})
		if errors.Is(err, syscall.ENODATA) {
			continue // removed since it was listed
		}
		if err != nil {
			return nil, err
		}
		if attrs == nil {
			attrs = map[string]string{}
		}
		attrs[name] = string(value)
	}
	return attrs, nil
}

// maxXattrSize is the kernel's limit on an extended attribute's value, and
// on the list of a file's attribute names.
const maxXattrSize = 64 << 10

// sizedCall runs an xattr call that fills buf, first with an empty buf to
// learn the size, and again while the value grew in between, up to the
// kernel's own limit.
func sizedCall(call func(buf []byte) (uintptr, syscall.Errno)) ([]byte, error) {
	for {
		n, errno := call(nil)
		if errno != 0 {
			return nil, errno
		}
		if n == 0 {
			return nil, nil
		}
		buf := make([]byte, min(int(n), maxXattrSize))
		n, errno = call(buf)
		if errno == syscall.ERANGE && len(buf) < maxXattrSize {
			continue
		}
		if errno != 0 {
			return nil, errno
		}
		return buf[:n], nil
	}
}

// utimensat sets the access and modification times of base in the
// directory open as dirfd, not following base when it is a symlink, or of
// the file open as dirfd itself when base is empty.
func utimensat(dirfd uintptr, base string, atime, mtime time.Time) error {
	ts := [2]syscall.Timespec{timespec(atime), timespec(mtime)}
	var p *byte
	flags := 0
	if base != "" {
		var err error
		if p, err = syscall.BytePtrFromString(base); err != nil {
			return err
		}
		flags = atSymlinkNofollow
	}
	// A NULL path sets the times of dirfd's own file, as futimens does.
	//nolint:gosec // G103: utimensat(2) takes a C string and two timespecs; both outlive the call.
	_, _, errno := syscall.Syscall6(syscall.SYS_UTIMENSAT, dirfd, uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&ts[0])), uintptr(flags), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

const atSymlinkNofollow = 0x100

func timespec(t time.Time) syscall.Timespec {
	return syscall.Timespec{Sec: t.Unix(), Nsec: int64(t.Nanosecond())}
}

// bytePtr is b's first byte, or nil for an empty b.
func bytePtr(b []byte) *byte {
	if len(b) == 0 {
		return nil
	}
	return &b[0]
}
