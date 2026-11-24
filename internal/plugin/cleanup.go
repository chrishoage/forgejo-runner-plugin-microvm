// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
)

// cleanStartup is the plugin's side of startup cleanup (docs/architecture.md,
// Failure handling and startup cleanup): it deletes everything in
// <state>/jobs/ and <runtime>/ without inspecting it, and logs the counts
// in one line. The directory locks, which the caller holds, prove no other
// plugin uses these directories, and no job process outlives the plugin
// that started it (the liveness pipe and Pdeathsig), so everything there is
// a crashed plugin's leftover. It keeps <runtime>/lock, since deleting a
// held lock file would let a second plugin lock a new file at the same
// path, and, when the listen socket's directory is <runtime>, the listen
// socket and its temporary name, which listen deals with as it binds. It
// never touches <state>/logs/. An entry it can't delete is an error naming
// its directory, returned after the counts of what it deleted before it are
// logged.
func cleanStartup(log *slog.Logger, dirs Dirs) error {
	keep := []string{lockFile}
	same, err := sameDir(filepath.Dir(dirs.Listen), dirs.Runtime)
	if err != nil {
		return err
	}
	if same {
		name := filepath.Base(dirs.Listen)
		keep = append(keep, name, tempSocketName(name))
	}
	jobs, err := deleteEntries(filepath.Join(dirs.State, "jobs"))
	runtime := 0
	if err == nil {
		runtime, err = deleteEntries(dirs.Runtime, keep...)
	}
	// Logged before an error is returned too, so a cleanup an entry stops
	// still says what it deleted.
	if jobs > 0 || runtime > 0 {
		log.Info("deleted crash leftovers", "state", dirs.State, "jobs", jobs, "runtime", dirs.Runtime, "runtime_entries", runtime)
	}
	return err
}

// sameDir reports whether the directories a and b are one, by their file
// identity, as lockDirs decides it.
func sameDir(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

// deleteEntries deletes every entry of dir but those named in keep, read-only
// subtrees included, keeping dir, and returns how many it deleted. A missing
// dir has none. The first entry it can't delete stops it, with an error
// naming dir.
func deleteEntries(dir string, keep ...string) (int, error) {
	es, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("the directory %s: startup cleanup: %w", dir, err)
	}
	n := 0
	for _, e := range es {
		if slices.Contains(keep, e.Name()) {
			continue
		}
		if err := removeTree(filepath.Join(dir, e.Name())); err != nil {
			return n, fmt.Errorf("the directory %s: startup cleanup: %w", dir, err)
		}
		n++
	}
	return n, nil
}
