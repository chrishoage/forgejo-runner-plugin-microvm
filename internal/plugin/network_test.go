// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"testing"
)

// Each VM's address and MAC come from its index in the job spec, and the
// gateway is .1.
func TestVMAddress(t *testing.T) {
	t.Parallel()
	for index, want := range map[int][2]string{
		0:   {"198.18.0.2", "02:00:c6:12:00:02"},
		1:   {"198.18.0.3", "02:00:c6:12:00:03"},
		252: {"198.18.0.254", "02:00:c6:12:00:fe"},
	} {
		addr, mac, err := vmAddress(index)
		if err != nil || addr.String() != want[0] || mac.String() != want[1] {
			t.Errorf("vmAddress(%d) = %s, %s, %v; want %s, %s", index, addr, mac, err, want[0], want[1])
		}
	}
	for _, index := range []int{-1, 253} {
		if _, _, err := vmAddress(index); err == nil {
			t.Errorf("vmAddress(%d): no error", index)
		}
	}
	n := jobNetwork()
	if n.GetGatewayAddress() != "198.18.0.1" || n.GetGatewayMac() != "02:00:c6:12:00:01" || n.GetSubnet() != "198.18.0.0/24" || n.GetMtu() != 1500 {
		t.Errorf("job network = %v", n)
	}
}
