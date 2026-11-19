// SPDX-License-Identifier: GPL-3.0-or-later

// Package job is the job process: it reads its spec from the control
// channel, runs the job network, locks the read-write disks, starts a VM child
// for each of the spec's VMs and reports status events. With `vm` it is a VM
// child instead.
//
// The job network and the VM child are under internal/, so the compiler stops
// any package outside this one from importing them.
package job

import (
	// SHORTCUT(#4): blank imports keep the job network and the VM child in
	// the job binary for the dependency test until the job process uses them.
	_ "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/job/internal/jobnet"
	_ "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/job/internal/vm"
)
