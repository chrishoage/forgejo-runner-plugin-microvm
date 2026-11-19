// SPDX-License-Identifier: GPL-3.0-or-later

//go:build payload

package payload

import _ "embed"

var (
	//go:embed runner-microvm-job
	jobBinary []byte

	//go:embed runner-microvm-agent
	agent []byte
)

// SHORTCUT(#4): nothing calls JobBinary or Agent until the plugin writes them
// out at startup, and the linker drops unreferenced data. This variable keeps
// both in the binary meanwhile.
var linked = [][]byte{jobBinary, agent} //nolint:unused // read by nothing until #4; it only keeps the payload linked

// JobBinary returns the embedded job binary.
func JobBinary() ([]byte, error) { return jobBinary, nil }

// Agent returns the embedded agent.
func Agent() ([]byte, error) { return agent, nil }
