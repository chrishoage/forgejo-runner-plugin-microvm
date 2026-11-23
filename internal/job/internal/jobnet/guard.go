// SPDX-License-Identifier: GPL-3.0-or-later

package jobnet

import (
	"net"
	"net/netip"
	"slices"

	"golang.org/x/time/rate"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// The gateway's own services: DNS on UDP and TCP, and the DHCP server no
// VM uses (NET-6).
const (
	dnsPort  = 53
	dhcpPort = 67
)

// The ICMP echo requests a VM may send through the gateway: a burst of
// echoBurst, then echoRate a second. The library answers each from a host
// ping socket and a goroutine of its own, held up to 5 seconds for the
// reply, so a VM holds at most echoBurst + 5 × echoRate of each.
const (
	echoRate  = 10
	echoBurst = 10
)

// notUnicast are the IPv4 destinations that are no single host's on the
// host's networks: "this network" (0.0.0.0/8, whose 0.0.0.0 a host dial or
// send takes as its own loopback), loopback, multicast, and the reserved
// 240.0.0.0/4 with the limited broadcast in it.
var notUnicast = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// limitedBroadcast is where a DHCP client sends.
var limitedBroadcast = netip.AddrFrom4([4]byte{255, 255, 255, 255})

// guard is a VM's switch port, reading the VM's frames for the switch. Of
// the frames a VM sends the gateway, it passes to the gateway's forwarders
// only those to an outside unicast address: the forwarders dial or send
// each packet's destination from the host, so a packet to the gateway's
// address, or to another address of the subnet, would otherwise leave the
// host for whatever has that address on the host's networks, and one to
// 0.0.0.0 would reach the host's own loopback. To the gateway itself it
// passes DNS (UDP and TCP) and DHCP, answers a TCP connection to any other
// port with a reset, as a host with nothing listening does, and an echo
// request with an echo reply of its own, since the library's ICMP
// forwarder would also ping the gateway's address from the host. To the
// limited broadcast it passes only DHCP. Of ICMP to the outside it passes
// only unfragmented echo requests, within the rate above, which also bounds
// the echo replies it makes: the library logs every other ICMP packet, and
// answers each echo request from the host. It drops every frame whose
// source isn't the VM's own MAC: the switch learns every source MAC, and
// forgets one only when its port goes away. Frames between VMs and ARP pass
// unchanged.
type guard struct {
	net.Conn
	mac        tcpip.LinkAddress
	subnet     netip.Prefix
	gateway    tcpip.Address
	gatewayMAC tcpip.LinkAddress
	echoes     *rate.Limiter
}

func newGuard(port net.Conn, mac net.HardwareAddr, cfg Config) *guard {
	return &guard{
		Conn:       port,
		mac:        tcpip.LinkAddress(mac),
		subnet:     cfg.Subnet,
		gateway:    tcpip.AddrFrom4(cfg.Gateway.As4()),
		gatewayMAC: tcpip.LinkAddress(cfg.GatewayMAC),
		echoes:     rate.NewLimiter(echoRate, echoBurst),
	}
}

// Read reads the VM's next frame that the switch may have.
func (g *guard) Read(b []byte) (int, error) {
	for {
		n, err := g.Conn.Read(b)
		if err != nil || g.pass(b[:n]) {
			return n, err
		}
	}
}

// pass reports whether the switch may have frame, answering a TCP SYN to a
// refused gateway port with a reset, or an echo request to the gateway with
// a reply, first.
func (g *guard) pass(frame []byte) bool {
	if len(frame) < header.EthernetMinimumSize {
		// The switch reads the addresses of every frame.
		return false
	}
	eth := header.Ethernet(frame)
	if eth.SourceAddress() != g.mac {
		return false
	}
	if dst := eth.DestinationAddress(); dst != g.gatewayMAC && dst != header.EthernetBroadcastAddress {
		return true
	}
	if eth.Type() != header.IPv4ProtocolNumber {
		return true
	}
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	if !ip.IsValid(len(ip)) {
		return false
	}
	dst := netip.AddrFrom4(ip.DestinationAddress().As4())
	fragment := ip.More() || ip.FragmentOffset() != 0
	switch {
	case g.subnet.Contains(dst):
		return ip.DestinationAddress() == g.gateway && !fragment && g.toGateway(eth, ip)
	case dst == limitedBroadcast:
		return !fragment && ip.TransportProtocol() == header.UDPProtocolNumber && udpPort(ip) == dhcpPort
	case slices.ContainsFunc(notUnicast, func(p netip.Prefix) bool { return p.Contains(dst) }):
		return false
	case ip.TransportProtocol() == header.ICMPv4ProtocolNumber:
		return !fragment && isEchoRequest(ip) && g.echoes.Allow()
	default:
		return true
	}
}

// toGateway reports whether the switch may have ip, an unfragmented packet
// to the gateway's address in eth, answering it first where the guard
// does.
func (g *guard) toGateway(eth header.Ethernet, ip header.IPv4) bool {
	switch ip.TransportProtocol() {
	case header.ICMPv4ProtocolNumber:
		if isEchoRequest(ip) && g.echoes.Allow() {
			// A write that fails means the VM's end is gone; the
			// switch learns that from its own next write.
			_, _ = g.Write(g.echoReply(eth, ip))
		}
		return false
	case header.UDPProtocolNumber:
		port := udpPort(ip)
		return port == dnsPort || port == dhcpPort
	case header.TCPProtocolNumber:
		tcp := header.TCP(ip.Payload())
		if len(tcp) < header.TCPMinimumSize || int(tcp.DataOffset()) < header.TCPMinimumSize || int(tcp.DataOffset()) > len(tcp) {
			return false
		}
		if tcp.DestinationPort() == dnsPort {
			return true
		}
		if tcp.Flags()&header.TCPFlagRst == 0 {
			_, _ = g.Write(g.reset(eth, ip, tcp))
		}
		return false
	default:
		return false
	}
}

// udpPort returns the destination port of ip's UDP datagram, or 0 if it
// is too short for a UDP header.
func udpPort(ip header.IPv4) uint16 {
	udp := header.UDP(ip.Payload())
	if len(udp) < header.UDPMinimumSize {
		return 0
	}
	return udp.DestinationPort()
}

// isEchoRequest reports whether ip, an ICMP packet, is an echo request.
func isEchoRequest(ip header.IPv4) bool {
	icmp := header.ICMPv4(ip.Payload())
	return len(icmp) >= header.ICMPv4MinimumSize && icmp.Type() == header.ICMPv4Echo
}

// echoReply returns the frame that answers the echo request in ip and eth
// with an echo reply from the gateway, carrying the request's identifier,
// sequence number and data, as RFC 792 says. The request's IP options
// aren't echoed.
func (g *guard) echoReply(eth header.Ethernet, ip header.IPv4) []byte {
	req := header.ICMPv4(ip.Payload())
	frame := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize+len(req))
	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: g.gatewayMAC,
		DstAddr: eth.SourceAddress(),
		Type:    header.IPv4ProtocolNumber,
	})
	rip := header.IPv4(frame[header.EthernetMinimumSize:])
	rip.Encode(&header.IPv4Fields{
		TotalLength: uint16(len(rip)), //nolint:gosec // G115: a frame is under 128 KiB
		TTL:         64,
		Protocol:    uint8(header.ICMPv4ProtocolNumber),
		SrcAddr:     g.gateway,
		DstAddr:     ip.SourceAddress(),
	})
	rip.SetChecksum(^rip.CalculateChecksum())
	reply := header.ICMPv4(rip.Payload())
	copy(reply, req)
	reply.SetType(header.ICMPv4EchoReply)
	reply.SetCode(0)
	reply.SetChecksum(0)
	reply.SetChecksum(^checksum.Checksum(reply, 0))
	return frame
}

// reset returns the frame that answers the TCP segment tcp, in ip and eth,
// with a reset from the gateway, as RFC 9293 says a closed port does.
func (g *guard) reset(eth header.Ethernet, ip header.IPv4, tcp header.TCP) []byte {
	const size = header.EthernetMinimumSize + header.IPv4MinimumSize + header.TCPMinimumSize
	frame := make([]byte, size)
	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: g.gatewayMAC,
		DstAddr: eth.SourceAddress(),
		Type:    header.IPv4ProtocolNumber,
	})
	src, dst := ip.DestinationAddress(), ip.SourceAddress()
	rip := header.IPv4(frame[header.EthernetMinimumSize:])
	rip.Encode(&header.IPv4Fields{
		TotalLength: header.IPv4MinimumSize + header.TCPMinimumSize,
		TTL:         64,
		Protocol:    uint8(header.TCPProtocolNumber),
		SrcAddr:     src,
		DstAddr:     dst,
	})
	rip.SetChecksum(^rip.CalculateChecksum())
	fields := header.TCPFields{
		SrcPort:    tcp.DestinationPort(),
		DstPort:    tcp.SourcePort(),
		DataOffset: header.TCPMinimumSize,
	}
	if tcp.Flags()&header.TCPFlagAck != 0 {
		fields.SeqNum, fields.Flags = tcp.AckNumber(), header.TCPFlagRst
	} else {
		// The segment's length counts its SYN and FIN.
		length := uint32(len(tcp.Payload())) //nolint:gosec // G115: a frame's payload is under 128 KiB
		if tcp.Flags()&header.TCPFlagSyn != 0 {
			length++
		}
		if tcp.Flags()&header.TCPFlagFin != 0 {
			length++
		}
		fields.AckNum, fields.Flags = tcp.SequenceNumber()+length, header.TCPFlagRst|header.TCPFlagAck
	}
	rtcp := header.TCP(rip.Payload())
	rtcp.Encode(&fields)
	xsum := header.PseudoHeaderChecksum(header.TCPProtocolNumber, src, dst, header.TCPMinimumSize)
	rtcp.SetChecksum(^rtcp.CalculateChecksum(xsum))
	return frame
}
