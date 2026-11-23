// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// guestLink is the VM's network device: libkrun names its first one eth0.
const guestLink = "eth0"

// netlinker is the kernel's side of the guest's network, so its steps are
// tested without privilege.
type netlinker interface {
	linkUp(name string) error
	addAddr(name string, addr netip.Prefix) error
	setMTU(name string, mtu int) error
	// noIPv6Addrs stops the kernel giving the link IPv6 addresses of its
	// own, its link-local fe80:: address among them, once it is up.
	noIPv6Addrs(name string) error
	// addDefaultRoute routes everything off the link's subnet via gw.
	addDefaultRoute(name string, gw netip.Addr) error
}

// configureNetwork addresses the guest statically from its boot config,
// with no DHCP client: lo up, explicitly rather than trusting libkrun's
// init, then on eth0 the address, the MTU, no IPv6 addresses of the
// kernel's own (IPv4 only: lo's ::1 is the guest's only IPv6 address) and
// up, then the default route via the gateway. IPv4 forwarding stays at the
// kernel's default, off. A failure names its step.
func configureNetwork(n netlinker, cfg *agentv1.Network) error {
	addr, gw, err := checkNetwork(cfg)
	if err != nil {
		return err
	}
	for _, s := range []struct {
		name string
		do   func() error
	}{
		{"lo up", func() error { return n.linkUp("lo") }},
		{guestLink + " address " + addr.String(), func() error { return n.addAddr(guestLink, addr) }},
		{fmt.Sprintf("%s MTU %d", guestLink, cfg.GetMtu()), func() error { return n.setMTU(guestLink, int(cfg.GetMtu())) }},
		{guestLink + " no IPv6 addresses", func() error { return n.noIPv6Addrs(guestLink) }},
		{guestLink + " up", func() error { return n.linkUp(guestLink) }},
		{"default route via " + gw.String(), func() error { return n.addDefaultRoute(guestLink, gw) }},
	} {
		if err := s.do(); err != nil {
			return fmt.Errorf("network: %s: %w", s.name, err)
		}
	}
	return nil
}

// checkNetwork returns the guest's address and gateway, refusing a config
// that isn't an IPv4 address with its subnet, a gateway on that subnet and
// an MTU.
func checkNetwork(cfg *agentv1.Network) (netip.Prefix, netip.Addr, error) {
	if cfg == nil {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("boot config: no network")
	}
	addr, err := netip.ParsePrefix(cfg.GetAddress())
	if err != nil || !addr.Addr().Is4() {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("boot config: network address %q is not IPv4 in CIDR notation", cfg.GetAddress())
	}
	gw, err := netip.ParseAddr(cfg.GetGateway())
	if err != nil || !addr.Masked().Contains(gw) || gw == addr.Addr() {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("boot config: gateway %q is not another address on %s", cfg.GetGateway(), addr.Masked())
	}
	if mtu := cfg.GetMtu(); mtu < 576 || mtu > 65535 {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("boot config: MTU %d is not 576 to 65535", mtu)
	}
	return addr, gw, nil
}

// rtnetlink is the guest kernel's network, through rtnetlink.
type rtnetlink struct{}

func (rtnetlink) link(name string) (netlink.Link, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("link %s: %w", name, err)
	}
	return l, nil
}

func (r rtnetlink) linkUp(name string) error {
	l, err := r.link(name)
	if err != nil {
		return err
	}
	return netlink.LinkSetUp(l)
}

func (r rtnetlink) addAddr(name string, addr netip.Prefix) error {
	l, err := r.link(name)
	if err != nil {
		return err
	}
	ip := addr.Addr().As4()
	return netlink.AddrAdd(l, &netlink.Addr{IPNet: &net.IPNet{IP: ip[:], Mask: net.CIDRMask(addr.Bits(), 32)}})
}

func (r rtnetlink) setMTU(name string, mtu int) error {
	l, err := r.link(name)
	if err != nil {
		return err
	}
	return netlink.LinkSetMTU(l, mtu)
}

// noIPv6Addrs sets the link's IPv6 address generation mode to none, through
// rtnetlink, since /proc/sys need not be mounted. A kernel without IPv6
// refuses it, and gives the link no IPv6 address anyway.
func (r rtnetlink) noIPv6Addrs(name string) error {
	l, err := r.link(name)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetIP6AddrGenMode(l, nl.IN6_ADDR_GEN_MODE_NONE); err != nil && !errors.Is(err, syscall.EAFNOSUPPORT) {
		return err
	}
	return nil
}

func (r rtnetlink) addDefaultRoute(name string, gw netip.Addr) error {
	l, err := r.link(name)
	if err != nil {
		return err
	}
	ip := gw.As4()
	return netlink.RouteAdd(&netlink.Route{
		LinkIndex: l.Attrs().Index,
		Dst:       &net.IPNet{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)},
		Gw:        ip[:],
	})
}
