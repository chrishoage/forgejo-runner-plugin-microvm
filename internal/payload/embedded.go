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

// JobBinary returns the embedded job binary.
func JobBinary() ([]byte, error) { return jobBinary, nil }

// Agent returns the embedded agent.
func Agent() ([]byte, error) { return agent, nil }
