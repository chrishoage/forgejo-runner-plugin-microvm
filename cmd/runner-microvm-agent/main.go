// SPDX-License-Identifier: GPL-3.0-or-later

// Command runner-microvm-agent is the agent: the static program injected into
// every VM that prepares it at boot, runs commands and copies files in and
// out. The job's steps run through the job's container's agent. The plugin
// embeds it. The work is in internal/agent.
package main

import (
	// SHORTCUT(#4): main is empty until the agent serves; the blank import
	// puts the agent's tree in this binary for the dependency test.
	_ "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/agent"
)

func main() {}
