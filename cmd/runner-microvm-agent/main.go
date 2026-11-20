// SPDX-License-Identifier: GPL-3.0-or-later

// Command runner-microvm-agent is the agent: the static program injected into
// every VM that prepares it at boot, runs commands and copies files in and
// out. The job's steps run through the job's container's agent. The plugin
// embeds it. The work is in internal/agent.
package main

import (
	"os"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/agent"
)

func main() { os.Exit(agent.Main(os.Args[1:])) }
