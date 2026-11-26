// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"fmt"
	"syscall"
)

// close_range(2) and its flag, which the syscall package doesn't name and
// the agent may not import golang.org/x/sys for; one number on every
// architecture, as for the mount API's calls (mount.go).
const (
	sysCloseRange     = 436
	closeRangeCloexec = 0x4
)

// closeInheritedOnExec marks every descriptor above 2 close-on-exec: Go
// sets it only on the ones it opens itself, so a descriptor the agent
// inherited would otherwise pass on to the service and every Exec's
// command. libkrun's init hands the agent only 0, 1 and 2; nothing of the
// agent's relies on another. close_range's flag needs Linux 5.11, which the
// agent's own 6.8 (mount.go) passes.
func closeInheritedOnExec() error {
	if _, _, errno := syscall.Syscall(sysCloseRange, 3, ^uintptr(0), closeRangeCloexec); errno != 0 {
		return fmt.Errorf("mark inherited descriptors close-on-exec: %w", errno)
	}
	return nil
}
