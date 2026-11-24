// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The superblock magics the probe reads (STO-4): EROFS's at byte 1024 of
// the disk, and ext4's s_magic at byte 1080, 0x38 into its superblock, which
// also starts at byte 1024. ext2 and ext3 share ext4's; the ext4 driver
// mounts them too.
const (
	superblockOffset = 1024
	erofsMagic       = 0xE0F5E1E2
	ext4Magic        = 0xEF53
	ext4MagicAt      = 0x38
	// ext4IncompatAt is s_feature_incompat, and ext4IncompatRecover its
	// needs_recovery bit: the journal holds transactions not yet written
	// to the filesystem.
	ext4IncompatAt      = 0x60
	ext4IncompatRecover = 0x4
)

// The filesystems a disk may hold, as the kernel names them.
const (
	fsExt4  = "ext4"
	fsErofs = "erofs"
)

// errNotExt4OrErofs is the probe's error for a disk whose superblock names
// neither filesystem.
var errNotExt4OrErofs = errors.New("not ext4 or EROFS")

// superblock is what the probe found on a disk.
type superblock struct {
	// fstype is fsExt4 or fsErofs.
	fstype string
	// needsRecovery is set on an ext4 disk whose journal needs replaying.
	needsRecovery bool
}

// probe reads the superblock of the disk r and returns the filesystem it
// names: EROFS or ext4, by its magic. A disk too short to hold either
// superblock, or with neither magic, is errNotExt4OrErofs. EROFS is checked
// first: its superblock spans byte 1080, where one of its fields could
// happen to read as ext4's magic, while byte 1024 of an ext4 is its inode
// count, which no mkfs makes 0xE0F5E1E2.
func probe(r io.ReaderAt) (superblock, error) {
	b := make([]byte, ext4IncompatAt+4)
	n, err := r.ReadAt(b, superblockOffset)
	if err != nil && !errors.Is(err, io.EOF) {
		return superblock{}, err
	}
	b = b[:n]
	switch {
	case len(b) >= 4 && binary.LittleEndian.Uint32(b) == erofsMagic:
		return superblock{fstype: fsErofs}, nil
	case len(b) >= ext4IncompatAt+4 && binary.LittleEndian.Uint16(b[ext4MagicAt:]) == ext4Magic:
		incompat := binary.LittleEndian.Uint32(b[ext4IncompatAt:])
		return superblock{fstype: fsExt4, needsRecovery: incompat&ext4IncompatRecover != 0}, nil
	}
	return superblock{}, errNotExt4OrErofs
}

// probeDisk reads the superblock of the block device device.
func (kernel) probeDisk(device string) (superblock, error) {
	f, err := os.Open(device) //nolint:gosec // G304: a guest block device the agent found by its serial
	if err != nil {
		return superblock{}, err
	}
	defer func() { _ = f.Close() }()
	return probe(f)
}

// findDisk returns the block device, under /dev, whose virtio-blk serial is
// blockID, reading each device's serial from sysfs in the job's root at
// root, where the agent has mounted it. The serial is the block id the host
// attached the disk under; a disk is never found by its position, so a
// change in device order can't mount the wrong one. No device with that
// serial, or two, is an error.
func (kernel) findDisk(root, blockID string) (string, error) {
	return findDiskIn(root+"/sys/block", blockID)
}

// findDiskIn is findDisk on the sysfs block directory dir.
func findDiskIn(dir, blockID string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var found []string
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "serial")) //nolint:gosec // G304: sysfs's own files
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if strings.TrimRight(string(b), "\x00\n ") == blockID {
			found = append(found, e.Name())
		}
	}
	sort.Strings(found)
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no block device has the serial %s", blockID)
	case 1:
		return "/dev/" + found[0], nil
	}
	return "", fmt.Errorf("block devices %s all have the serial %s", strings.Join(found, ", "), blockID)
}
