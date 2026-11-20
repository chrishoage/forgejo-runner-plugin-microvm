// SPDX-License-Identifier: GPL-3.0-or-later

// Command runner-microvm-job is the job binary: the job process, and with
// `vm` the VM child. The plugin embeds it and runs it by path. The work is in
// internal/job.
package main

import (
	"os"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/job"
)

func main() { os.Exit(job.Main(os.Args[1:])) }
