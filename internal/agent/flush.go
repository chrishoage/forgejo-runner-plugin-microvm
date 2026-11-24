// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// fiFreeze is FIFREEZE, _IOWR('X', 119, int): it writes a filesystem's
// dirty data and its journal out, marks it clean, and blocks every write to
// it until it is thawed.
const fiFreeze = 0xC0045877

// rwDisk is a read-write disk as the agent mounted it at boot, which Flush
// freezes.
type rwDisk struct {
	// target is the disk's target, as the boot config names it.
	target string
	// place is where the target landed in the job's root, the disk's mount
	// point there.
	place string
	// dev is the block device's number, which every file on the disk's
	// filesystem carries as its st_dev.
	dev uint64
}

// flusher is the kernel's side of Flush, so it is tested without
// privilege.
type flusher interface {
	// sync writes every dirty page in the guest to its device: virtio-blk
	// for the disks, virtiofs for the mounts.
	sync()
	// freeze freezes the filesystem mounted at place, in the job's root,
	// that is the block device dev's.
	freeze(place string, dev uint64) error
}

// Flush writes everything on the VM's read-write mounts and disks through
// to the host, and leaves each read-write disk's ext4 clean, so its next
// mount needs no journal recovery (STO-9): sync(), then FIFREEZE on each
// read-write disk's mount. libkrun answers the guest's flushes with fsync
// of the host's files. The disks are never thawed: the plugin kills the VM
// right after. A disk that fails is reported, and the others are still
// frozen.
func (s *server) Flush(context.Context, *agentv1.FlushRequest) (*agentv1.FlushResponse, error) {
	f := s.flusher
	if f == nil {
		f = kernelFlusher{}
	}
	f.sync()
	var errs []error
	for _, d := range s.rwDisks {
		if err := f.freeze(d.place, d.dev); err != nil {
			errs = append(errs, fmt.Errorf("disk %s: %w", d.target, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "flush: %v", err)
	}
	s.log.Info("flushed", "disks", len(s.rwDisks))
	return &agentv1.FlushResponse{}, nil
}

// kernelFlusher is the real flusher.
type kernelFlusher struct{}

func (kernelFlusher) sync() { syscall.Sync() }

// freeze opens place and freezes its filesystem, if it still is the disk's:
// a step may have unmounted the disk, which leaves it clean already, or
// mounted something else over it, which must not be frozen in its stead. A
// filesystem already frozen, by an earlier Flush, is no error.
func (kernelFlusher) freeze(place string, dev uint64) error {
	fd, err := syscall.Open(place, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", place, err)
	}
	defer func() { _ = syscall.Close(fd) }()
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return fmt.Errorf("stat %s: %w", place, err)
	}
	if st.Dev != dev {
		return fmt.Errorf("%s is no longer the disk's mount", place)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), fiFreeze, 0); errno != 0 && errno != syscall.EBUSY {
		return fmt.Errorf("freeze %s: %w", place, errno)
	}
	return nil
}
