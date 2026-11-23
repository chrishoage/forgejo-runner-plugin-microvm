// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// fakeNetlink records the network calls, and fails the one named in fail.
type fakeNetlink struct {
	calls []string
	fail  string
}

func (f *fakeNetlink) rec(call string) error {
	f.calls = append(f.calls, call)
	if strings.HasPrefix(call, f.fail) && f.fail != "" {
		return errors.New("operation not permitted")
	}
	return nil
}

func (f *fakeNetlink) linkUp(name string) error { return f.rec("up " + name) }
func (f *fakeNetlink) addAddr(name string, a netip.Prefix) error {
	return f.rec(fmt.Sprintf("addr %s %s", name, a))
}

func (f *fakeNetlink) setMTU(name string, mtu int) error {
	return f.rec(fmt.Sprintf("mtu %s %d", name, mtu))
}

func (f *fakeNetlink) noIPv6Addrs(name string) error { return f.rec("noipv6 " + name) }

func (f *fakeNetlink) addDefaultRoute(name string, gw netip.Addr) error {
	return f.rec(fmt.Sprintf("route %s via %s", name, gw))
}

func testNetwork() *agentv1.Network {
	return &agentv1.Network{Address: "198.18.0.2/24", Gateway: "198.18.0.1", Mtu: 1500}
}

// The agent brings lo up, then addresses eth0, sets its MTU, stops the
// kernel giving it IPv6 addresses before it is up, and brings it up, then
// routes everything else via the gateway.
func TestConfigureNetwork(t *testing.T) {
	t.Parallel()
	f := &fakeNetlink{}
	if err := configureNetwork(f, testNetwork()); err != nil {
		t.Fatal(err)
	}
	want := []string{"up lo", "addr eth0 198.18.0.2/24", "mtu eth0 1500", "noipv6 eth0", "up eth0", "route eth0 via 198.18.0.1"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
}

// A failed step stops there, and the error names it.
func TestConfigureNetworkNamesTheFailedStep(t *testing.T) {
	t.Parallel()
	for fail, want := range map[string]string{
		"up lo":  "network: lo up: operation not permitted",
		"addr":   "network: eth0 address 198.18.0.2/24: operation not permitted",
		"mtu":    "network: eth0 MTU 1500: operation not permitted",
		"noipv6": "network: eth0 no IPv6 addresses: operation not permitted",
		"up eth": "network: eth0 up: operation not permitted",
		"route":  "network: default route via 198.18.0.1: operation not permitted",
	} {
		f := &fakeNetlink{fail: fail}
		err := configureNetwork(f, testNetwork())
		if err == nil || err.Error() != want {
			t.Errorf("failing %q: err = %v, want %q", fail, err, want)
		}
		if !strings.HasPrefix(f.calls[len(f.calls)-1], fail) {
			t.Errorf("failing %q: went on to %v", fail, f.calls)
		}
	}
}

// A boot config without a usable network fails before any call.
func TestConfigureNetworkRefusesABadConfig(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*agentv1.Network){
		"no address":          func(n *agentv1.Network) { n.Address = "" },
		"no prefix":           func(n *agentv1.Network) { n.Address = "198.18.0.2" },
		"IPv6":                func(n *agentv1.Network) { n.Address = "fd00::2/64" },
		"a gateway elsewhere": func(n *agentv1.Network) { n.Gateway = "10.0.0.1" },
		"the guest's address": func(n *agentv1.Network) { n.Gateway = "198.18.0.2" },
		"no MTU":              func(n *agentv1.Network) { n.Mtu = 0 },
	} {
		cfg := testNetwork()
		change(cfg)
		f := &fakeNetlink{}
		if err := configureNetwork(f, cfg); err == nil || len(f.calls) != 0 {
			t.Errorf("%s: err %v, calls %v", name, err, f.calls)
		}
	}
	if err := configureNetwork(&fakeNetlink{}, nil); err == nil {
		t.Error("no network: no error")
	}
}
