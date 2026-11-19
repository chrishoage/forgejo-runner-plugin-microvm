// SPDX-License-Identifier: GPL-3.0-or-later

// Package joblaunch is the plugin's side of a job process: it starts the job
// binary with the liveness pipe and Pdeathsig, writes the job spec to its
// control channel, reads its status events and kills it. The plugin reaches
// it through its JobLauncher seam.
package joblaunch

import (
	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
)

// Process is the plugin's handle on one running job process.
//
// The caller may stop reading Events at any time, for example after the job's
// container's VMStarted, once it dials that VM's agent to learn when it
// answers Ping; today the job's container is the only VM, because the plugin
// protocol passes no VM options for services. The launcher
// reads the job process's stdout to its end whether or not anyone reads
// Events, so Exited and Kill never wait on the caller.
type Process struct {
	// Events delivers the job process's status events in order. It is
	// buffered for every event the control channel allows for the spec (at
	// most one NetworkUp and one Failed, and one VMStarted and one VMExited
	// for each of its VMs), so the launcher never blocks sending one, and an
	// unread VMExited stays in the buffer until the caller reads it. An event
	// that doesn't fit, which only a job process breaking the protocol can
	// send, is logged and dropped. Events is closed when the job process's
	// stdout ends.
	Events <-chan *jobv1.Event
	// Exited is closed once the job process and every other process in its
	// group, its VM children included, have exited and been reaped, whether
	// or not Events has been read. The launcher kills the group when the job
	// process exits, so a VM child it leaves behind never outlives Exited.
	Exited <-chan struct{}
}
