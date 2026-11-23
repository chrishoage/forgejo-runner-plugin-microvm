// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
)

// The job network's addressing, the same for every job: each job network is
// its own switch, so they never meet. RFC 2544's benchmarking range never
// appears on a real LAN, unlike gvisor-tap-vsock's default 192.168.127.0/24
// or CGNAT's 100.64.0.0/10 (Tailscale). There is no config key; one can be
// added if a collision is ever reported.
var (
	jobSubnet  = netip.MustParsePrefix("198.18.0.0/24")
	jobGateway = netip.MustParseAddr("198.18.0.1")
)

// jobMTU is the job network's MTU, proven by the spike. A larger one is a
// follow-up once a problem is measured.
const jobMTU = 1500

// maxVMs is the most VMs a job network addresses: .2 to .254.
const maxVMs = 253

// vmAddress returns the address and MAC on the job network of the VM at
// index in the job spec: .(2+index), so the job's container is 198.18.0.2.
// They come from the index alone, with no allocator, so a later VM needs no
// change to the job network.
func vmAddress(index int) (netip.Addr, net.HardwareAddr, error) {
	if index < 0 || index >= maxVMs {
		return netip.Addr{}, nil, fmt.Errorf("VM %d: a job network addresses at most %d VMs", index, maxVMs)
	}
	a := jobSubnet.Addr().As4()
	a[3] += byte(2 + index)
	addr := netip.AddrFrom4(a)
	return addr, macOf(addr), nil
}

// macOf returns addr's MAC on the job network: locally administered, 02:00
// and the address's four bytes, so the job's container's is
// 02:00:c6:12:00:02.
func macOf(addr netip.Addr) net.HardwareAddr {
	a := addr.As4()
	return net.HardwareAddr{0x02, 0x00, a[0], a[1], a[2], a[3]}
}

// jobNetwork is the job spec's job network.
func jobNetwork() *jobv1.Network {
	return &jobv1.Network{
		Subnet:         jobSubnet.String(),
		GatewayAddress: jobGateway.String(),
		GatewayMac:     macOf(jobGateway).String(),
		Mtu:            jobMTU,
	}
}

// bootNetwork is the boot config's network for a VM at addr: an ordinary
// subnet, with the gateway as its default route and name server.
func bootNetwork(addr netip.Addr) *agentv1.Network {
	return &agentv1.Network{
		Address: addr.String() + "/" + strconv.Itoa(jobSubnet.Bits()),
		Gateway: jobGateway.String(),
		Mtu:     jobMTU,
	}
}
