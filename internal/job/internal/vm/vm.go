// SPDX-License-Identifier: GPL-3.0-or-later

// Package vm is the VM child: it loads libkrun with purego, configures the VM
// from its spec and inherited descriptors, and starts it. It is the only code
// that loads libkrun (docs/adr/0004-libkrun-via-purego.md).
package vm

import (
	// SHORTCUT(#4): a blank import keeps purego in the job binary for the
	// dependency test until the VM child binds libkrun.
	_ "github.com/ebitengine/purego"
)
