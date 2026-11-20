// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"fmt"
	"path/filepath"
)

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

// The built-in paths (#1, Configuration and profiles).
var builtinDirs = Dirs{
	State:   "/var/lib/runner-microvm",
	Cache:   "/var/cache/runner-microvm",
	Runtime: "/run/runner-microvm/jobs",
	Listen:  "/run/runner-microvm/runner-microvm.sock",
}

// BaseDirEnv names the one directory that holds all of the plugin's
// directories.
const BaseDirEnv = "RUNNER_MICROVM_DIR"

// DirsFromEnv returns the built-in paths, or with RUNNER_MICROVM_DIR=<base>
// set, <base>/state, <base>/cache and <base>/runtime, with the listen socket
// in the runtime directory. The paths are absolute.
//
// SHORTCUT(#8): one environment variable stands in for the config file's
// listen, state, cache and runtime keys.
func DirsFromEnv(getenv func(string) string) (Dirs, error) {
	base := getenv(BaseDirEnv)
	if base == "" {
		return builtinDirs, nil
	}
	abs, err := filepath.Abs(base)
	if err != nil {
		return Dirs{}, fmt.Errorf("%s=%s: %w", BaseDirEnv, base, err)
	}
	runtime := filepath.Join(abs, "runtime")
	return Dirs{
		State:   filepath.Join(abs, "state"),
		Cache:   filepath.Join(abs, "cache"),
		Runtime: runtime,
		Listen:  filepath.Join(runtime, "runner-microvm.sock"),
	}, nil
}
