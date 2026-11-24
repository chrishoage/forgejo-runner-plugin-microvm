// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The job network's addresses as the guest sees them (docs/architecture.md,
// Job network), the agent's vsock port, and how long the probe gives a
// connection that must not open.
const (
	gatewayIP  = "198.18.0.1"
	vmIP       = "198.18.0.2"
	jobSubnet  = "198.18.0.0/24"
	unusedIP   = "198.18.0.254"
	agentVsock = "1024"
	// An unused subnet address never answers ARP, so a connection to it
	// neither opens nor fails until the bound. One that would open does
	// so within milliseconds: a NAT entry mapping the address to the
	// host's loopback, as gvproxy's defaults have for their .254, has the
	// gateway answer ARP for it at once and its forwarder dial the host's
	// loopback.
	unreachWait = "1s"
)

// hostListener is a TCP listener on the CI host that records the peer of
// each connection it accepts.
type hostListener struct {
	ln   net.Listener
	mu   sync.Mutex
	from []netip.Addr
	seen chan struct{}
}

// listenHost listens on addr and stops on cleanup.
func listenHost(t *testing.T, addr netip.AddrPort) (*hostListener, error) {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp4", addr.String())
	if err != nil {
		return nil, err
	}
	l := &hostListener{ln: ln, seen: make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			l.mu.Lock()
			l.from = append(l.from, c.RemoteAddr().(*net.TCPAddr).AddrPort().Addr().Unmap())
			l.mu.Unlock()
			select {
			case l.seen <- struct{}{}:
			default:
			}
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); <-done })
	return l, nil
}

func (l *hostListener) addr() netip.AddrPort { return l.ln.Addr().(*net.TCPAddr).AddrPort() }

func (l *hostListener) accepted() []netip.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.from)
}

// waitAccepted fails t unless l has accepted a connection within 10 s. A
// connection the guest opened is accepted within milliseconds, so only a
// missing one waits it out.
func (l *hostListener) waitAccepted(t *testing.T) {
	t.Helper()
	select {
	case <-l.seen:
	case <-time.After(10 * time.Second):
		t.Errorf("the listener on %s saw no connection", l.addr())
	}
}

// listenPair opens a listener on the host's 127.0.0.1 and one on the same
// port of its non-loopback address host.
func listenPair(t *testing.T, host netip.Addr) (loopback, outside *hostListener) {
	t.Helper()
	for range 20 {
		lo, err := listenHost(t, netip.MustParseAddrPort("127.0.0.1:0"))
		if err != nil {
			t.Fatal(err)
		}
		out, err := listenHost(t, netip.AddrPortFrom(host, lo.addr().Port()))
		if errors.Is(err, syscall.EADDRINUSE) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		return lo, out
	}
	t.Fatal("no port free on both the host's loopback and its address")
	return nil, nil
}

// hostUDP is a UDP socket on the CI host that records whom it received
// datagrams from.
type hostUDP struct {
	conn net.PacketConn
	mu   sync.Mutex
	from []netip.Addr
	seen chan struct{}
}

// listenHostUDP listens on addr and stops on cleanup.
func listenHostUDP(t *testing.T, addr netip.AddrPort) *hostUDP {
	t.Helper()
	conn, err := new(net.ListenConfig).ListenPacket(t.Context(), "udp4", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	l := &hostUDP{conn: conn, seen: make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1500)
		for {
			_, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			l.mu.Lock()
			l.from = append(l.from, from.(*net.UDPAddr).AddrPort().Addr().Unmap())
			l.mu.Unlock()
			select {
			case l.seen <- struct{}{}:
			default:
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close(); <-done })
	return l
}

func (l *hostUDP) addr() netip.AddrPort { return l.conn.LocalAddr().(*net.UDPAddr).AddrPort() }

func (l *hostUDP) received() []netip.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.from)
}

// probeIn runs the probe in v with args, allowing it up to five minutes,
// and fails t unless it exits 0. The probe bounds each of its own waits in
// seconds, so only an Exec that hangs reaches the bound.
func probeIn(t *testing.T, v *vm, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	code, out, err := v.run(ctx, append([]string{probeDir + "/probe"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Errorf("probe %s: exit %d:\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

// From inside a booted VM, a step can't reach what the job network keeps
// from it: the host's loopback (NET-4), any gateway service but DNS and its
// fixed-lease DHCP server (NET-6), the host over vsock, or a link-local
// address (NET-5). Each check that something can't be reached has a
// positive control from the same guest, so a guest with no network can't
// pass.
func TestGuestCantReach(t *testing.T) {
	host := hostAddr(t)
	probe := buildProbe(t)
	v := bootVM(t, startPlugin(t, buildPlugin(t)), "unreachable")
	v.copyIn(t, probe, probeDir)
	// The host loopback subtest sends it datagrams; the last subtest
	// checks that none arrived.
	loUDP := listenHostUDP(t, netip.MustParseAddrPort("127.0.0.1:0"))
	var (
		loRan     bool
		controlAt time.Time
	)

	loOK := t.Run("host loopback", func(t *testing.T) {
		loRan = true
		lo, out := listenPair(t, host)
		port := strconv.Itoa(int(lo.addr().Port()))
		// The probe's default 10 s for the positive control, which only
		// a connection that fails waits out.
		probeIn(t, v, "connect", out.addr().String())
		out.waitAccepted(t)
		// 127.0.0.1 is the guest's own and the guard resets a connection
		// to the gateway, so only the unused address waits unreachWait.
		probeIn(t, v, "unreachable", "-timeout", unreachWait, port, "127.0.0.1", gatewayIP, unusedIP)
		if from := lo.accepted(); len(from) != 0 {
			t.Errorf("the listener on the host's loopback accepted from %v", from)
		}

		// A datagram to 0.0.0.0 or 127.0.0.1, written raw to the gateway's
		// MAC as a root guest can, since the guest's own routing would
		// keep it, reaches no UDP socket on the host's loopback; the same
		// to the host's non-loopback address arrives.
		outUDP := listenHostUDP(t, netip.AddrPortFrom(host, 0))
		loPort := strconv.Itoa(int(loUDP.addr().Port()))
		probeIn(t, v, "udp", "0.0.0.0:"+loPort, "127.0.0.1:"+loPort, outUDP.addr().String())
		// The control arrives within milliseconds of the probe's exit,
		// so only a missing one waits out the 10 s.
		select {
		case <-outUDP.seen:
			controlAt = time.Now()
		case <-time.After(10 * time.Second):
			t.Fatalf("the UDP socket on %s received nothing", outUDP.addr())
		}
	})

	t.Run("gateway", func(t *testing.T) {
		// The guard refuses within milliseconds. Only a port that does
		// anything else waits out the 5 s, and fails the check either
		// way, so the bound costs only a failing run.
		probeIn(t, v, "refuses", "-timeout", "5s", gatewayIP, "1-52")
		probeIn(t, v, "refuses", "-timeout", "5s", gatewayIP, "54-65535")
		// Each answer waits up to the probe's default 10 s, which only a
		// missing one waits out.
		for _, network := range []string{"-tcp=false", "-tcp"} {
			if out := probeIn(t, v, "resolve", network, "localhost", gatewayIP+":53"); !strings.Contains(out, "127.0.0.1") {
				t.Errorf("resolve %s: no 127.0.0.1 in %q", network, out)
			}
		}
		// The probe sends each DISCOVER once the one before it has its
		// offer, and stops at the last one's, so only a DISCOVER with no
		// offer waits out its 10 s, the probe's default; the gateway's
		// server answers within milliseconds.
		out := probeIn(t, v, "dhcp", "-n", "3")
		offers := 0
		for line := range strings.Lines(out) {
			if !strings.HasPrefix(line, "offer ") {
				continue
			}
			offers++
			if !strings.Contains(line, " yiaddr="+vmIP+"\n") {
				t.Errorf("DHCP offered another address than the VM's own %s: %s", vmIP, line)
			}
		}
		if offers < 3 {
			t.Errorf("%d DHCP offers to 3 DISCOVERs:\n%s", offers, out)
		}
	})

	t.Run("vsock", func(t *testing.T) {
		// Every port up to 2048, the agent's among them, and a few above,
		// all at once: libkrun leaves a request for a port it doesn't
		// serve unanswered, so each waits out the probe's 2 s bound,
		// the kernel's default for a vsock connect.
		probeIn(t, v, "vsock", "0-2048", agentVsock, "8080", "65535", "4294967294")
		// The plugin still reaches the agent.
		probeIn(t, v, "netinfo")
	})

	t.Run("link-local", func(t *testing.T) {
		// Refused at once, by the library; as for the gateway, only a
		// failing run waits out the bound.
		probeIn(t, v, "refuses", "-timeout", "5s", "169.254.169.254", "80")
	})

	// The gateway sends each datagram on from a goroutine of its own, one
	// for each destination, so the control arriving doesn't mean an
	// earlier datagram has been sent, and nothing in the gateway orders
	// them. One to the host's loopback would arrive well within 2 s of the
	// control. The subtests since then took longer than that, so this
	// waits for whatever is left of it, if anything.
	t.Run("host loopback, late UDP", func(t *testing.T) {
		// Without the host loopback subtest's datagrams there is nothing
		// to wait for, and a failure before its control arrived is
		// already reported there.
		switch {
		case !loRan:
			t.Skip("the host loopback subtest, which sends the datagrams, didn't run")
		case controlAt.IsZero() && !loOK:
			t.Skip("the host loopback subtest failed before its control datagram arrived")
		case controlAt.IsZero():
			t.Fatal("the host loopback subtest passed with no control datagram")
		}
		select {
		case <-loUDP.seen:
		case <-time.After(time.Until(controlAt.Add(2 * time.Second))):
		}
		if from := loUDP.received(); len(from) != 0 {
			t.Errorf("the UDP socket on the host's loopback received from %v", from)
		}
	})
}

// While job A's guest listens on a port, job B's guest, on a job network of
// its own with the same addresses, reaches it at no address of the subnet,
// and A's listener records no connection from B (NET-1). Both guests reach
// the host, so neither passes for lack of a network.
//
// Both VMs have the same address, so B scanning the subnet would pass even
// on a switch shared with A: B's connection to A's address stays in B. So
// B also broadcasts an ARP request carrying bMarker as its sender address,
// which every port of B's switch gets and A's guest, sniffing ARP, must not
// see. A's own request carrying aMarker afterwards, at its stop, is its
// sniffer's positive control, and orders the two: the switch hands a
// broadcast to its other ports before its gateway answers it, and a port's
// frames reach its guest in order, so on a shared switch A's sniffer would
// see bMarker before the gateway's reply to aMarker.
func TestJobsCantReachEachOther(t *testing.T) {
	const (
		port    = "7777"
		aMarker = "198.18.0.250"
		bMarker = "198.18.0.251"
	)
	ext, err := listenHost(t, netip.AddrPortFrom(hostAddr(t), 0))
	if err != nil {
		t.Fatal(err)
	}
	probe := buildProbe(t)
	p := startPlugin(t, buildPlugin(t))
	a, b := bootVM(t, p, "job-a"), bootVM(t, p, "job-b")
	for _, v := range []*vm{a, b} {
		v.copyIn(t, probe, probeDir)
		probeIn(t, v, "connect", ext.addr().String())
		ext.waitAccepted(t)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	type result struct {
		code int32
		out  string
		err  error
	}
	// The plugin runs one call on a job at a time, so A's guest does its
	// part in one Exec: once its listener is up and has accepted A's own
	// connection to it at A's address, it dials ctl, and it stops at the
	// line the test sends there.
	ctl, err := new(net.ListenConfig).Listen(t.Context(), "tcp4", netip.AddrPortFrom(hostAddr(t), 0).String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctl.Close() })
	if err := ctl.(*net.TCPListener).SetDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	listened := make(chan result, 1)
	go func() {
		code, out, err := a.run(ctx, probeDir+"/probe", "listen", "-for", "4m", "-arp", aMarker, "0.0.0.0:"+port, ctl.Addr().String())
		listened <- result{code, out, err}
	}()
	var res *result
	t.Cleanup(func() {
		cancel()
		if res == nil {
			<-listened
		}
	})
	type dial struct {
		c   net.Conn
		err error
	}
	dialed := make(chan dial, 1)
	go func() {
		c, err := ctl.Accept()
		dialed <- dial{c, err}
	}()
	var stop net.Conn
	select {
	case d := <-dialed:
		if d.err != nil {
			t.Fatalf("job A's listener never came up: %v", d.err)
		}
		stop = d.c
		t.Cleanup(func() { _ = stop.Close() })
	case r := <-listened:
		res = &r
		t.Fatalf("job A's listener ended: exit %d, %v:\n%s", r.code, r.err, r.out)
	}

	probeIn(t, b, "unreachable", "-timeout", unreachWait, "-send", "from-b", port, jobSubnet)
	probeIn(t, b, "arp", bMarker, gatewayIP)

	if err := stop.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stop, "stop\n"); err != nil {
		t.Fatalf("stopping job A's listener: %v", err)
	}
	r := <-listened
	res = &r
	if r.err != nil || r.code != 0 {
		t.Fatalf("job A's listener: exit %d, %v:\n%s", r.code, r.err, r.out)
	}
	wantOnlyOwn(t, r.out)
	if !strings.Contains(r.out, " -> "+aMarker+"\n") {
		t.Errorf("job A's sniffer missed the gateway's reply to %s:\n%s", aMarker, r.out)
	}
	if strings.Contains(r.out, " "+bMarker+" ") || strings.Contains(r.out, " "+bMarker+"\n") {
		t.Errorf("job A's guest received job B's ARP request:\n%s", r.out)
	}
}

// wantOnlyOwn fails t unless the listen probe's output out shows it
// accepted only job A's own connection: the one "ready" from A's address.
// A connection from B, even one whose line never arrived, would be an
// accept of another line.
func wantOnlyOwn(t *testing.T, out string) {
	t.Helper()
	var accepts [][2]string
	for line := range strings.Lines(out) {
		peer, sent, ok := strings.Cut(strings.TrimPrefix(strings.TrimSuffix(line, "\n"), "accepted "), " ")
		if !strings.HasPrefix(line, "accepted ") || !ok {
			continue
		}
		host, _, err := net.SplitHostPort(peer)
		if err != nil {
			t.Errorf("job A's listener: peer %q: %v", peer, err)
		}
		accepts = append(accepts, [2]string{host, sent})
	}
	if !slices.Equal(accepts, [][2]string{{vmIP, `"ready"`}}) {
		t.Errorf("job A's listener accepted other connections than its own ready:\n%s", out)
	}
}
