// SPDX-License-Identifier: GPL-3.0-or-later

// Command runner-microvm is the plugin: a microVM backend plugin for
// forgejo-runner. The work is in internal/plugin.
package main

import (
	"os"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/plugin"
)

func main() { os.Exit(plugin.Main(os.Args[1:])) }
