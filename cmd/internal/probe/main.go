// SPDX-License-Identifier: GPL-3.0-or-later

// Command probe is the network probe the KVM tests copy into a booted VM
// with CopyIn and run with Exec. It is built by the test, static
// (CGO_ENABLED=0), so it runs on any image, and never ships.
//
//	probe connect <host:port>         open a TCP connection, print "connected"
//	probe resolve <name> <server:53>  ask server for name's A records
//	probe netinfo                     print lo's and eth0's state, addresses and the default route
//
// Each exits 0 on success, and 1 with the error on stderr otherwise.
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// timeout bounds each check.
const timeout = 10 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	switch {
	case len(args) == 2 && args[0] == "connect":
		return connect(args[1])
	case len(args) == 3 && args[0] == "resolve":
		return resolve(args[1], args[2])
	case len(args) == 1 && args[0] == "netinfo":
		return netinfo()
	default:
		return errors.New("usage: probe connect <host:port> | resolve <name> <server:53> | netinfo")
	}
}

func connect(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, err := new(net.Dialer).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	fmt.Printf("connected %s -> %s\n", c.LocalAddr(), c.RemoteAddr())
	return c.Close()
}

// resolve asks server for name's A records over UDP, directly, so the
// answer can only have come from server, never from the image's
// /etc/hosts.
func resolve(name, server string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, err := new(net.Dialer).DialContext(ctx, "udp", server)
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
	if _, err := c.Write(b); err != nil {
		return err
	}
	buf := make([]byte, 1500)
	size, err := c.Read(buf)
	if err != nil {
		return err
	}
	var m dnsmessage.Message
	if err := m.Unpack(buf[:size]); err != nil {
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
	fmt.Printf("resolved %s via %s: %s\n", name, server, strings.Join(found, " "))
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
