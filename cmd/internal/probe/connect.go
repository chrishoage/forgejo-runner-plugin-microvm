// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// connectAll starts a non-blocking connect of a stream socket to each of
// addrs, IPv4 or vsock, at most inFlight at once, and returns each one's result,
// by index: nil if it opened, the connect's error if it failed, and
// os.ErrDeadlineExceeded if it did neither within wait of its start. It
// fails if a socket can't be made or watched, and closes every socket it
// made on every path.
//
// One event loop over raw sockets, rather than a goroutine and a net.Dial
// for each address: a scan of every port of an address is bound by the
// guest's CPUs, and the loop makes about half the work of net.Dial's
// goroutines, timers and poller registrations.
func connectAll(addrs []unix.Sockaddr, wait time.Duration, inFlight int) ([]error, error) {
	if inFlight < 1 {
		return nil, fmt.Errorf("%d connects in flight: want at least 1", inFlight)
	}
	ep, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("epoll: %w", err)
	}
	defer func() { _ = unix.Close(ep) }()

	type pending struct {
		fd, i    int
		deadline time.Time
		done     bool
	}
	// open holds the connects in flight by descriptor, and queue the same
	// in the order they started, which is their deadlines' order. A
	// descriptor closed is reused, so queue keeps the pending itself.
	open := make(map[int]*pending, min(inFlight, len(addrs)))
	var queue []*pending
	defer func() {
		for fd := range open {
			_ = unix.Close(fd)
		}
	}()
	errs := make([]error, len(addrs))
	finish := func(p *pending, err error) {
		errs[p.i], p.done = err, true
		delete(open, p.fd)
		// Closing the socket also takes it out of the epoll set.
		_ = unix.Close(p.fd)
	}

	events := make([]unix.EpollEvent, eventsLen)
	for next := 0; next < len(addrs) || len(open) > 0; {
		for ; next < len(addrs) && len(open) < inFlight; next++ {
			domain, err := family(addrs[next])
			if err != nil {
				return nil, err
			}
			fd, err := unix.Socket(domain, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				return nil, fmt.Errorf("socket: %w", err)
			}
			if err := unix.Connect(fd, addrs[next]); !errors.Is(err, unix.EINPROGRESS) {
				// Done at once: opened (nil) or failed.
				errs[next] = err
				_ = unix.Close(fd)
				continue
			}
			p := &pending{fd: fd, i: next, deadline: time.Now().Add(wait)}
			open[fd] = p
			queue = append(queue, p)
			ev := unix.EpollEvent{Events: unix.EPOLLOUT, Fd: int32(fd)} //nolint:gosec // G115: a descriptor fits in 32 bits
			if err := unix.EpollCtl(ep, unix.EPOLL_CTL_ADD, fd, &ev); err != nil {
				return nil, fmt.Errorf("epoll: %w", err)
			}
		}
		for len(queue) > 0 && queue[0].done {
			queue = queue[1:]
		}
		if len(queue) == 0 {
			continue
		}
		// Wake for the first deadline at the latest, rounded up so the
		// connect is past it.
		ms := int((time.Until(queue[0].deadline) + time.Millisecond - 1) / time.Millisecond)
		n, err := unix.EpollWait(ep, events, max(ms, 0))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("epoll: %w", err)
		}
		for _, ev := range events[:n] {
			p, ok := open[int(ev.Fd)]
			if !ok {
				continue
			}
			// SO_ERROR is the connect's result once it is writable or
			// in error: 0 if it opened. Not knowing it is no result.
			code, err := unix.GetsockoptInt(p.fd, unix.SOL_SOCKET, unix.SO_ERROR)
			if err != nil {
				return nil, fmt.Errorf("SO_ERROR: %w", err)
			}
			var result error
			if code != 0 {
				result = syscall.Errno(code)
			}
			finish(p, result)
		}
		// A connect past its deadline may still have opened or failed:
		// its event may be among those this wait had no room for, and an
		// AF_VSOCK socket that opened is writable only once its peer
		// grants it credit, which a peer may never do.
		for now := time.Now(); len(queue) > 0 && (queue[0].done || !now.Before(queue[0].deadline)); queue = queue[1:] {
			if !queue[0].done {
				result, err := deadlineResult(queue[0].fd)
				if err != nil {
					return nil, err
				}
				finish(queue[0], result)
			}
		}
	}
	return errs, nil
}

// eventsLen is how many events one wait of connectAll's returns at most.
const eventsLen = 256

// deadlineResult returns the result of fd's connect once its wait is over:
// nil if the socket is connected, the connect's error if it failed, and
// os.ErrDeadlineExceeded if it is still under way. It fails if it can't
// tell.
func deadlineResult(fd int) (result, err error) {
	code, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
	if err != nil {
		return nil, fmt.Errorf("SO_ERROR: %w", err)
	}
	if code != 0 {
		return syscall.Errno(code), nil
	}
	// getpeername succeeds only on a connected socket, TCP or vsock.
	switch _, err := unix.Getpeername(fd); {
	case err == nil:
		return nil, nil
	case errors.Is(err, unix.ENOTCONN):
		return os.ErrDeadlineExceeded, nil
	default:
		return nil, fmt.Errorf("getpeername: %w", err)
	}
}

// family returns the address family of sa, which connectAll takes.
func family(sa unix.Sockaddr) (int, error) {
	switch sa.(type) {
	case *unix.SockaddrInet4:
		return unix.AF_INET, nil
	case *unix.SockaddrVM:
		return unix.AF_VSOCK, nil
	default:
		return 0, fmt.Errorf("%T: not an IPv4 or vsock address", sa)
	}
}
