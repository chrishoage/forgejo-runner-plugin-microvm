// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/config"

// Dirs are the plugin's directories and its listen socket
// (docs/architecture.md, On-disk layout).
type Dirs struct {
	// State holds the job binary and each job's files: <state>/bin/,
	// <state>/jobs/<id>/.
	State string
	// Cache is the image cache's directory.
	Cache string
	// Runtime holds each VM's agent socket: <runtime>/<id>/<n>/.
	Runtime string
	// Listen is the listen socket's path.
	Listen string
}

// dirsOf returns cfg's directories and listen socket, which config.Load has
// made absolute and checked against each other.
func dirsOf(cfg *config.Config) Dirs {
	return Dirs{State: cfg.StateDir, Cache: cfg.CacheDir, Runtime: cfg.RuntimeDir, Listen: cfg.Listen}
}
