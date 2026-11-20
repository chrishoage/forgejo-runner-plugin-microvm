// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// The vsock constants the syscall package doesn't name.
const (
	afVsock      = 40         // AF_VSOCK
	vmaddrCIDAny = 0xFFFFFFFF // VMADDR_CID_ANY
)

// sockaddrVM is struct sockaddr_vm.
type sockaddrVM struct {
	family    uint16
	reserved1 uint16
	port      uint32
	cid       uint32
	flags     uint8
	zero      [3]uint8
}

// listenVsock listens on a vsock port of the guest. The net package knows no
// vsock and the agent may import nothing beyond grpc and protobuf, so the
// socket is made with raw system calls; accepted connections go through the
// runtime's poller as *os.File.
func listenVsock(port uint32) (net.Listener, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	sa := sockaddrVM{family: afVsock, port: port, cid: vmaddrCIDAny}
	//nolint:gosec // G103: bind(2) takes the sockaddr by pointer; sa outlives the call.
	if _, _, errno := syscall.Syscall(syscall.SYS_BIND, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa)); errno != 0 {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("vsock bind port %d: %w", port, errno)
	}
	if err := syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("vsock listen port %d: %w", port, err)
	}
	// A non-blocking descriptor makes a pollable *os.File.
	f := os.NewFile(uintptr(fd), "vsock:"+strconv.FormatUint(uint64(port), 10))
	rc, err := f.SyscallConn()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &vsockListener{f: f, rc: rc, addr: vsockAddr{port: port}}, nil
}

type vsockListener struct {
	f    *os.File
	rc   syscall.RawConn
	addr vsockAddr
}

func (l *vsockListener) Accept() (net.Conn, error) {
	var nfd int
	var errno syscall.Errno
	err := l.rc.Read(func(fd uintptr) bool {
		// accept4 with no address: syscall.Accept4 fails on a vsock peer,
		// whose address family it can't convert.
		r, _, e := syscall.Syscall6(syscall.SYS_ACCEPT4, fd, 0, 0, syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0, 0)
		nfd, errno = int(r), e
		return errno != syscall.EAGAIN
	})
	if err != nil {
		if errors.Is(err, os.ErrClosed) {
			return nil, net.ErrClosed
		}
		return nil, err
	}
	if errno != 0 {
		return nil, fmt.Errorf("vsock accept: %w", errno)
	}
	return &vsockConn{File: os.NewFile(uintptr(nfd), "vsock-conn"), local: l.addr}, nil
}

func (l *vsockListener) Close() error   { return l.f.Close() }
func (l *vsockListener) Addr() net.Addr { return l.addr }

// vsockConn is an accepted vsock connection. *os.File gives it Read, Write,
// Close and deadlines through the poller.
type vsockConn struct {
	*os.File
	local vsockAddr
}

func (c *vsockConn) LocalAddr() net.Addr  { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr { return vsockAddr{} }

type vsockAddr struct{ port uint32 }

func (vsockAddr) Network() string  { return "vsock" }
func (a vsockAddr) String() string { return "vsock:" + strconv.FormatUint(uint64(a.port), 10) }
