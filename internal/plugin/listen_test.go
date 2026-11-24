// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// leaveSocket binds a unix socket at path and closes it without unlinking
// it, as a crashed plugin leaves its listen socket.
func leaveSocket(t *testing.T, path string) {
	t.Helper()
	l, err := new(net.ListenConfig).Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = l.Close()
}

// Binding over a socket a crashed plugin left succeeds, and the new socket
// is mode 0660 whatever the umask, also one that would widen it (0) or
// narrow it (077); it accepts, and Close deletes it. Not parallel: the umask
// is the process's.
func TestListenOverALeftoverSocket(t *testing.T) {
	for _, umask := range []int{0, 0o077} {
		dir := t.TempDir()
		sock := filepath.Join(dir, "plugin.sock")
		leaveSocket(t, sock)
		old := syscall.Umask(umask)
		lis, closeLis, err := listen(t.Context(), sock)
		syscall.Umask(old)
		if err != nil {
			t.Fatalf("umask %#o: listen over a leftover socket: %v", umask, err)
		}
		fi, err := os.Lstat(sock)
		if err != nil {
			closeLis()
			t.Fatal(err)
		}
		if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o660 {
			t.Errorf("umask %#o: the listen socket is %v, want a socket of mode 0660", umask, fi.Mode())
		}
		accepted := make(chan error, 1)
		go func() {
			c, err := lis.Accept()
			if err == nil {
				err = c.Close()
			}
			accepted <- err
		}()
		c, err := new(net.Dialer).DialContext(t.Context(), "unix", sock)
		if err != nil {
			closeLis()
			t.Fatalf("umask %#o: dial the new socket: %v", umask, err)
		}
		_ = c.Close()
		if err := <-accepted; err != nil {
			t.Errorf("umask %#o: accept: %v", umask, err)
		}
		closeLis()
		if es, err := os.ReadDir(dir); err != nil || len(es) != 0 {
			t.Errorf("umask %#o: after Close the directory holds %v (%v), want nothing", umask, es, err)
		}
	}
}

// Anything but a socket at the listen socket's path stops startup, naming
// the path and what is there, and is left as it was, with no open
// descriptor left behind. Not parallel: it counts the process's open
// descriptors.
func TestListenRefusesAnythingButASocket(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file.sock")
	if err := os.WriteFile(file, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "dir.sock")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	// A symlink to a socket is not a socket the plugin left.
	target := filepath.Join(dir, "target.sock")
	leaveSocket(t, target)
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for path, kind := range map[string]string{file: "regular file", sub: "directory", link: "symbolic link"} {
		before := openFDs(t)
		lis, closeLis, err := listen(t.Context(), path)
		if err == nil {
			closeLis()
			t.Fatalf("listen on %s, where a %s is, succeeded on %v", path, kind, lis.Addr())
		}
		if msg := err.Error(); !strings.Contains(msg, path) || !strings.Contains(msg, "a "+kind+" is there") {
			t.Errorf("error %q does not say a %s is at %s", msg, kind, path)
		}
		if fi, err := os.Lstat(path); err != nil || fi.Mode().Type() == os.ModeSocket {
			t.Errorf("the %s at %s is gone or replaced: %v", kind, path, err)
		}
		if after := openFDs(t); after != before {
			t.Errorf("%d open descriptors after a failed listen, %d before", after, before)
		}
	}
	if fi, err := os.Lstat(target); err != nil || fi.Mode().Type() != os.ModeSocket {
		t.Errorf("the symlink's target socket is gone: %v", err)
	}
}
