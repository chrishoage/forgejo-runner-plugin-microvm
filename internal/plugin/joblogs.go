// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// The job logs' names in a job's state directory, <state>/jobs/<id>/: the
// job process log at the top, and each VM's log and console in its <n>/.
// keptLogsName is the directory there that the logs to keep are gathered
// in before it becomes <state>/logs/<id>/; not being a VM's index, it is
// never a VM's directory.
const (
	jobLogName   = "job.log"
	vmLogName    = "vm.log"
	consoleName  = "console"
	keptLogsName = "kept-logs"
)

// keepJobLogs moves the job logs of the job id, which has ended, from
// <state>/jobs/<id>/ to <state>/logs/<id>/, keeping their layout, and returns
// that directory. A job with no job logs, one never started, keeps nothing,
// and the directory is "". rename, renameNoReplace outside tests, makes
// every rename.
//
// The logs are gathered in <state>/jobs/<id>/kept-logs/ first, and that
// directory is then renamed to <state>/logs/<id> in one step, so after a
// crash at any point <state>/logs/ holds all of the job's logs or none of
// them, and a partial gathering is under <state>/jobs/, which startup
// cleanup deletes. For the same reason an error needs no undoing: nothing
// is kept, and what was gathered is deleted with the job's state directory
// by Remove. A <state>/logs/<id> already there is never the job's, so the
// last rename fails then and leaves it be.
func keepJobLogs(state, id string, rename func(oldpath, newpath string) error) (string, error) {
	src := filepath.Join(state, "jobs", id)
	logs, err := jobLogs(src)
	if err != nil || len(logs) == 0 {
		return "", err
	}
	gather := filepath.Join(src, keptLogsName)
	if err := os.Mkdir(gather, 0o700); err != nil {
		return "", err
	}
	for _, rel := range logs {
		if dir := filepath.Dir(rel); dir != "." {
			if err := os.Mkdir(filepath.Join(gather, dir), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
		}
		if err := rename(filepath.Join(src, rel), filepath.Join(gather, rel)); err != nil {
			return "", err
		}
	}
	kept := filepath.Join(state, "logs")
	if err := os.MkdirAll(kept, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(kept, id)
	if err := rename(gather, dst); err != nil {
		if errors.Is(err, unix.EXDEV) {
			return "", fmt.Errorf("%s is on another filesystem than the state directory %s, and the logs are only renamed: %w", kept, state, err)
		}
		return "", err
	}
	return dst, nil
}

// renameNoReplace renames oldpath to newpath unless newpath exists, an empty
// directory included, which a plain rename of a directory would replace. Its
// error names both paths, as os.Rename's does.
func renameNoReplace(oldpath, newpath string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE); err != nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}

// jobLogs returns the job logs in the job's state directory src, relative
// to it: the job process log, then each VM's log and console. Only a regular
// file is a job log; a missing one, such as a VM's of a Start that failed
// before its VM began, is left out.
func jobLogs(src string) ([]string, error) {
	es, err := os.ReadDir(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	candidates := []string{jobLogName}
	for _, e := range es {
		// A VM's directory is named by its index, which only the plugin
		// chooses.
		if _, err := strconv.ParseUint(e.Name(), 10, 32); err == nil && e.IsDir() {
			candidates = append(candidates, filepath.Join(e.Name(), vmLogName), filepath.Join(e.Name(), consoleName))
		}
	}
	var logs []string
	for _, rel := range candidates {
		fi, err := os.Lstat(filepath.Join(src, rel))
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return nil, err
		case fi.Mode().IsRegular():
			logs = append(logs, rel)
		}
	}
	return logs, nil
}
