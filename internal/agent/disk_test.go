// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	erofs "github.com/erofs/go-erofs"
)

// mkfsExt4 formats a fresh 8 MiB file in dir as ext4 with the host's
// mkfs.ext4, and returns its path.
func mkfsExt4(t *testing.T, dir string) string {
	t.Helper()
	mkfs, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		t.Fatalf("mkfs.ext4, which the dev shell provides: %v", err)
	}
	p := filepath.Join(dir, "disk.ext4")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, mkfs, "-q", "-F", p, "8M").CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v\n%s", err, out)
	}
	return p
}

// setNeedsRecovery sets the needs_recovery bit in the ext4 superblock of the
// file p, as a crash with a dirty journal leaves it.
func setNeedsRecovery(t *testing.T, p string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, superblockOffset+ext4IncompatAt); err != nil {
		t.Fatal(err)
	}
	b[0] |= ext4IncompatRecover
	if _, err := f.WriteAt(b, superblockOffset+ext4IncompatAt); err != nil {
		t.Fatal(err)
	}
}

// probeFile probes the file p.
func probeFile(t *testing.T, p string) (superblock, error) {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	return probe(f)
}

// The probe names the filesystem a disk's superblock names: ext4, made by
// mkfs.ext4, and EROFS, made by go-erofs, and an ext4 whose journal needs
// recovery. Anything else, zeros or a file too short for a superblock, is
// neither (STO-4).
func TestProbe(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ext4 := mkfsExt4(t, dir)
	recovering := mkfsExt4(t, t.TempDir())
	setNeedsRecovery(t, recovering)
	ero := filepath.Join(dir, "disk.erofs")
	f, err := os.Create(ero)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(erofs.Create(f).Close(), f.Close()); err != nil {
		t.Fatal(err)
	}
	zeros, short := filepath.Join(dir, "zeros"), filepath.Join(dir, "short")
	if err := os.WriteFile(zeros, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(short, make([]byte, 1030), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		path string
		want superblock
	}{
		"ext4":           {ext4, superblock{fstype: fsExt4}},
		"needs recovery": {recovering, superblock{fstype: fsExt4, needsRecovery: true}},
		"EROFS":          {ero, superblock{fstype: fsErofs}},
	} {
		got, err := probeFile(t, tc.path)
		if err != nil || got != tc.want {
			t.Errorf("%s: %+v, %v, want %+v", name, got, err, tc.want)
		}
	}
	for _, p := range []string{zeros, short} {
		if got, err := probeFile(t, p); !errors.Is(err, errNotExt4OrErofs) || err.Error() != "not ext4 or EROFS" {
			t.Errorf("%s: %+v, %v, want not ext4 or EROFS", filepath.Base(p), got, err)
		}
	}
}

// writeSerials makes a sysfs block directory holding a device for each
// name, with its serial if it has one, and returns it.
func writeSerials(t *testing.T, serials map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, serial := range serials {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if serial == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name, "serial"), []byte(serial), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A disk is the one device whose serial, as sysfs shows it with its
// newline, is its block id, wherever it is in device order; a device with
// no serial is skipped. No such device, or two, is an error naming the
// serial.
func TestFindDisk(t *testing.T) {
	t.Parallel()
	dir := writeSerials(t, map[string]string{"vda": "scratch\n", "vdb": "disk1\n", "vdc": "disk0\n", "loop0": ""})
	for id, want := range map[string]string{"disk0": "/dev/vdc", "disk1": "/dev/vdb"} {
		if got, err := findDiskIn(dir, id); err != nil || got != want {
			t.Errorf("%s: %q, %v, want %s", id, got, err, want)
		}
	}
	if _, err := findDiskIn(dir, "disk2"); err == nil || err.Error() != "no block device has the serial disk2" {
		t.Errorf("disk2: %v", err)
	}
	if _, err := findDiskIn(dir, "disk"); err == nil {
		t.Error("a prefix of a serial found a disk")
	}
	twice := writeSerials(t, map[string]string{"vdb": "disk0\n", "vdc": "disk0\n"})
	if _, err := findDiskIn(twice, "disk0"); err == nil || err.Error() != "block devices vdb, vdc all have the serial disk0" {
		t.Errorf("twice: %v", err)
	}
}
