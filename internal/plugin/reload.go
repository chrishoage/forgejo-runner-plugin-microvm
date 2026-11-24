// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/config"
)

// readConfig reads the config file again for a reload of the running
// config: (*config.Config).Reload, or a test's fake.
type readConfig func(running *config.Config) (*config.Config, []config.Change, error)

// reloadOnSignal reloads the config file with read on each signal from hup,
// SIGHUP in serve, until ctx ends, starting from cfg, the config the plugin
// started with. Reloads run one at a time, each from the config the last one
// left. A reload still reading when ctx ends finishes on its own; srv is
// closed by then, so it changes nothing.
func reloadOnSignal(ctx context.Context, log *slog.Logger, hup <-chan os.Signal, srv *Server, cfg *config.Config, read readConfig) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			cfg = reloadConfig(log, srv, cfg, read)
		}
	}
}

// reloadConfig reads the config file again with read and returns the config
// now in effect. When the file is valid, srv gets its
// profiles in one step; each plugin-level key whose value changed is logged
// as needing a restart, and keeps its running value; then each profile is
// logged, and last their names. When it is not, cur stays in effect and the error is
// logged, as at startup (CFG-8). Jobs already created keep their copies of
// their profiles (CFG-7). A reload that ends after srv is closed changes
// nothing, and says so.
func reloadConfig(log *slog.Logger, srv *Server, cur *config.Config, read readConfig) *config.Config {
	next, changes, err := read(cur)
	if err != nil {
		log.Warn("reload failed: kept the running config", "err", err)
		return cur
	}
	for _, ch := range changes {
		log.Warn("a changed setting takes effect only at a restart: kept the running value",
			"key", ch.Key, "running", ch.Running, "file", ch.File)
	}
	if !srv.SetProfiles(next.Profiles) {
		log.Info("reload ended after the plugin stopped: changed nothing", "path", next.File)
		return cur
	}
	path := next.File
	if path == "" {
		path = config.DefaultPath + " (none: built-in values)"
	}
	// The summary comes last, so a reader who sees it has every line of
	// the reload.
	logProfiles(log, next.Profiles)
	log.Info("reloaded the config file", "path", path, "profiles", strings.Join(profileNames(next.Profiles), " "))
	return next
}

// profileNames returns the names of ps, sorted, and default among them
// whether ps defines it or not.
func profileNames(ps config.Profiles) []string {
	names := slices.Collect(maps.Keys(ps))
	if _, ok := ps[config.DefaultProfile]; !ok {
		names = append(names, config.DefaultProfile)
	}
	slices.Sort(names)
	return names
}

// logProfiles logs each profile of ps, default always among them.
func logProfiles(log *slog.Logger, ps config.Profiles) {
	for _, name := range profileNames(ps) {
		p, _ := ps.Lookup(name)
		_, defined := ps[name]
		log.Info("profile", "name", name, "defined", defined, "cpus", p.CPUs, "memory_mib", p.MemoryMiB, "scratch_gib", p.ScratchGiB,
			"scratch_paths", p.ScratchPaths, "mounts", mountsAttr(p.Mounts), "disks", disksAttr(p.Disks), "force_pull", p.ForcePull)
	}
}
