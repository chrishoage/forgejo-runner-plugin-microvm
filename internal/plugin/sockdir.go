// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// sockDir is an open directory that the plugin addresses unix sockets
// through, as /proc/self/fd/<fd>/<name>. A socket's path in bind and connect
// is limited to 107 bytes, which a deep directory, such as a checkout's
// .e2e/plugin/runtime, outgrows; through the open directory, the path stays
// short however deep the directory is.
type sockDir struct {
	f  *os.File
	fd int
}

// openSockDir opens dir, which must exist, for addressing sockets in it.
func openSockDir(dir string) (*sockDir, error) {
	fd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	return &sockDir{f: os.NewFile(uintptr(fd), dir), fd: fd}, nil
}

// path returns a short path for name, relative to the directory, that bind
// and connect accept.
func (d *sockDir) path(name string) string {
	return "/proc/self/fd/" + strconv.Itoa(d.fd) + "/" + name
}

func (d *sockDir) Close() error { return d.f.Close() }
