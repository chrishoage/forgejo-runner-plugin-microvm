// SPDX-License-Identifier: GPL-3.0-or-later

// Package agent is the agent that runs inside every VM: it makes the guest's
// boot mounts from its boot config, then serves the agent protocol's Ping,
// Exec, CopyIn, CopyOut and Flush.
//
// The agent binary may depend only on the standard library, grpc, protobuf,
// this package, internal/logging, internal/secret and the agent protocol's
// generated package; cmd/deps_test.go checks it.
package agent
