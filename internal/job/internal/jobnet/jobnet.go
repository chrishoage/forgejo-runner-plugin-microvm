// SPDX-License-Identifier: GPL-3.0-or-later

// Package jobnet is the job network: gvisor-tap-vsock's switch, gateway, DNS
// server and forwarders, run inside the job process, serving each of the
// job's VMs over a frame socket of its own (NET-1, NET-2).
//
// It is gvisor-tap-vsock's virtualnetwork.New, configured only: no NAT
// table, no gateway virtual IPs, no port forwards, no DNS zones and
// link-local access off. So no address on the job network maps to the
// host's loopback (NET-4), no host port is forwarded into a VM (NET-3), and
// connections leave the host as the job process's own (NET-5). Its DHCP
// server always runs, and no VM uses it; each VM's address is reserved for
// its MAC, so it can hand one VM's address to no other (NET-6).
package jobnet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
	"golang.org/x/sys/unix"
)

// The frame socket's buffers, on both ends: proven by the spike, and only to
// be tuned once a problem is measured.
const (
	sendBuffer    = 1 << 20
	receiveBuffer = 4 << 20
)

// Config is a job network's addressing.
type Config struct {
	// Subnet is the job network's subnet.
	Subnet netip.Prefix
	// Gateway is the gateway's address in Subnet, every VM's default route
	// and name server, and GatewayMAC its MAC.
	Gateway    netip.Addr
	GatewayMAC net.HardwareAddr
	// MTU is the link's MTU.
	MTU int
	// VMs are the job's VMs' addresses and MACs. The DHCP server no VM uses
	// reserves each address for its MAC.
	VMs []VM
}

// VM is one VM's address and MAC on the job network.
type VM struct {
	Address netip.Addr
	MAC     net.HardwareAddr
}

// Network is a running job network.
type Network struct {
	vn     *virtualnetwork.VirtualNetwork
	log    *slog.Logger
	libLog *libraryLog

	// ctx ends every switch port's service when Close cancels it.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	ports  []net.Conn
	closed bool
}

// New checks cfg, routes the library's logger into log, and starts the job
// network's gateway, DNS server and forwarders. It serves no VM until
// Attach.
func New(cfg Config, log *slog.Logger) (*Network, error) {
	if err := cfg.check(); err != nil {
		return nil, err
	}
	libLog := routeLibraryLog(log)
	leases := make(map[string]string, len(cfg.VMs))
	for _, vm := range cfg.VMs {
		leases[vm.Address.String()] = vm.MAC.String()
	}
	vn, err := virtualnetwork.New(&types.Configuration{
		MTU:               cfg.MTU,
		Subnet:            cfg.Subnet.String(),
		GatewayIP:         cfg.Gateway.String(),
		GatewayMacAddress: cfg.GatewayMAC.String(),
		DHCPStaticLeases:  leases,
		// Everything else is left out: NAT, GatewayVirtualIPs, Forwards
		// and DNS are gvproxy's command-line defaults, not the
		// library's, and Ec2MetadataAccess is off.
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Network{vn: vn, log: log, libLog: libLog, ctx: ctx, cancel: cancel}, nil
}

// check refuses a config the library would take but serve wrongly.
func (c Config) check() error {
	if !c.Subnet.IsValid() || !c.Subnet.Addr().Is4() || c.Subnet.Masked() != c.Subnet {
		return fmt.Errorf("subnet %s: not an IPv4 network", c.Subnet)
	}
	if !c.Subnet.Contains(c.Gateway) {
		return fmt.Errorf("gateway %s: not in the subnet %s", c.Gateway, c.Subnet)
	}
	if len(c.GatewayMAC) != 6 {
		return fmt.Errorf("gateway MAC %q: not an Ethernet address", c.GatewayMAC)
	}
	if c.MTU < 576 || c.MTU > 65535 {
		return fmt.Errorf("MTU %d: not 576 to 65535", c.MTU)
	}
	addrs := map[netip.Addr]bool{c.Gateway: true}
	macs := map[string]bool{c.GatewayMAC.String(): true}
	for _, vm := range c.VMs {
		if !c.Subnet.Contains(vm.Address) {
			return fmt.Errorf("VM address %s: not in the subnet %s", vm.Address, c.Subnet)
		}
		if len(vm.MAC) != 6 {
			return fmt.Errorf("VM MAC %q: not an Ethernet address", vm.MAC)
		}
		if addrs[vm.Address] || macs[vm.MAC.String()] {
			return fmt.Errorf("VM %s (%s): its address or MAC is taken", vm.Address, vm.MAC)
		}
		addrs[vm.Address], macs[vm.MAC.String()] = true, true
	}
	return nil
}

// Attach makes a frame socket, one Ethernet frame per datagram, serves its
// switch end, and returns the VM's end, for the VM child's fd 4. Each VM
// attaches through a call of its own.
func (n *Network) Attach() (*os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("frame socket: %w", err)
	}
	vmEnd, swEnd := os.NewFile(uintptr(fds[0]), "vm-frames"), os.NewFile(uintptr(fds[1]), "switch-frames")
	port, err := attach(vmEnd, swEnd)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("frame socket: %w", err), vmEnd.Close(), swEnd.Close())
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, errors.Join(net.ErrClosed, vmEnd.Close(), port.Close())
	}
	n.ports = append(n.ports, port)
	n.wg.Go(func() {
		// It returns once Close closes the port, or the VM's end is gone
		// and a write to it fails.
		if err := n.vn.AcceptVfkit(n.ctx, port); err != nil && n.ctx.Err() == nil {
			n.log.Warn("job network: a VM's frame socket stopped", "err", err)
		}
	})
	return vmEnd, nil
}

// attach sizes both ends' buffers and returns the switch's end as a
// net.Conn; swEnd is then closed, its descriptor duplicated into the conn.
func attach(vmEnd, swEnd *os.File) (net.Conn, error) {
	for _, f := range []*os.File{vmEnd, swEnd} {
		if err := setBuffers(f); err != nil {
			return nil, err
		}
	}
	port, err := net.FileConn(swEnd)
	if err != nil {
		return nil, err
	}
	if err := swEnd.Close(); err != nil {
		return nil, errors.Join(err, port.Close())
	}
	return port, nil
}

// setBuffers sets f's send and receive buffers. The kernel caps each at its
// wmem_max and rmem_max.
func setBuffers(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	err = rc.Control(func(fd uintptr) {
		serr = errors.Join(
			unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF, sendBuffer),
			unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, receiveBuffer),
		)
	})
	return errors.Join(err, serr)
}

// Close stops serving every VM's frame socket and waits until each port's
// service has returned. The gateway's stack itself is never freed, which is
// harmless: the job process ends with the job.
func (n *Network) Close() error {
	n.mu.Lock()
	n.closed = true
	ports := n.ports
	n.ports = nil
	n.mu.Unlock()
	n.libLog.closing.Store(true)
	n.cancel()
	var errs []error
	for _, p := range ports {
		// The switch closes a port whose writes fail.
		if err := p.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	n.wg.Wait()
	return errors.Join(append(errs, n.vn.Close())...)
}
