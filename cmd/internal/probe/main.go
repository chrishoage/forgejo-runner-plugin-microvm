// SPDX-License-Identifier: GPL-3.0-or-later

// Command probe is the network probe the KVM tests copy into a booted VM
// with CopyIn and run with Exec. It is built by the test, static
// (CGO_ENABLED=0), so it runs on any image, and never ships.
//
//	probe connect [-timeout d] [-send line] <host:port>
//	    open a TCP connection, send line if given, print "connected"
//	probe unreachable [-timeout d] [-send line] <port> <addr|prefix>...
//	    connect to port at each address, every address of each prefix
//	    included; fail if any connection opens, sending it line first
//	probe refuses [-timeout d] <IPv4 address> <first>-<last>
//	    connect to every port in the range; fail if any port does anything
//	    but refuse
//	probe listen [-for d] [-arp marker] <host:port> <control host:port>
//	    accept connections, print each one's peer and first line; once up,
//	    connect to the listener at eth0's address saying "ready", then to
//	    control, and exit at the line "stop" from it, or fail after d; with
//	    -arp, sniff ARP on eth0 from before the listener is up, and at the
//	    stop ask the default gateway as arp does, with marker as the
//	    sender, then print every ARP packet eth0 received
//	probe arp [-timeout d] <sender> <target>
//	    broadcast an ARP request for target from eth0's MAC, carrying
//	    sender as its sender address, until target's reply to sender
//	    arrives
//	probe udp <host:port>...
//	    send each a UDP datagram, written raw on eth0 to the default
//	    gateway's MAC, as a root guest can, so the guest's own routing
//	    can't keep it (it takes 0.0.0.0 as its own loopback)
//	probe resolve [-tcp] <name> <server:53>
//	    ask server for name's A records, over UDP or TCP
//	probe lookup <name>
//	    resolve name's IPv4 addresses as a step's programs do, through
//	    /etc/hosts and /etc/resolv.conf, and print each
//	probe dhcp [-n count] [-timeout d]
//	    broadcast count DHCP DISCOVERs from eth0's MAC, print every reply's
//	    offered address
//	probe vsock [-cid cid] <first>-<last>...
//	    open an AF_VSOCK connection to cid on every port in the ranges; fail
//	    if any opens
//	probe netinfo
//	    print lo's and eth0's state, addresses and the default route
//
// Each exits 0 on success, and 1 with the error on stderr otherwise. Every
// network wait is bounded by a timeout.
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"
)

// timeout bounds each check that expects an answer.
const timeout = 10 * time.Second

// workers bounds the connections a scan has open at once.
const workers = 256

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
}

const usage = "usage: probe connect|unreachable|refuses|listen|arp|udp|resolve|lookup|dhcp|vsock|netinfo [flags] [args]"

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	wait := fs.Duration("timeout", timeout, "how long each connection may take")
	send := fs.String("send", "", "a line to send on each connection that opens")
	listenFor := fs.Duration("for", 5*time.Minute, "how long to listen")
	marker := fs.String("arp", "", "sniff ARP while listening, and at the stop ask the gateway with this sender address")
	overTCP := fs.Bool("tcp", false, "resolve over TCP")
	count := fs.Int("n", 3, "how many DISCOVERs to send")
	cid := fs.Uint("cid", unix.VMADDR_CID_HOST, "the vsock CID to connect to")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("%s: %w", args[0], err)
	}
	rest := fs.Args()
	switch {
	case args[0] == "connect" && len(rest) == 1:
		return connect(rest[0], *wait, *send)
	case args[0] == "unreachable" && len(rest) >= 2:
		return unreachable(rest[0], rest[1:], *wait, *send)
	case args[0] == "refuses" && len(rest) == 2:
		return refuses(rest[0], rest[1], *wait)
	case args[0] == "listen" && len(rest) == 2:
		return listen(rest[0], rest[1], *listenFor, *marker)
	case args[0] == "arp" && len(rest) == 2:
		return arp(rest[0], rest[1], *wait)
	case args[0] == "udp" && len(rest) >= 1:
		return rawUDP(rest)
	case args[0] == "resolve" && len(rest) == 2:
		return resolve(rest[0], rest[1], *overTCP)
	case args[0] == "lookup" && len(rest) == 1:
		return lookup(rest[0])
	case args[0] == "dhcp" && len(rest) == 0:
		return dhcp(*count, *wait)
	case args[0] == "vsock" && len(rest) >= 1:
		return vsock(uint32(*cid), rest) //nolint:gosec // G115: a flag value; a CID is 32 bits
	case args[0] == "netinfo" && len(rest) == 0:
		return netinfo()
	default:
		return errors.New(usage)
	}
}

// dial opens a TCP connection to addr within wait.
func dial(addr string, wait time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return new(net.Dialer).DialContext(ctx, "tcp", addr)
}

// sendLine sends line on c within wait, if line isn't empty.
func sendLine(c net.Conn, line string, wait time.Duration) error {
	if line == "" {
		return nil
	}
	if err := c.SetWriteDeadline(time.Now().Add(wait)); err != nil {
		return err
	}
	_, err := io.WriteString(c, line+"\n")
	return err
}

func connect(addr string, wait time.Duration, line string) error {
	c, err := dial(addr, wait)
	if err != nil {
		return err
	}
	if err := sendLine(c, line, wait); err != nil {
		return errors.Join(err, c.Close())
	}
	fmt.Printf("connected %s -> %s\n", c.LocalAddr(), c.RemoteAddr())
	return c.Close()
}

// scan runs try on each of items, workers at a time, and returns the
// errors it returned, by item.
func scan[T comparable](items []T, try func(T) error) map[T]error {
	var mu sync.Mutex
	errs := make(map[T]error, len(items))
	next := make(chan T)
	var wg sync.WaitGroup
	for range min(workers, len(items)) {
		wg.Go(func() {
			for it := range next {
				err := try(it)
				mu.Lock()
				errs[it] = err
				mu.Unlock()
			}
		})
	}
	for _, it := range items {
		next <- it
	}
	close(next)
	wg.Wait()
	return errs
}

// unreachable connects to port at every target, an address or a prefix of
// addresses, each within wait, and fails if any connection opens. It
// asserts no particular error: an address nothing answers ARP for only
// times out.
func unreachable(port string, targets []string, wait time.Duration, line string) error {
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("port %q: %w", port, err)
	}
	var addrs []netip.Addr
	for _, t := range targets {
		if p, err := netip.ParsePrefix(t); err == nil {
			for a := p.Masked().Addr(); p.Contains(a); a = a.Next() {
				addrs = append(addrs, a)
			}
			continue
		}
		a, err := netip.ParseAddr(t)
		if err != nil {
			return fmt.Errorf("target %q: neither an address nor a prefix", t)
		}
		addrs = append(addrs, a)
	}
	errs := scan(addrs, func(a netip.Addr) error {
		c, err := dial(net.JoinHostPort(a.String(), port), wait)
		if err != nil {
			return err
		}
		// The connection opened, so a is reached, whether or not the
		// line gets through.
		_ = sendLine(c, line, wait)
		_ = c.Close()
		return nil
	})
	var reached []string
	for _, a := range addrs {
		if errs[a] == nil {
			reached = append(reached, a.String())
		}
	}
	fmt.Printf("unreachable: %d of %d addresses on port %s\n", len(addrs)-len(reached), len(addrs), port)
	if len(reached) > 0 {
		return fmt.Errorf("reached port %s at %s", port, strings.Join(reached, " "))
	}
	return nil
}

// refuses connects to every port of host, an IPv4 address, in ports,
// "first-last", workers at a time, and fails if any port does anything but
// refuse within wait.
func refuses(host, ports string, wait time.Duration) error {
	a, err := netip.ParseAddr(host)
	if err != nil || !a.Is4() {
		return fmt.Errorf("host %q: not an IPv4 address", host)
	}
	all, err := portRange(ports, 1, 65535)
	if err != nil {
		return err
	}
	addrs := make([]unix.Sockaddr, len(all))
	for i, p := range all {
		addrs[i] = &unix.SockaddrInet4{Addr: a.As4(), Port: int(p)}
	}
	errs, err := connectAll(addrs, wait, workers)
	if err != nil {
		return err
	}
	var bad []string
	for i, p := range all {
		switch err := errs[i]; {
		case err == nil:
			bad = append(bad, fmt.Sprintf("%d: connected", p))
		case !errors.Is(err, syscall.ECONNREFUSED):
			bad = append(bad, fmt.Sprintf("%d: %v", p, err))
		}
	}
	fmt.Printf("refused: %d of %d ports of %s\n", len(all)-len(bad), len(all), host)
	if len(bad) > 0 {
		return fmt.Errorf("%d ports did not refuse, the first: %s", len(bad), strings.Join(bad[:min(len(bad), 10)], "; "))
	}
	return nil
}

// portRange parses "first-last", or a single port, within lo and hi.
func portRange(s string, lo, hi uint64) ([]uint32, error) {
	first, last, ok := strings.Cut(s, "-")
	if !ok {
		last = first
	}
	a, err := strconv.ParseUint(first, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("range %q: %w", s, err)
	}
	b, err := strconv.ParseUint(last, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("range %q: %w", s, err)
	}
	if a < lo || b > hi || a > b {
		return nil, fmt.Errorf("range %q: not within %d-%d", s, lo, hi)
	}
	ports := make([]uint32, 0, b-a+1)
	for p := a; p <= b; p++ {
		ports = append(ports, uint32(p)) //nolint:gosec // G115: p <= hi, which callers keep within 32 bits
	}
	return ports, nil
}

// listen accepts connections on addr, printing each one's peer and first
// line, from when it is up until the stop. Once its listener is up, it
// connects to it at eth0's address, saying "ready", then connects to
// control and waits for the line "stop" from it. It fails if a step fails
// or there is no stop within d.
//
// With a marker, it opens an ARP socket on eth0 before it listens, and at
// the stop asks the default gateway as arp does, with marker as the
// sender, then prints every ARP packet that socket received.
//
// One process does it all because the plugin runs one call on a job at a
// time: while this Exec runs, the job takes no other, so the test tells it
// to stop through a connection it opens to the test.
func listen(addr, control string, d time.Duration, marker string) error {
	deadline := time.Now().Add(d)
	sniffer := -1
	if marker != "" {
		fd, _, err := arpSocket()
		if err != nil {
			return err
		}
		defer func() { _ = unix.Close(fd) }()
		sniffer = fd
	}
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	if err := ln.(*net.TCPListener).SetDeadline(deadline); err != nil {
		return err
	}
	fmt.Printf("listening %s\n", ln.Addr())
	ready := make(chan struct{})
	var readyOnce sync.Once
	var accepting sync.WaitGroup
	accepting.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			line, err := firstLine(c)
			fmt.Printf("accepted %s %q\n", c.RemoteAddr(), line)
			if err == nil && line == "ready" {
				readyOnce.Do(func() { close(ready) })
			}
		}
	})
	err = untilStop(ln.Addr().(*net.TCPAddr).Port, control, ready, deadline)
	if err == nil && sniffer >= 0 {
		err = askGateway(marker)
	}
	// Closing the listener ends Accept; firstLine is bounded by timeout.
	_ = ln.Close()
	accepting.Wait()
	if err == nil && sniffer >= 0 {
		err = printARP(sniffer)
	}
	return err
}

// askGateway asks the default gateway as arp does, with marker as the
// sender.
func askGateway(marker string) error {
	gw, _, err := defaultRoute()
	if err != nil {
		return err
	}
	return arp(marker, gw.String(), timeout)
}

// untilStop connects to port at eth0's address, saying "ready", waits until
// ready is closed, then connects to control and waits for the line "stop"
// from it, all before deadline.
func untilStop(port int, control string, ready <-chan struct{}, deadline time.Time) error {
	own, err := eth0Addr()
	if err != nil {
		return err
	}
	if err := connect(net.JoinHostPort(own.String(), strconv.Itoa(port)), timeout, "ready"); err != nil {
		return fmt.Errorf("own listener: %w", err)
	}
	select {
	case <-ready:
	case <-time.After(timeout):
		return fmt.Errorf("own listener: no \"ready\" accepted within %s", timeout)
	}
	c, err := dial(control, timeout)
	if err != nil {
		return fmt.Errorf("control: %w", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.SetReadDeadline(deadline); err != nil {
		return err
	}
	line, err := bufio.NewReaderSize(io.LimitReader(c, 64), 64).ReadString('\n')
	if err != nil {
		return fmt.Errorf("control: no stop: %w", err)
	}
	if line != "stop\n" {
		return fmt.Errorf("control: %q, not stop", line)
	}
	return nil
}

// eth0Addr returns eth0's IPv4 address.
func eth0Addr() (netip.Addr, error) {
	ifi, err := net.InterfaceByName("eth0")
	if err != nil {
		return netip.Addr{}, err
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return netip.Addr{}, err
	}
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() {
			return p.Addr(), nil
		}
	}
	return netip.Addr{}, errors.New("eth0 has no IPv4 address")
}

// firstLine reads c's first line, of at most 64 bytes, within timeout, and
// closes c.
func firstLine(c net.Conn) (string, error) {
	defer func() { _ = c.Close() }()
	if err := c.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return "", err
	}
	line, err := bufio.NewReaderSize(io.LimitReader(c, 64), 64).ReadString('\n')
	return strings.TrimSuffix(line, "\n"), err
}

// ethPARP is ETH_P_ARP in network byte order, as AF_PACKET takes it.
var ethPARP = binary.NativeEndian.Uint16(binary.BigEndian.AppendUint16(nil, unix.ETH_P_ARP))

// arpSocket returns an AF_PACKET socket bound to eth0 that sends and
// receives ARP packets without their Ethernet header, and eth0.
func arpSocket() (int, *net.Interface, error) {
	ifi, err := net.InterfaceByName("eth0")
	if err != nil {
		return -1, nil, err
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, int(ethPARP))
	if err != nil {
		return -1, nil, fmt.Errorf("ARP socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: ethPARP, Ifindex: ifi.Index}); err != nil {
		return -1, nil, errors.Join(fmt.Errorf("ARP socket: %w", err), unix.Close(fd))
	}
	return fd, ifi, nil
}

// arpPacket is an ARP packet for IPv4 over Ethernet.
type arpPacket struct {
	op       uint16
	sha      net.HardwareAddr
	spa, tpa netip.Addr
}

// parseARP parses b as an ARP packet for IPv4 over Ethernet.
func parseARP(b []byte) (arpPacket, bool) {
	if len(b) < 28 || binary.BigEndian.Uint16(b[0:2]) != 1 || binary.BigEndian.Uint16(b[2:4]) != unix.ETH_P_IP || b[4] != 6 || b[5] != 4 {
		return arpPacket{}, false
	}
	return arpPacket{
		op:  binary.BigEndian.Uint16(b[6:8]),
		sha: net.HardwareAddr(slices.Clone(b[8:14])),
		spa: netip.AddrFrom4([4]byte(b[14:18])),
		tpa: netip.AddrFrom4([4]byte(b[24:28])),
	}, true
}

// arp broadcasts an ARP request for target from eth0's MAC, carrying
// sender as its sender address, once a second until target's reply to
// sender arrives, and fails if none does within wait. Every port of the
// switch eth0 is on gets the request, and the switch hands a broadcast to
// its other ports before its gateway, so the gateway's reply shows they
// have it.
func arp(sender, target string, wait time.Duration) error {
	spa, err := netip.ParseAddr(sender)
	if err != nil || !spa.Is4() {
		return fmt.Errorf("sender %q: not an IPv4 address", sender)
	}
	tpa, err := netip.ParseAddr(target)
	if err != nil || !tpa.Is4() {
		return fmt.Errorf("target %q: not an IPv4 address", target)
	}
	fd, ifi, err := arpSocket()
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	req := make([]byte, 28)
	binary.BigEndian.PutUint16(req[0:2], 1) // Ethernet
	binary.BigEndian.PutUint16(req[2:4], unix.ETH_P_IP)
	req[4], req[5] = 6, 4
	binary.BigEndian.PutUint16(req[6:8], 1) // request
	copy(req[8:14], ifi.HardwareAddr)
	copy(req[14:18], spa.AsSlice())
	copy(req[24:28], tpa.AsSlice())
	bcast := &unix.SockaddrLinklayer{Protocol: ethPARP, Ifindex: ifi.Index, Halen: 6, Addr: [8]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}
	buf := make([]byte, 1500)
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); {
		if err := unix.Sendto(fd, req, 0, bcast); err != nil {
			return fmt.Errorf("send the ARP request: %w", err)
		}
		for resend := time.Now().Add(time.Second); ; {
			left := min(time.Until(resend), time.Until(deadline))
			if left <= 0 {
				break
			}
			n, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, int(left.Milliseconds())+1) //nolint:gosec // G115: a descriptor fits in 32 bits
			if errors.Is(err, unix.EINTR) || n == 0 {
				continue
			}
			if err != nil {
				return err
			}
			size, _, err := unix.Recvfrom(fd, buf, unix.MSG_DONTWAIT)
			if errors.Is(err, unix.EAGAIN) {
				continue
			}
			if err != nil {
				return err
			}
			if p, ok := parseARP(buf[:size]); ok && p.op == 2 && p.spa == tpa && p.tpa == spa {
				fmt.Printf("arp reply %s is-at %s to %s\n", tpa, p.sha, spa)
				return nil
			}
		}
	}
	return fmt.Errorf("no ARP reply from %s to %s within %s", target, sender, wait)
}

// printARP prints every ARP packet waiting on fd, and fails if more than
// 256 are, since one it left unread could be the one a test looks for.
func printARP(fd int) error {
	buf := make([]byte, 1500)
	for range 256 {
		size, _, err := unix.Recvfrom(fd, buf, unix.MSG_DONTWAIT)
		if errors.Is(err, unix.EAGAIN) {
			return nil
		}
		if err != nil {
			return err
		}
		if p, ok := parseARP(buf[:size]); ok {
			fmt.Printf("arp op %d from %s %s -> %s\n", p.op, p.sha, p.spa, p.tpa)
		}
	}
	return errors.New("more than 256 ARP packets")
}

// lookup resolves name's IPv4 addresses with Go's own resolver, the probe
// being static, which reads /etc/hosts and /etc/resolv.conf as glibc and
// musl do, and prints each one as "address <addr>".
func lookup(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", name)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		fmt.Printf("address %s\n", ip.Unmap())
	}
	return nil
}

// ethPIP is ETH_P_IP in network byte order, as AF_PACKET takes it.
var ethPIP = binary.NativeEndian.Uint16(binary.BigEndian.AppendUint16(nil, unix.ETH_P_IP))

// rawUDP sends each target a UDP datagram from eth0's address, written on
// an AF_PACKET socket to the default gateway's MAC, so it leaves eth0
// whatever the guest's routing would do with its destination.
func rawUDP(targets []string) error {
	dsts := make([]netip.AddrPort, 0, len(targets))
	for _, t := range targets {
		dst, err := netip.ParseAddrPort(t)
		if err != nil || !dst.Addr().Is4() {
			return fmt.Errorf("%q: not an IPv4 address and port", t)
		}
		dsts = append(dsts, dst)
	}
	ifi, err := net.InterfaceByName("eth0")
	if err != nil {
		return err
	}
	src, err := eth0Addr()
	if err != nil {
		return err
	}
	gw, _, err := defaultRoute()
	if err != nil {
		return err
	}
	mac, err := neighbour(gw, timeout)
	if err != nil {
		return err
	}
	// Protocol 0: the socket sends and receives nothing.
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("packet socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	to := &unix.SockaddrLinklayer{Protocol: ethPIP, Ifindex: ifi.Index, Halen: 6}
	copy(to.Addr[:], mac)
	for _, dst := range dsts {
		if err := unix.Sendto(fd, udpPacket(src, dst), 0, to); err != nil {
			return fmt.Errorf("send to %s: %w", dst, err)
		}
		fmt.Printf("sent %s via %s\n", dst, mac)
	}
	return nil
}

// udpPacket returns an IPv4 packet from src holding a UDP datagram to dst,
// with no UDP checksum, which IPv4 allows.
func udpPacket(src netip.Addr, dst netip.AddrPort) []byte {
	payload := []byte("probe\n")
	b := make([]byte, 20+8+len(payload))
	// IPv4, a 20-byte header.
	b[0] = 0x45
	binary.BigEndian.PutUint16(b[2:4], uint16(len(b))) //nolint:gosec // G115: under 64 bytes
	b[8], b[9] = 64, unix.IPPROTO_UDP
	copy(b[12:16], src.AsSlice())
	copy(b[16:20], dst.Addr().AsSlice())
	var sum uint32
	for i := 0; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	for sum > 0xffff {
		sum = sum&0xffff + sum>>16
	}
	binary.BigEndian.PutUint16(b[10:12], ^uint16(sum))
	udp := b[20:]
	binary.BigEndian.PutUint16(udp[0:2], 40000)
	binary.BigEndian.PutUint16(udp[2:4], dst.Port())
	binary.BigEndian.PutUint16(udp[4:6], uint16(len(udp))) //nolint:gosec // G115: under 64 bytes
	copy(udp[8:], payload)
	return b
}

// neighbour returns addr's MAC from the kernel's ARP table, first sending
// addr a datagram to its discard port so the kernel resolves it, and fails
// if it isn't resolved within wait.
func neighbour(addr netip.Addr, wait time.Duration) (net.HardwareAddr, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	c, err := new(net.Dialer).DialContext(ctx, "udp4", net.JoinHostPort(addr.String(), "9"))
	if err != nil {
		return nil, err
	}
	_, err = c.Write([]byte{0})
	_ = c.Close()
	if err != nil {
		return nil, err
	}
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); <-tick.C {
		b, err := os.ReadFile("/proc/net/arp")
		if err != nil {
			return nil, err
		}
		// IP address, HW type, Flags, HW address, Mask, Device.
		for line := range strings.Lines(string(b)) {
			f := strings.Fields(line)
			if len(f) < 4 || f[0] != addr.String() || f[2] != "0x2" {
				continue
			}
			return net.ParseMAC(f[3])
		}
	}
	return nil, fmt.Errorf("%s's MAC: not resolved within %s", addr, wait)
}

// resolve asks server for name's A records over UDP or TCP, directly, so
// the answer can only have come from server, never from the image's
// /etc/hosts.
func resolve(name, server string, overTCP bool) error {
	network := "udp"
	if overTCP {
		network = "tcp"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, err := new(net.Dialer).DialContext(ctx, network, server)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if err := c.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	n, err := dnsmessage.NewName(strings.TrimSuffix(name, ".") + ".")
	if err != nil {
		return err
	}
	q := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 0x6262, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: n, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}
	b, err := q.Pack()
	if err != nil {
		return err
	}
	var answer []byte
	if overTCP {
		// RFC 1035 4.2.2: each message is prefixed with its length.
		if _, err := c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(b))), b...)); err != nil { //nolint:gosec // G115: a one-question message is far under 64 KiB
			return err
		}
		var size [2]byte
		if _, err := io.ReadFull(c, size[:]); err != nil {
			return err
		}
		answer = make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(c, answer); err != nil {
			return err
		}
	} else {
		if _, err := c.Write(b); err != nil {
			return err
		}
		buf := make([]byte, 1500)
		size, err := c.Read(buf)
		if err != nil {
			return err
		}
		answer = buf[:size]
	}
	var m dnsmessage.Message
	if err := m.Unpack(answer); err != nil {
		return err
	}
	if m.ID != q.ID || m.RCode != dnsmessage.RCodeSuccess {
		return fmt.Errorf("%s: id %#x, rcode %v", server, m.ID, m.RCode)
	}
	var found []string
	for _, a := range m.Answers {
		if r, ok := a.Body.(*dnsmessage.AResource); ok {
			found = append(found, netip.AddrFrom4(r.A).String())
		}
	}
	if len(found) == 0 {
		return fmt.Errorf("%s: no A record for %s", server, name)
	}
	fmt.Printf("resolved %s via %s/%s: %s\n", name, server, network, strings.Join(found, " "))
	return nil
}

// dhcp broadcasts count DHCP DISCOVERs on eth0, each from eth0's MAC with a
// transaction id of its own, and prints the address every reply to it
// offers. It reads replies to each until wait passes with none, and fails
// if a DISCOVER got none.
func dhcp(count int, wait time.Duration) error {
	ifi, err := net.InterfaceByName("eth0")
	if err != nil {
		return err
	}
	lc := net.ListenConfig{Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		err := rc.Control(func(fd uintptr) {
			serr = errors.Join(
				unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1),
				unix.BindToDevice(int(fd), ifi.Name),
			)
		})
		return errors.Join(err, serr)
	}}
	conn, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:68")
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	for i := range count {
		xid := 0x63630000 + uint32(i)
		if _, err := conn.WriteTo(discover(xid, ifi.HardwareAddr), &net.UDPAddr{IP: net.IPv4bcast, Port: 67}); err != nil {
			return err
		}
		replies := 0
		for {
			if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
				return err
			}
			buf := make([]byte, 1500)
			n, from, err := conn.ReadFrom(buf)
			if errors.Is(err, os.ErrDeadlineExceeded) {
				break
			}
			if err != nil {
				return err
			}
			r := buf[:n]
			if len(r) < 240 || r[0] != 2 || binary.BigEndian.Uint32(r[4:8]) != xid {
				continue
			}
			replies++
			fmt.Printf("offer xid=%#x from=%s yiaddr=%s\n", xid, from, netip.AddrFrom4([4]byte(r[16:20])))
		}
		if replies == 0 {
			return fmt.Errorf("DISCOVER %#x: no reply within %s", xid, wait)
		}
	}
	return nil
}

// discover returns a DHCPDISCOVER from mac with transaction id xid, asking
// for a broadcast reply.
func discover(xid uint32, mac net.HardwareAddr) []byte {
	b := make([]byte, 240, 300)
	b[0], b[1], b[2] = 1, 1, 6 // BOOTREQUEST, Ethernet, 6-byte address
	binary.BigEndian.PutUint32(b[4:8], xid)
	binary.BigEndian.PutUint16(b[10:12], 0x8000) // broadcast reply
	copy(b[28:], mac)
	copy(b[236:], []byte{99, 130, 83, 99}) // the magic cookie
	return append(b, 53, 1, 1, 255)        // DHCPDISCOVER, end
}

// vsock opens an AF_VSOCK stream connection to cid on every port in ranges
// and fails if any opens. The kernel bounds each connect (2 s by default).
func vsock(cid uint32, ranges []string) error {
	var ports []uint32
	for _, r := range ranges {
		p, err := portRange(r, 0, unix.VMADDR_PORT_ANY-1)
		if err != nil {
			return err
		}
		ports = append(ports, p...)
	}
	slices.Sort(ports)
	ports = slices.Compact(ports)
	// A guest that can't make a vsock socket at all must not pass.
	errNoSocket := errors.New("no vsock socket")
	errs := scan(ports, func(port uint32) error {
		fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("%w: %w", errNoSocket, err)
		}
		defer func() { _ = unix.Close(fd) }()
		return unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: port})
	})
	var opened []string
	reasons := map[string]int{}
	for _, p := range ports {
		err := errs[p]
		if errors.Is(err, errNoSocket) {
			return err
		}
		if err == nil {
			opened = append(opened, strconv.FormatUint(uint64(p), 10))
		} else {
			reasons[err.Error()]++
		}
	}
	fmt.Printf("vsock: %d of %d ports of CID %d failed: %v\n", len(ports)-len(opened), len(ports), cid, reasons)
	if len(opened) > 0 {
		return fmt.Errorf("connected to CID %d on ports %s", cid, strings.Join(opened, " "))
	}
	return nil
}

// netinfo prints lo's and eth0's flags, MTU and addresses, IPv4 and IPv6,
// and the default route, one line each.
func netinfo() error {
	for _, name := range []string{"lo", "eth0"} {
		ifi, err := net.InterfaceByName(name)
		if err != nil {
			return err
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			return err
		}
		var all []string
		for _, a := range addrs {
			all = append(all, a.String())
		}
		up := "down"
		if ifi.Flags&net.FlagUp != 0 {
			up = "up"
		}
		fmt.Printf("link %s %s mtu %d addrs %s\n", name, up, ifi.MTU, strings.Join(all, ","))
	}
	gw, dev, err := defaultRoute()
	if err != nil {
		return err
	}
	fmt.Printf("default via %s dev %s\n", gw, dev)
	return nil
}

// defaultRoute reads the IPv4 default route from /proc/net/route.
func defaultRoute() (netip.Addr, string, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return netip.Addr{}, "", err
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	for s.Scan() {
		// Iface Destination Gateway Flags ..., addresses in host order.
		fields := strings.Fields(s.Text())
		if len(fields) < 3 || fields[1] != "00000000" {
			continue
		}
		b, err := hex.DecodeString(fields[2])
		if err != nil || len(b) != 4 {
			continue
		}
		var a [4]byte
		binary.LittleEndian.PutUint32(a[:], binary.BigEndian.Uint32(b))
		return netip.AddrFrom4(a), fields[0], nil
	}
	if err := s.Err(); err != nil {
		return netip.Addr{}, "", err
	}
	return netip.Addr{}, "", errors.New("no default route")
}
