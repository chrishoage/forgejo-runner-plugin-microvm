// SPDX-License-Identifier: GPL-3.0-or-later

// Package jobnet is the job network: gvisor-tap-vsock's switch, gateway and
// DNS server, run inside the job process, serving one job's VM over a frame
// socket. Its DHCP and HTTP API stay off (NET-6). Port forwarding is off
// today: the only ports the plugin protocol passes are a service's ports, and
// a job that declares services fails at Create.
package jobnet

import (
	// SHORTCUT(#4): a blank import keeps gvisor-tap-vsock in the job binary
	// for the dependency test until the job network uses it.
	_ "github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)
