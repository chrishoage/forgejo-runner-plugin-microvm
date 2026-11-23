// SPDX-License-Identifier: GPL-3.0-or-later

package jobnet

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// The job network the tests run: the plugin's addressing for a job's
// container.
var (
	subnet  = netip.MustParsePrefix("198.18.0.0/24")
	gateway = netip.MustParseAddr("198.18.0.1")
	gwMAC   = net.HardwareAddr{0x02, 0x00, 0xc6, 0x12, 0x00, 0x01}
	vmAddr  = netip.MustParseAddr("198.18.0.2")
	vmMAC   = net.HardwareAddr{0x02, 0x00, 0xc6, 0x12, 0x00, 0x02}
)

func testConfig() Config {
	return Config{Subnet: subnet, Gateway: gateway, GatewayMAC: gwMAC, MTU: 1500, VMs: []VM{{vmAddr, vmMAC}}}
}

// guest is the test peer playing a VM: a second gvisor netstack on the
// VM's end of the frame socket, addressed as the agent addresses a guest.
type guest struct {
	s *stack.Stack
}

// startGuest starts a job network and attaches a guest to it.
func startGuest(t *testing.T) *guest {
	t.Helper()
	n, err := New(testConfig(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	vmEnd, err := n.Attach(vmMAC)
	if err != nil {
		t.Fatal(err)
	}
	// fdbased takes the descriptor, so it gets its own copy.
	fd, err := dup(vmEnd)
	if err != nil {
		t.Fatal(err)
	}
	if err := vmEnd.Close(); err != nil {
		t.Fatal(err)
	}
	ep, err := fdbased.New(&fdbased.Options{FDs: []int{fd}, MTU: 1500, EthernetHeader: true, Address: tcpip.LinkAddress(vmMAC)})
	if err != nil {
		t.Fatal(err)
	}
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	if err := s.CreateNIC(1, ep); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr(vmAddr), PrefixLen: subnet.Bits()},
	}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	// An ordinary /24 with a default route via the gateway, as the agent
	// configures eth0.
	onLink, err := tcpip.NewSubnet(addr(subnet.Addr()), tcpip.MaskFromBytes(net.CIDRMask(subnet.Bits(), 32)))
	if err != nil {
		t.Fatal(err)
	}
	s.SetRouteTable([]tcpip.Route{
		{Destination: onLink, NIC: 1},
		{Destination: header.IPv4EmptySubnet, Gateway: addr(gateway), NIC: 1},
	})
	t.Cleanup(func() {
		s.Close()
		s.Wait()
		if err := n.Close(); err != nil {
			t.Error(err)
		}
	})
	return &guest{s: s}
}

// dup returns a close-on-exec copy of f's descriptor.
func dup(f *os.File) (int, error) {
	return unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
}

func addr(a netip.Addr) tcpip.Address { return tcpip.AddrFrom4(a.As4()) }

// dial connects from the guest to to over TCP, giving up after timeout.
func (g *guest) dial(t *testing.T, to netip.AddrPort, timeout time.Duration) (net.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	return gonet.DialContextTCP(ctx, g.s, tcpip.FullAddress{NIC: 1, Addr: addr(to.Addr()), Port: to.Port()}, ipv4.ProtocolNumber)
}

// hostAddr returns a non-loopback, non-link-local IPv4 address of the test
// host.
func hostAddr(t *testing.T) netip.Addr {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		p, err := netip.ParsePrefix(a.String())
		if err != nil {
			continue
		}
		if ip := p.Addr(); ip.Is4() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			return ip
		}
	}
	t.Fatal("the test host has no non-loopback IPv4 address")
	return netip.Addr{}
}

// listener is a TCP listener on the host that records whom it accepted.
type listener struct {
	ln   net.Listener
	mu   sync.Mutex
	from []netip.Addr
	seen chan struct{}
}

func listen(t *testing.T, at netip.Addr) *listener {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp4", netip.AddrPortFrom(at, 0).String())
	if err != nil {
		t.Fatal(err)
	}
	l := &listener{ln: ln, seen: make(chan struct{}, 1)}
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
	return l
}

func (l *listener) port() uint16 { return l.ln.Addr().(*net.TCPAddr).AddrPort().Port() }

func (l *listener) accepted() []netip.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.from)
}

// A name the host resolves gets an answer from the gateway's DNS server,
// which asks the host's resolver (NET-5).
func TestGatewayAnswersNamesTheHostResolves(t *testing.T) {
	t.Parallel()
	g := startGuest(t)
	conn, err := gonet.DialUDP(g.s, nil, &tcpip.FullAddress{NIC: 1, Addr: addr(gateway), Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	q := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 4242, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("localhost."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}
	b, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(b); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("no answer from the gateway: %v", err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(buf[:n]); err != nil {
		t.Fatal(err)
	}
	if m.ID != 4242 || m.RCode != dnsmessage.RCodeSuccess {
		t.Fatalf("answer: id %d, rcode %v", m.ID, m.RCode)
	}
	for _, a := range m.Answers {
		if r, ok := a.Body.(*dnsmessage.AResource); ok && netip.AddrFrom4(r.A).IsLoopback() {
			return
		}
	}
	t.Errorf("answer holds no A record for localhost: %v", m.Answers)
}

// A connection to a listener on the host's non-loopback address succeeds,
// and leaves the host as the job process's own: the listener sees it come
// from the host, not from the job network (NET-5).
func TestGuestConnectsOut(t *testing.T) {
	t.Parallel()
	host := hostAddr(t)
	l := listen(t, host)
	g := startGuest(t)
	c, err := g.dial(t, netip.AddrPortFrom(host, l.port()), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	select {
	case <-l.seen:
	case <-time.After(10 * time.Second):
		t.Fatal("the listener saw no connection")
	}
	from := l.accepted()
	if len(from) != 1 || subnet.Contains(from[0]) || !isHostAddr(t, from[0]) {
		t.Errorf("the listener accepted from %v, want one of the host's addresses", from)
	}
}

func isHostAddr(t *testing.T, a netip.Addr) bool {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range addrs {
		if p, err := netip.ParsePrefix(h.String()); err == nil && p.Addr() == a {
			return true
		}
	}
	return false
}

// A listener on the host's loopback can't be reached at the gateway's
// address, at another address of the subnet, or at 127.0.0.1 (NET-4).
func TestGuestCantReachHostLoopback(t *testing.T) {
	t.Parallel()
	l := listen(t, netip.MustParseAddr("127.0.0.1"))
	g := startGuest(t)
	for _, to := range []string{"198.18.0.1", "198.18.0.3", "198.18.0.254", "127.0.0.1"} {
		c, err := g.dial(t, netip.AddrPortFrom(netip.MustParseAddr(to), l.port()), time.Second)
		if err == nil {
			_ = c.Close()
			t.Errorf("%s: connected", to)
		}
	}
	if from := l.accepted(); len(from) != 0 {
		t.Errorf("the loopback listener accepted from %v", from)
	}
}

// A packet a root guest crafts itself, to 0.0.0.0, which a host dial or send
// takes as its own loopback, or to a loopback address, reaches no listener
// on the host's 127.0.0.1, over TCP or UDP (NET-4): the guard keeps every
// destination that isn't outside unicast from the gateway's forwarders. The
// same packets to the host's non-loopback address are the positive control.
func TestGuardKeepsTheForwardersOffTheHostsLoopback(t *testing.T) {
	t.Parallel()
	host := hostAddr(t)
	lo, ctl := listen(t, netip.MustParseAddr("127.0.0.1")), listen(t, host)
	loUDP, ctlUDP := listenUDP(t, netip.MustParseAddr("127.0.0.1")), listenUDP(t, host)
	vmEnd := attachRaw(t)
	for _, to := range []string{"0.0.0.0", "127.0.0.1"} {
		for _, f := range [][]byte{syn(to, lo.port()), datagram(to, loUDP.port())} {
			if _, err := vmEnd.Write(f); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, f := range [][]byte{syn(host.String(), ctl.port()), datagram(host.String(), ctlUDP.port())} {
		if _, err := vmEnd.Write(f); err != nil {
			t.Fatal(err)
		}
	}
	for name, seen := range map[string]chan struct{}{"TCP": ctl.seen, "UDP": ctlUDP.seen} {
		select {
		case <-seen:
		case <-time.After(10 * time.Second):
			t.Fatalf("the %s control reached no listener", name)
		}
	}
	waitLoopback(lo.seen, loUDP.seen)
	if from := lo.accepted(); len(from) != 0 {
		t.Errorf("the loopback TCP listener accepted from %v", from)
	}
	if from := loUDP.received(); len(from) != 0 {
		t.Errorf("the loopback UDP listener received from %v", from)
	}
}

// An echo request to the gateway gets one echo reply, the guard's, and the
// gateway's stack never sees it: the stack would ask the VM for its MAC
// first, and its ICMP forwarder would ping the gateway's address from the
// host.
func TestGatewayAnswersAnEchoItself(t *testing.T) {
	t.Parallel()
	vm, err := net.FileConn(attachRaw(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vm.Close() })
	req := frame(gwMAC, gateway.String(), header.ICMPv4ProtocolNumber, icmp(header.ICMPv4Echo))
	ip := header.IPv4(req[header.EthernetMinimumSize:])
	ip.SetChecksum(^ip.CalculateChecksum())
	echo := header.ICMPv4(ip.Payload())
	echo.SetChecksum(^checksum.Checksum(echo, 0))
	if _, err := vm.Write(req); err != nil {
		t.Fatal(err)
	}
	var got []header.ICMPv4Type
	buf := make([]byte, 1500)
	// One reply is written before Write returns; a second frame, from the
	// stack, would follow within a millisecond.
	if err := vm.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		n, err := vm.Read(buf)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		eth := header.Ethernet(buf[:n])
		if eth.Type() != header.IPv4ProtocolNumber {
			t.Errorf("the VM got a frame of type %#x", eth.Type())
			continue
		}
		got = append(got, header.ICMPv4(header.IPv4(buf[header.EthernetMinimumSize:n]).Payload()).Type())
	}
	if !slices.Equal(got, []header.ICMPv4Type{header.ICMPv4EchoReply}) {
		t.Errorf("the VM got ICMP %v, want one echo reply", got)
	}
}

// waitLoopback waits for either loopback listener to see a packet sent before
// the controls, which have arrived. The switch hands the VM's frames to the
// gateway in order, but the gateway's forwarders dial each one's
// destination in a goroutine of their own, and the listeners record what
// they see in theirs: a control arriving doesn't mean a dial for an earlier
// packet has finished. A dial or send to the host's loopback, had one been
// made, finishes in well under a millisecond, so one still to come shows up
// within loopbackWindow of the controls, which is far longer.
func waitLoopback(tcp, udp chan struct{}) {
	const loopbackWindow = 2 * time.Second
	select {
	case <-tcp:
	case <-udp:
	case <-time.After(loopbackWindow):
	}
}

// attachRaw starts a job network and returns the VM's end of its frame
// socket, for a test that writes frames itself, as a root guest can.
func attachRaw(t *testing.T) *os.File {
	t.Helper()
	n, err := New(testConfig(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	vmEnd, err := n.Attach(vmMAC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = vmEnd.Close()
		if err := n.Close(); err != nil {
			t.Error(err)
		}
	})
	return vmEnd
}

// syn returns a frame from the VM to the gateway's MAC holding a TCP SYN to
// dst:port, with valid checksums.
func syn(dst string, port uint16) []byte {
	b := frame(gwMAC, dst, header.TCPProtocolNumber, tcpSegment(port, header.TCPFlagSyn))
	ip := header.IPv4(b[header.EthernetMinimumSize:])
	ip.SetChecksum(^ip.CalculateChecksum())
	seg := header.TCP(ip.Payload())
	xsum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, ip.SourceAddress(), ip.DestinationAddress(), uint16(len(seg)))
	seg.SetChecksum(^seg.CalculateChecksum(xsum))
	return b
}

// datagram returns a frame from the VM to the gateway's MAC holding a UDP
// datagram to dst:port, with a valid IPv4 checksum and none for UDP, which
// IPv4 allows.
func datagram(dst string, port uint16) []byte {
	payload := []byte("probe")
	d := make([]byte, header.UDPMinimumSize+len(payload))
	header.UDP(d).Encode(&header.UDPFields{SrcPort: 40000, DstPort: port, Length: uint16(len(d))})
	copy(d[header.UDPMinimumSize:], payload)
	b := frame(gwMAC, dst, header.UDPProtocolNumber, d)
	ip := header.IPv4(b[header.EthernetMinimumSize:])
	ip.SetChecksum(^ip.CalculateChecksum())
	return b
}

// udpListener is a UDP socket on the host that records whom it received
// datagrams from.
type udpListener struct {
	conn net.PacketConn
	mu   sync.Mutex
	from []netip.Addr
	seen chan struct{}
}

func listenUDP(t *testing.T, at netip.Addr) *udpListener {
	t.Helper()
	conn, err := new(net.ListenConfig).ListenPacket(t.Context(), "udp4", netip.AddrPortFrom(at, 0).String())
	if err != nil {
		t.Fatal(err)
	}
	l := &udpListener{conn: conn, seen: make(chan struct{}, 1)}
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

func (l *udpListener) port() uint16 { return l.conn.LocalAddr().(*net.UDPAddr).AddrPort().Port() }

func (l *udpListener) received() []netip.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.from)
}

// Every TCP port on the gateway refuses, except the DNS server's 53
// (NET-6).
func TestGatewayRefusesEveryTCPPortButDNS(t *testing.T) {
	t.Parallel()
	g := startGuest(t)
	c, err := g.dial(t, netip.AddrPortFrom(gateway, 53), 10*time.Second)
	if err != nil {
		t.Fatalf("port 53: %v", err)
	}
	_ = c.Close()
	ports := make(chan uint16)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var open []uint16
	for range 64 {
		wg.Go(func() {
			for p := range ports {
				c, err := g.dial(t, netip.AddrPortFrom(gateway, p), 10*time.Second)
				if err == nil {
					_ = c.Close()
				}
				if !isRefused(err) {
					mu.Lock()
					open = append(open, p)
					mu.Unlock()
				}
			}
		})
	}
	for p := 1; p <= 65535; p++ {
		if p != 53 {
			ports <- uint16(p)
		}
	}
	close(ports)
	wg.Wait()
	if len(open) > 0 {
		slices.Sort(open)
		t.Errorf("%d gateway ports did not refuse, the first %v", len(open), open[:min(len(open), 10)])
	}
}

// isRefused reports whether err is a refused connection.
func isRefused(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Err != nil && op.Err.Error() == (&tcpip.ErrConnectionRefused{}).String()
}

// A DHCP DISCOVER from the VM's MAC is answered only with the VM's own
// address (NET-6).
func TestDHCPOffersOnlyTheVMsAddress(t *testing.T) {
	t.Parallel()
	g := startGuest(t)
	var wq waiter.Queue
	ep, terr := g.s.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if terr != nil {
		t.Fatal(terr)
	}
	ep.SocketOptions().SetBroadcast(true)
	if terr := ep.Bind(tcpip.FullAddress{NIC: 1, Port: 68}); terr != nil {
		t.Fatal(terr)
	}
	conn := gonet.NewUDPConn(&wq, ep)
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for xid := range uint32(3) {
		if _, err := conn.WriteTo(discover(xid, vmMAC), &net.UDPAddr{IP: net.IPv4bcast, Port: 67}); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 1500)
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			t.Fatalf("no DHCP reply: %v", err)
		}
		reply := buf[:n]
		if len(reply) < 240 || reply[0] != 2 || binary.BigEndian.Uint32(reply[4:8]) != xid {
			t.Fatalf("reply %d is not a BOOTREPLY to %d: % x", xid, xid, reply[:min(len(reply), 8)])
		}
		if got := netip.AddrFrom4([4]byte(reply[16:20])); got != vmAddr {
			t.Errorf("DISCOVER %d: offered %s, want the VM's own %s", xid, got, vmAddr)
		}
	}
}

// discover returns a DHCPDISCOVER from mac with transaction id xid.
func discover(xid uint32, mac net.HardwareAddr) []byte {
	b := make([]byte, 240, 300)
	b[0], b[1], b[2] = 1, 1, 6 // BOOTREQUEST, Ethernet, 6-byte address
	binary.BigEndian.PutUint32(b[4:8], xid)
	binary.BigEndian.PutUint16(b[10:12], 0x8000) // broadcast reply
	copy(b[28:], mac)
	copy(b[236:], []byte{99, 130, 83, 99}) // the magic cookie
	return append(b, 53, 1, 1, 255)        // DHCPDISCOVER, end
}

// A connection to the link-local metadata address is refused (NET-5).
func TestLinkLocalIsRefused(t *testing.T) {
	t.Parallel()
	g := startGuest(t)
	c, err := g.dial(t, netip.MustParseAddrPort("169.254.169.254:80"), 10*time.Second)
	if err == nil {
		_ = c.Close()
		t.Fatal("connected to 169.254.169.254")
	}
	if !isRefused(err) {
		t.Errorf("err = %v, want refused", err)
	}
}

// New refuses a config the library would take but serve wrongly.
func TestNewRefusesABadConfig(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*Config){
		"an IPv6 subnet":          func(c *Config) { c.Subnet = netip.MustParsePrefix("fd00::/64") },
		"an unmasked subnet":      func(c *Config) { c.Subnet = netip.MustParsePrefix("198.18.0.1/24") },
		"a gateway off it":        func(c *Config) { c.Gateway = netip.MustParseAddr("198.19.0.1") },
		"a short gateway MAC":     func(c *Config) { c.GatewayMAC = c.GatewayMAC[:4] },
		"no MTU":                  func(c *Config) { c.MTU = 0 },
		"a VM off the subnet":     func(c *Config) { c.VMs[0].Address = netip.MustParseAddr("10.0.0.2") },
		"a VM at the gateway":     func(c *Config) { c.VMs[0].Address = gateway },
		"a VM with the gateway's": func(c *Config) { c.VMs[0].MAC = gwMAC },
		"two VMs at one address":  func(c *Config) { c.VMs = append(c.VMs, VM{vmAddr, net.HardwareAddr{2, 0, 0, 0, 0, 9}}) },
	} {
		cfg := testConfig()
		change(&cfg)
		if n, err := New(cfg, slog.New(slog.DiscardHandler)); err == nil {
			_ = n.Close()
			t.Errorf("%s: no error", name)
		}
	}
}

// Close stops serving every frame socket and waits for it; a VM can't
// attach afterwards.
func TestCloseStopsServing(t *testing.T) {
	t.Parallel()
	n, err := New(testConfig(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	vmEnd, err := n.Attach(vmMAC)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = vmEnd.Close() }()
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := vmEnd.Write(make([]byte, 60)); err == nil {
		t.Error("the switch's end still takes frames after Close")
	}
	if f, err := n.Attach(vmMAC); err == nil {
		_ = f.Close()
		t.Error("Attach after Close succeeded")
	}
}

// Only a VM of the config attaches, by its MAC.
func TestAttachRefusesAnUnknownMAC(t *testing.T) {
	t.Parallel()
	n, err := New(testConfig(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })
	if f, err := n.Attach(gwMAC); err == nil {
		_ = f.Close()
		t.Error("the gateway's MAC attached")
	}
}
