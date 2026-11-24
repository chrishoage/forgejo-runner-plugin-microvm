// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// needsRecovery reports whether the ext4 superblock of the host file p has
// its needs_recovery bit, s_feature_incompat's 0x4 at byte 1120, set.
func needsRecovery(t *testing.T, p string) bool {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, 1120); err != nil {
		t.Fatal(err)
	}
	return b[0]&0x4 != 0
}

// rwStorageProfile is the profile rw: a read-write disk at /data and a
// read-write mount at /share, in a VM as small as boot allows.
func rwStorageProfile(disk, dir string) string {
	return fmt.Sprintf(`profiles:
  rw:
    cpus: 1
    memory_mib: 512
    disks: [{source: %s, target: /data, read_write: true}]
    mounts: [{source: %s, target: /share, read_write: true}]
`, disk, dir)
}

// STO-9: a job writes a file to a read-write disk and one to a read-write
// mount, with no sync of its own, and is removed; Remove's flush leaves the
// disk's ext4 with no needs_recovery flag on the host, and the next job on
// the same profile reads both files.
func TestFlushKeepsTheWrites(t *testing.T) {
	t.Parallel()
	img, host := baseImage(t)
	disk := ext4Disk(t, "store")
	dir := hostDirs(t, "share")[0]
	p := startPluginWith(t, buildPlugin(t), rwStorageProfile(disk, dir))
	ref := pushImage(t, host, "flush", img) + ";profile=rw"

	first := bootImage(t, p, "flush-1", ref)
	if code, out := as(t, first, "0", `echo disk-kept > /data/kept && echo mount-kept > /share/kept`); code != 0 {
		t.Fatalf("the first job's step exited %d:\n%s", code, out)
	}
	// Mounted read-write, the disk's journal is in use, so the check below
	// can fail.
	if !needsRecovery(t, disk) {
		t.Fatal("the disk's ext4 needs no recovery while the first job has it mounted read-write")
	}
	first.remove(t)
	if needsRecovery(t, disk) {
		t.Error("the disk's ext4 needs recovery after the first job's Remove: the flush did not leave it clean")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "kept")); err != nil || string(b) != "mount-kept\n" {
		t.Errorf("the mount's host directory holds %q, %v after the first job", b, err)
	}

	second := bootImage(t, p, "flush-2", ref)
	if code, out := as(t, second, "0", `test "$(cat /data/kept)" = disk-kept && test "$(cat /share/kept)" = mount-kept`); code != 0 {
		t.Errorf("the second job does not read the first job's files: exit %d:\n%s", code, out)
	}
}
