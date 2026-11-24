// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"syscall"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// Boot returns the read-write disks it mounted, in the order it mounted
// them, each with where its target landed and its device's number, and
// none of the read-only disks.
func TestBootReturnsTheReadWriteDisks(t *testing.T) {
	t.Parallel()
	cfg := testBootConfig()
	cfg.Mounts = []*agentv1.Mount{{Tag: "mount0", Target: "/srv", ReadWrite: true}}
	cfg.Disks = []*agentv1.Disk{
		{BlockId: "disk0", Target: "/cache", ReadWrite: true},
		{BlockId: "disk1", Target: "/opt/tools"},
		{BlockId: "disk2", Target: "/store", ReadWrite: true},
	}
	m := &fakeMounter{
		targets: map[string]string{"/store": "/real/store", "/cache": "/real/cache"},
		superblocks: map[string]superblock{
			"/dev/vdb": {fstype: fsExt4, dev: 0xfe10},
			"/dev/vdc": {fstype: fsExt4, dev: 0xfe20},
			"/dev/vdd": {fstype: fsExt4, dev: 0xfe30},
		},
	}
	got, err := boot(m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []rwDisk{{"/cache", "/real/cache", 0xfe10}, {"/store", "/real/store", 0xfe30}}
	if !slices.Equal(got, want) {
		t.Errorf("boot returned %v, want %v", got, want)
	}
	if got, err := boot(&fakeMounter{}, testBootConfig()); err != nil || got != nil {
		t.Errorf("boot with no disks returned %v, %v", got, err)
	}
}

// fakeFlusher records Flush's calls to the kernel, and fails the freeze of
// a place in fail.
type fakeFlusher struct {
	calls []string
	fail  map[string]error
}

func (f *fakeFlusher) sync() { f.calls = append(f.calls, "sync") }

func (f *fakeFlusher) freeze(place string, dev uint64) error {
	f.calls = append(f.calls, fmt.Sprintf("freeze %s %#x", place, dev))
	return f.fail[place]
}

// Flush syncs, then freezes each read-write disk at its mount point, checked
// against its device; a disk that fails is named in the error, which is
// FailedPrecondition, and the disks after it are still frozen. With no
// read-write disk, it only syncs: the mounts' writes need nothing more.
func TestFlush(t *testing.T) {
	t.Parallel()
	f := &fakeFlusher{}
	s := &server{log: slog.New(slog.DiscardHandler), flusher: f, rwDisks: []rwDisk{
		{"/data", "/data", 0xfe10}, {"/cache", "/real/cache", 0xfe20},
	}}
	c := startServer(t, s)
	if _, err := c.Flush(testContext(t), &agentv1.FlushRequest{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"sync", "freeze /data 0xfe10", "freeze /real/cache 0xfe20"}; !slices.Equal(f.calls, want) {
		t.Errorf("Flush made %v, want %v", f.calls, want)
	}

	f.calls, f.fail = nil, map[string]error{"/data": syscall.EINVAL}
	_, err := c.Flush(testContext(t), &agentv1.FlushRequest{})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "disk /data: invalid argument") {
		t.Errorf("Flush = %v, want FailedPrecondition naming disk /data", err)
	}
	if want := []string{"sync", "freeze /data 0xfe10", "freeze /real/cache 0xfe20"}; !slices.Equal(f.calls, want) {
		t.Errorf("Flush made %v, want %v", f.calls, want)
	}

	none := &fakeFlusher{}
	c = startServer(t, &server{log: slog.New(slog.DiscardHandler), flusher: none})
	if _, err := c.Flush(testContext(t), &agentv1.FlushRequest{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"sync"}; !slices.Equal(none.calls, want) {
		t.Errorf("Flush with no read-write disk made %v, want %v", none.calls, want)
	}
}

// The real freeze refuses a place whose filesystem is not the disk's, as
// when a step mounted something over it, before it would freeze anything.
func TestFreezeRefusesAnotherFilesystem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var st syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		t.Fatal(err)
	}
	err := kernelFlusher{}.freeze(dir, st.Dev+1)
	if err == nil || !strings.Contains(err.Error(), "is no longer the disk's mount") {
		t.Errorf("freeze = %v, want it to refuse another filesystem", err)
	}
	if err := (kernelFlusher{}).freeze(dir+"/missing", st.Dev); err == nil {
		t.Error("freeze of a missing place succeeded")
	}
}
