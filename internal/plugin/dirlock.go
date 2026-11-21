// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockFile is the name of the lock file in each of the plugin's directories
// (docs/architecture.md, On-disk layout). Nothing ever deletes it: deleting a
// held lock file would let a second plugin lock a new file at the same path.
const lockFile = "lock"

// dirLocks holds the plugin's directory locks (IMG-16). The locks last until
// Close, so the plugin keeps them for its whole life.
type dirLocks struct {
	files []*os.File
}

// lockDirs creates the state, cache, runtime and listen-socket directories
// where they are missing, and takes an exclusive, non-blocking flock on a lock
// file in each, in that order, before anything else is written to them. The
// first one another process holds stops it with an error naming that
// directory, and the locks it took are released. The listen-socket directory
// takes no lock of its own when it is the runtime directory: two flocks on one
// file through two opens in one process block each other.
//
// Checking that the directories are distinct is #8's, with the config keys
// that make them choosable; under RUNNER_MICROVM_DIR they always are.
func lockDirs(dirs Dirs) (*dirLocks, error) {
	type dir struct {
		path string
		mode os.FileMode
	}
	order := []dir{{dirs.State, 0o700}, {dirs.Cache, 0o755}, {dirs.Runtime, 0o755}}
	if listen := filepath.Dir(dirs.Listen); filepath.Clean(listen) != filepath.Clean(dirs.Runtime) {
		order = append(order, dir{listen, 0o755})
	}
	l := &dirLocks{}
	for _, d := range order {
		f, err := lockDir(d.path, d.mode)
		if err != nil {
			return nil, errors.Join(err, l.Close())
		}
		l.files = append(l.files, f)
	}
	return l, nil
}

// lockDir creates dir if it is missing and locks its lock file. The lock
// file is opened close-on-exec, so no job process inherits it and keeps the
// directory locked after the plugin dies.
func lockDir(dir string, mode os.FileMode) (*os.File, error) {
	if err := os.MkdirAll(dir, mode); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, lockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|unix.O_CLOEXEC, 0o600) //nolint:gosec // G304: the lock file in the plugin's own directory
	if err != nil {
		return nil, fmt.Errorf("lock the directory %s: %w", dir, err)
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("the directory %s is in use by another runner-microvm, which holds %s", dir, path)
		}
		return nil, fmt.Errorf("lock the directory %s: %w", dir, err)
	}
	return f, nil
}

// Close releases every lock. The lock files stay.
func (l *dirLocks) Close() error {
	var err error
	for _, f := range l.files {
		err = errors.Join(err, f.Close())
	}
	l.files = nil
	return err
}
