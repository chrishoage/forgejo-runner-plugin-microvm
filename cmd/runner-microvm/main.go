// SPDX-License-Identifier: GPL-3.0-or-later

// Command runner-microvm is the plugin: a microVM backend plugin for
// forgejo-runner. The work is in internal/plugin.
package main

import (
	// SHORTCUT(#4): main is empty until the plugin serves the runner; the
	// blank import puts the plugin's tree in this binary for the dependency
	// test.
	_ "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/plugin"
)

func main() {}
