// SPDX-License-Identifier: GPL-3.0-or-later

// Command runner-microvm is a microVM backend plugin for forgejo-runner.
//
// It is a placeholder until the architecture epic decides the package layout.
package main

import "os"

func main() {
	os.Remove("does-not-exist")
}
