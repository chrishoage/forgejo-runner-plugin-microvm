// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openPort returns the address of a listener on the host's loopback that
// accepts connections, closed on cleanup.
func openPort(t *testing.T) unix.Sockaddr {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return sockaddr(t, ln.Addr())
}

// closedPort returns the address of a TCP socket on the host's loopback
// that is bound but not listening, so a connection to it is refused, and
// no other socket takes the port before cleanup.
func closedPort(t *testing.T) unix.Sockaddr {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	return sa
}

// silentPort returns the address of a listener on the host's loopback
// whose accept queue is full, so the kernel drops each new SYN to it and a
// connection neither opens nor fails until the client gives up.
func silentPort(t *testing.T) unix.Sockaddr {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	// A backlog of 0 queues one connection; the one below fills it.
	if err := unix.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	in := sa.(*unix.SockaddrInet4)
	c, err := new(net.Dialer).DialContext(t.Context(), "tcp4", (&net.TCPAddr{IP: in.Addr[:], Port: in.Port}).String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return sa
}

func sockaddr(t *testing.T, a net.Addr) unix.Sockaddr {
	t.Helper()
	ap := a.(*net.TCPAddr).AddrPort()
	return &unix.SockaddrInet4{Addr: ap.Addr().Unmap().As4(), Port: int(ap.Port())}
}

// openFDs returns how many descriptors the test process has open.
func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// Each address gets its own result, by index: nil for one that opened,
// the connect's error for one that failed, and os.ErrDeadlineExceeded for
// one that did neither within the wait, however few connects may be open
// at once. Every socket is closed afterwards.
func TestConnectAll(t *testing.T) {
	open, closed, silent := openPort(t), closedPort(t), silentPort(t)
	addrs := []unix.Sockaddr{closed, open, silent, closed, silent, open, closed}
	for _, inFlight := range []int{1, 2, 3, len(addrs), 100} {
		before := openFDs(t)
		errs, err := connectAll(addrs, 50*time.Millisecond, inFlight)
		if err != nil {
			t.Fatalf("in flight %d: %v", inFlight, err)
		}
		if len(errs) != len(addrs) {
			t.Fatalf("in flight %d: %d results for %d addresses", inFlight, len(errs), len(addrs))
		}
		for i, err := range errs {
			var want error
			switch addrs[i] {
			case closed:
				want = unix.ECONNREFUSED
			case silent:
				want = os.ErrDeadlineExceeded
			}
			if !errors.Is(err, want) || (want == nil && err != nil) {
				t.Errorf("in flight %d: address %d: %v, want %v", inFlight, i, err, want)
			}
		}
		if after := openFDs(t); after != before {
			t.Errorf("in flight %d: %d descriptors open before, %d after", inFlight, before, after)
		}
	}
}

// A connect that never ends waits out the wait once, not once per address
// before it: no more than inFlight are open at once, and each is timed from
// its own start.
func TestConnectAllInFlight(t *testing.T) {
	silent := silentPort(t)
	addrs := []unix.Sockaddr{silent, silent, silent, silent}
	const wait = 50 * time.Millisecond
	for _, tc := range []struct {
		inFlight int
		rounds   time.Duration
	}{{1, 4}, {2, 2}, {4, 1}} {
		start := time.Now()
		errs, err := connectAll(addrs, wait, tc.inFlight)
		took := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		for i, err := range errs {
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("in flight %d: address %d: %v, want a timeout", tc.inFlight, i, err)
			}
		}
		// The lower bound is exact: no connect ends before its wait.
		// The upper one only catches a wait run once per address.
		if took < tc.rounds*wait || took >= (tc.rounds+1)*wait+time.Second {
			t.Errorf("in flight %d: took %s, want about %s", tc.inFlight, took, tc.rounds*wait)
		}
	}
}

// When a socket can't be made, connectAll fails, and closes every socket
// it had open.
func TestConnectAllNoSocket(t *testing.T) {
	silent := silentPort(t)
	addrs := make([]unix.Sockaddr, 32)
	for i := range addrs {
		addrs[i] = silent
	}
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	before := openFDs(t)
	// Room for the epoll descriptor and a few sockets.
	low := limit
	low.Cur = uint64(before) + 8
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &low); err != nil {
		t.Fatal(err)
	}
	_, err := connectAll(addrs, time.Minute, len(addrs))
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(err, unix.EMFILE) {
		t.Errorf("connectAll beyond the descriptor limit: %v, want EMFILE", err)
	}
	if after := openFDs(t); after != before {
		t.Errorf("%d descriptors open before, %d after", before, after)
	}
}

// An address of a family connectAll doesn't take fails it, with the
// sockets before it closed.
func TestConnectAllOtherFamily(t *testing.T) {
	before := openFDs(t)
	addrs := []unix.Sockaddr{silentPort(t), &unix.SockaddrUnix{Name: "/nonexistent"}}
	if _, err := connectAll(addrs, time.Minute, len(addrs)); err == nil {
		t.Error("connectAll took a unix socket address")
	}
	if after := openFDs(t); after != before+2 {
		// silentPort's listener and its one connection stay open.
		t.Errorf("%d descriptors open before, %d after, want 2 more", before, after)
	}
}

// A connect that opened is never taken for one that timed out, even when
// its wait ends before connectAll reads its event: here every connect to
// the loopback listener opens before the first wait for events, more of
// them than one wait returns, and the wait is over at once. (AF_VSOCK
// sockets can also open with no event at all: one stays unwritable while
// its peer grants it no credit.)
func TestConnectAllOpenedAtDeadline(t *testing.T) {
	open := openPort(t)
	addrs := make([]unix.Sockaddr, eventsLen+eventsLen/4)
	for i := range addrs {
		addrs[i] = open
	}
	errs, err := connectAll(addrs, 0, len(addrs))
	if err != nil {
		t.Fatal(err)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("address %d: %v, want it opened", i, err)
		}
	}
}

// At its deadline a connect is judged by its socket, not by the events
// read: opened if the socket is connected, the connect's error if it has
// one, and a timeout only if it is still under way.
func TestDeadlineResult(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr unix.Sockaddr
		want error
	}{
		{"opened", openPort(t), nil},
		{"refused", closedPort(t), unix.ECONNREFUSED},
		{"under way", silentPort(t), os.ErrDeadlineExceeded},
	} {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Connect(fd, tc.addr); !errors.Is(err, unix.EINPROGRESS) {
			t.Fatalf("%s: connect: %v, want it in progress", tc.name, err)
		}
		// On the loopback, the connect to an open or closed port has
		// ended by now; poll for it, so the test doesn't rely on that.
		if !errors.Is(tc.want, os.ErrDeadlineExceeded) {
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
			if _, err := unix.Poll(fds, 10_000); err != nil {
				t.Fatal(err)
			}
		}
		got, err := deadlineResult(fd)
		_ = unix.Close(fd)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !errors.Is(got, tc.want) || (tc.want == nil && got != nil) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
