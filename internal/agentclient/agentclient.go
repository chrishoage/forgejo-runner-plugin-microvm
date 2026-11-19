// SPDX-License-Identifier: GPL-3.0-or-later

// Package agentclient is the plugin's side of the agent protocol: it dials a
// VM's agent over the unix socket libkrun maps to the guest's vsock port,
// decides readiness, and runs Exec, CopyIn, CopyOut and Flush.
//
// It is its own package so it can't drift into internal/agent and pull host
// code toward the guest.
package agentclient
