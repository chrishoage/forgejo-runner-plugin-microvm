// SPDX-License-Identifier: GPL-3.0-or-later

// Command runner-microvm-job is the job binary: the job process, and with
// `vm` the VM child. The plugin embeds it and runs it by path. The work is in
// internal/job.
package main

import (
	// SHORTCUT(#4): main is empty until the job process runs; the blank
	// import puts the job's tree in this binary for the dependency test.
	_ "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/job"
)

func main() {}
