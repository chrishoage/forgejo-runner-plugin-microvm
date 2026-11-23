// SPDX-License-Identifier: GPL-3.0-or-later

package jobnet

import (
	"net"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// written is a switch port that records what the guard writes to the VM.
type written struct {
	net.Conn
	frames [][]byte
}

func (w *written) Write(b []byte) (int, error) {
	w.frames = append(w.frames, append([]byte(nil), b...))
	return len(b), nil
}

func testGuard() (*guard, *written) {
	w := &written{}
	return newGuard(w, vmMAC, testConfig()), w
}

// frame returns an Ethernet frame from the VM to dstMAC holding an IPv4
// packet to dst of protocol proto, whose payload is transport.
func frame(dstMAC net.HardwareAddr, dst string, proto tcpip.TransportProtocolNumber, transport []byte) []byte {
	b := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize+len(transport))
	header.Ethernet(b).Encode(&header.EthernetFields{SrcAddr: tcpip.LinkAddress(vmMAC), DstAddr: tcpip.LinkAddress(dstMAC), Type: header.IPv4ProtocolNumber})
	ip := header.IPv4(b[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(header.IPv4MinimumSize + len(transport)),
		TTL:         64, Protocol: uint8(proto), SrcAddr: addr(vmAddr), DstAddr: addr(netip.MustParseAddr(dst)),
	})
	copy(ip.Payload(), transport)
	return b
}

func tcpSegment(port uint16, flags header.TCPFlags) []byte {
	b := make([]byte, header.TCPMinimumSize)
	header.TCP(b).Encode(&header.TCPFields{SrcPort: 40000, DstPort: port, SeqNum: 1000, AckNum: 77, DataOffset: header.TCPMinimumSize, Flags: flags})
	return b
}

// icmp returns an ICMP message of type typ.
func icmp(typ header.ICMPv4Type) []byte {
	b := make([]byte, header.ICMPv4MinimumSize)
	header.ICMPv4(b).SetType(typ)
	return b
}

func udpDatagram(port uint16) []byte {
	b := make([]byte, header.UDPMinimumSize)
	header.UDP(b).Encode(&header.UDPFields{SrcPort: 40000, DstPort: port, Length: header.UDPMinimumSize})
	return b
}

// The guard passes frames between VMs and to the outside, the gateway's
// DNS and DHCP, and echo requests, and drops what the gateway would forward
// to an address of the subnet, other ICMP, and frames from another MAC.
func TestGuardPasses(t *testing.T) {
	t.Parallel()
	otherVM := net.HardwareAddr{0x02, 0x00, 0xc6, 0x12, 0x00, 0x03}
	bcast := net.HardwareAddr(header.EthernetBroadcastAddress)
	arp := frame(gwMAC, "198.18.0.1", 0, nil)
	header.Ethernet(arp).Encode(&header.EthernetFields{SrcAddr: tcpip.LinkAddress(vmMAC), DstAddr: tcpip.LinkAddress(bcast), Type: header.ARPProtocolNumber})
	spoofed := frame(gwMAC, "192.0.2.1", header.TCPProtocolNumber, tcpSegment(22, header.TCPFlagSyn))
	copy(header.Ethernet(spoofed)[6:12], []byte{0x02, 0x00, 0xde, 0xad, 0xbe, 0xef})
	fragment := frame(gwMAC, "192.0.2.1", header.ICMPv4ProtocolNumber, icmp(header.ICMPv4Echo))
	header.IPv4(fragment[header.EthernetMinimumSize:]).SetFlagsFragmentOffset(header.IPv4FlagMoreFragments, 0)
	badOffset := tcpSegment(22, header.TCPFlagSyn)
	badOffset[12] = 0xf0 // a data offset of 60 bytes in a 20-byte segment
	for _, tt := range []struct {
		name  string
		frame []byte
		want  bool
	}{
		{"a short frame", make([]byte, 13), false},
		{"ARP", arp, true},
		{"to another VM", frame(otherVM, "198.18.0.3", header.TCPProtocolNumber, tcpSegment(22, header.TCPFlagSyn)), true},
		{"to the outside", frame(gwMAC, "192.0.2.1", header.TCPProtocolNumber, tcpSegment(22, header.TCPFlagSyn)), true},
		{"DNS over UDP", frame(gwMAC, "198.18.0.1", header.UDPProtocolNumber, udpDatagram(53)), true},
		{"DNS over TCP", frame(gwMAC, "198.18.0.1", header.TCPProtocolNumber, tcpSegment(53, header.TCPFlagSyn)), true},
		{"DHCP to the gateway", frame(gwMAC, "198.18.0.1", header.UDPProtocolNumber, udpDatagram(67)), true},
		{"DHCP broadcast", frame(bcast, "255.255.255.255", header.UDPProtocolNumber, udpDatagram(67)), true},
		{"a source MAC not the VM's", spoofed, false},
		{"an echo request to the gateway", frame(gwMAC, "198.18.0.1", header.ICMPv4ProtocolNumber, icmp(header.ICMPv4Echo)), false},
		{"an echo request to the outside", frame(gwMAC, "192.0.2.1", header.ICMPv4ProtocolNumber, icmp(header.ICMPv4Echo)), true},
		{"an echo reply to the gateway", frame(gwMAC, "198.18.0.1", header.ICMPv4ProtocolNumber, icmp(header.ICMPv4EchoReply)), false},
		{"an echo reply to the outside", frame(gwMAC, "192.0.2.1", header.ICMPv4ProtocolNumber, icmp(header.ICMPv4EchoReply)), false},
		{"unreachable to the outside", frame(gwMAC, "192.0.2.1", header.ICMPv4ProtocolNumber, icmp(header.ICMPv4DstUnreachable)), false},
		{"a short ICMP message to the outside", frame(gwMAC, "192.0.2.1", header.ICMPv4ProtocolNumber, icmp(header.ICMPv4Echo)[:4]), false},
		{"a fragment of an echo request", fragment, false},
		{"UDP to another gateway port", frame(gwMAC, "198.18.0.1", header.UDPProtocolNumber, udpDatagram(123)), false},
		{"TCP to another gateway port", frame(gwMAC, "198.18.0.1", header.TCPProtocolNumber, tcpSegment(22, header.TCPFlagAck)), false},
		{"a subnet address through the gateway", frame(gwMAC, "198.18.0.7", header.TCPProtocolNumber, tcpSegment(53, header.TCPFlagSyn)), false},
		{"the subnet's broadcast", frame(bcast, "198.18.0.255", header.UDPProtocolNumber, udpDatagram(53)), false},
		{"a bad data offset", frame(gwMAC, "198.18.0.1", header.TCPProtocolNumber, badOffset), false},
		{"UDP broadcast to another port", frame(bcast, "255.255.255.255", header.UDPProtocolNumber, udpDatagram(53)), false},
		{"TCP to the limited broadcast", frame(bcast, "255.255.255.255", header.TCPProtocolNumber, tcpSegment(67, header.TCPFlagSyn)), false},
		{"an echo request to the limited broadcast", frame(bcast, "255.255.255.255", header.ICMPv4ProtocolNumber, icmp(header.ICMPv4Echo)), false},
		{"a truncated IPv4 header", frame(gwMAC, "198.18.0.1", header.UDPProtocolNumber, nil)[:header.EthernetMinimumSize+10], false},
	} {
		g, _ := testGuard()
		if got := g.pass(tt.frame); got != tt.want {
			t.Errorf("%s: pass = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// The guard keeps from the gateway every packet to a destination that isn't
// outside unicast, of every protocol: the forwarders would send it from the
// host, where 0.0.0.0 is the host's own loopback.
func TestGuardDropsWhatIsntUnicast(t *testing.T) {
	t.Parallel()
	bcast := net.HardwareAddr(header.EthernetBroadcastAddress)
	for _, dst := range []string{"0.0.0.0", "0.1.2.3", "127.0.0.1", "127.1.2.3", "224.0.0.251", "239.255.255.250", "240.0.0.1", "255.255.255.254"} {
		for name, transport := range map[string]struct {
			proto tcpip.TransportProtocolNumber
			b     []byte
		}{
			"TCP":  {header.TCPProtocolNumber, tcpSegment(80, header.TCPFlagSyn)},
			"UDP":  {header.UDPProtocolNumber, udpDatagram(80)},
			"ICMP": {header.ICMPv4ProtocolNumber, icmp(header.ICMPv4Echo)},
			"GRE":  {47, make([]byte, 8)},
		} {
			for _, mac := range []net.HardwareAddr{gwMAC, bcast} {
				g, w := testGuard()
				if g.pass(frame(mac, dst, transport.proto, transport.b)) {
					t.Errorf("%s to %s, to MAC %s: passed", name, dst, mac)
				}
				if len(w.frames) != 0 {
					t.Errorf("%s to %s, to MAC %s: answered", name, dst, mac)
				}
			}
		}
	}
}

// A SYN to a gateway port other than DNS's gets a reset from the gateway,
// acknowledging the SYN; a reset gets no answer.
func TestGuardResetsAGatewayPort(t *testing.T) {
	t.Parallel()
	g, w := testGuard()
	if g.pass(frame(gwMAC, "198.18.0.1", header.TCPProtocolNumber, tcpSegment(22, header.TCPFlagSyn))) {
		t.Fatal("the SYN passed")
	}
	if len(w.frames) != 1 {
		t.Fatalf("wrote %d frames, want one reset", len(w.frames))
	}
	eth := header.Ethernet(w.frames[0])
	if eth.SourceAddress() != tcpip.LinkAddress(gwMAC) || eth.DestinationAddress() != tcpip.LinkAddress(vmMAC) {
		t.Errorf("reset from %s to %s", eth.SourceAddress(), eth.DestinationAddress())
	}
	ip := header.IPv4(w.frames[0][header.EthernetMinimumSize:])
	if !ip.IsValid(len(ip)) || !ip.IsChecksumValid() || ip.SourceAddress() != addr(gateway) || ip.DestinationAddress() != addr(vmAddr) {
		t.Fatalf("reset's IPv4 header: valid %v, checksum %v, %s to %s", ip.IsValid(len(ip)), ip.IsChecksumValid(), ip.SourceAddress(), ip.DestinationAddress())
	}
	tcp := header.TCP(ip.Payload())
	if !tcp.IsChecksumValid(ip.SourceAddress(), ip.DestinationAddress(), 0, 0) {
		t.Error("reset's TCP checksum is wrong")
	}
	if tcp.SourcePort() != 22 || tcp.DestinationPort() != 40000 || tcp.Flags() != header.TCPFlagRst|header.TCPFlagAck || tcp.AckNumber() != 1001 || tcp.SequenceNumber() != 0 {
		t.Errorf("reset: ports %d to %d, flags %v, seq %d, ack %d", tcp.SourcePort(), tcp.DestinationPort(), tcp.Flags(), tcp.SequenceNumber(), tcp.AckNumber())
	}

	g, w = testGuard()
	g.pass(frame(gwMAC, "198.18.0.1", header.TCPProtocolNumber, tcpSegment(22, header.TCPFlagRst)))
	if len(w.frames) != 0 {
		t.Error("a reset was answered")
	}
}

// A flood of echo requests through the gateway passes only echoBurst at
// once and echoRate a second after that.
func TestGuardBoundsEchoRequests(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		g, _ := testGuard()
		ping := frame(gwMAC, "192.0.2.1", header.ICMPv4ProtocolNumber, icmp(header.ICMPv4Echo))
		count := func() (n int) {
			for range 1000 {
				if g.pass(ping) {
					n++
				}
			}
			return n
		}
		if n := count(); n != echoBurst {
			t.Errorf("at once: %d passed, want %d", n, echoBurst)
		}
		time.Sleep(time.Second)
		if n := count(); n != echoRate {
			t.Errorf("a second later: %d passed, want %d", n, echoRate)
		}
	})
}

// An echo request to the gateway is answered by the guard, from the
// gateway, and never reaches the gateway's stack, whose ICMP forwarder
// would also ping the gateway's address from the host.
func TestGuardAnswersAnEchoToTheGateway(t *testing.T) {
	t.Parallel()
	g, w := testGuard()
	req := make([]byte, header.ICMPv4MinimumSize+5)
	echo := header.ICMPv4(req)
	echo.SetType(header.ICMPv4Echo)
	echo.SetIdent(7)
	echo.SetSequence(9)
	copy(echo.Payload(), "hello")
	if g.pass(frame(gwMAC, "198.18.0.1", header.ICMPv4ProtocolNumber, req)) {
		t.Fatal("the echo request passed")
	}
	if len(w.frames) != 1 {
		t.Fatalf("wrote %d frames, want one echo reply", len(w.frames))
	}
	eth := header.Ethernet(w.frames[0])
	if eth.SourceAddress() != tcpip.LinkAddress(gwMAC) || eth.DestinationAddress() != tcpip.LinkAddress(vmMAC) || eth.Type() != header.IPv4ProtocolNumber {
		t.Errorf("reply from %s to %s, type %#x", eth.SourceAddress(), eth.DestinationAddress(), eth.Type())
	}
	ip := header.IPv4(w.frames[0][header.EthernetMinimumSize:])
	if !ip.IsValid(len(ip)) || !ip.IsChecksumValid() || ip.SourceAddress() != addr(gateway) || ip.DestinationAddress() != addr(vmAddr) || ip.TransportProtocol() != header.ICMPv4ProtocolNumber {
		t.Fatalf("reply's IPv4 header: valid %v, checksum %v, %s to %s", ip.IsValid(len(ip)), ip.IsChecksumValid(), ip.SourceAddress(), ip.DestinationAddress())
	}
	reply := header.ICMPv4(ip.Payload())
	if reply.Type() != header.ICMPv4EchoReply || reply.Code() != 0 || reply.Ident() != 7 || reply.Sequence() != 9 || string(reply.Payload()) != "hello" {
		t.Errorf("reply: type %d, code %d, ident %d, seq %d, payload %q", reply.Type(), reply.Code(), reply.Ident(), reply.Sequence(), reply.Payload())
	}
	if checksum.Checksum(reply, 0) != 0xffff {
		t.Error("reply's ICMP checksum is wrong")
	}

	// Past the rate, an echo request to the gateway gets no answer.
	g, w = testGuard()
	for range echoBurst + 5 {
		g.pass(frame(gwMAC, "198.18.0.1", header.ICMPv4ProtocolNumber, icmp(header.ICMPv4Echo)))
	}
	if len(w.frames) != echoBurst {
		t.Errorf("answered %d of a flood at once, want %d", len(w.frames), echoBurst)
	}
}
