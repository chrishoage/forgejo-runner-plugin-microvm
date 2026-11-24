// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// fakeMounter records the boot's calls, and fails the first whose
// description contains fail. A scratch path resolves to itself, root-owned,
// mode 0755, unless image names its target; a mount's target resolves to
// itself unless targets names where it lands, and a guest directory lands
// at itself unless dirs names where it lands. A disk's block id disk<i> is
// the device /dev/vd<b+i> unless devices names another, or "" for none, and
// holds ext4 unless superblocks names what its device holds.
type fakeMounter struct {
	calls       []string
	fail        string
	image       map[string]scratchTarget
	targets     map[string]string
	dirs        map[string]string
	devices     map[string]string
	superblocks map[string]superblock
}

func (m *fakeMounter) do(call string) error {
	m.calls = append(m.calls, call)
	if m.fail != "" && strings.Contains(call, m.fail) {
		return syscall.EINVAL
	}
	return nil
}

func (m *fakeMounter) mkdirAll(path string) error { return m.do("mkdir " + path) }

func (m *fakeMounter) mount(source, target, fstype string, flags uintptr, data string) error {
	return m.do(fmt.Sprintf("mount %s %s %s %#x %s", source, target, fstype, flags, data))
}

func (m *fakeMounter) overlay(lowers []string, upper, work, target string) error {
	return m.do(fmt.Sprintf("overlay %s upper=%s work=%s on %s", strings.Join(lowers, ":"), upper, work, target))
}

func (m *fakeMounter) resolveScratch(root, guestPath string) (scratchTarget, error) {
	t, ok := m.image[guestPath]
	if !ok {
		t = scratchTarget{resolved: guestPath, mode: 0o755}
	}
	t.guestPath = guestPath
	return t, m.do(fmt.Sprintf("resolve %s in %s", guestPath, root))
}

func (m *fakeMounter) bindScratch(root, dir string, t scratchTarget) error {
	return m.do(fmt.Sprintf("scratch %s at %s %d:%d:%o from %s in %s", t.guestPath, t.resolved, t.uid, t.gid, t.mode, dir, root))
}

func (m *fakeMounter) makeDir(root, guestPath string, mode uint32) (string, error) {
	place, ok := m.dirs[guestPath]
	if !ok {
		place = guestPath
	}
	return place, m.do(fmt.Sprintf("dir %s %o in %s", guestPath, mode, root))
}

func (m *fakeMounter) resolveTarget(root, guestPath string) (string, error) {
	place, ok := m.targets[guestPath]
	if !ok {
		place = guestPath
	}
	return place, m.do(fmt.Sprintf("resolve target %s in %s", guestPath, root))
}

func (m *fakeMounter) mountShare(root, tag, place string, readOnly bool) error {
	return m.do(fmt.Sprintf("share %s at %s ro=%v in %s", tag, place, readOnly, root))
}

func (m *fakeMounter) findDisk(root, blockID string) (string, error) {
	device, ok := m.devices[blockID]
	if !ok {
		var i int
		if _, err := fmt.Sscanf(blockID, "disk%d", &i); err == nil {
			device = "/dev/vd" + string(rune('b'+i))
		}
	}
	if err := m.do(fmt.Sprintf("find %s in %s", blockID, root)); err != nil {
		return "", err
	}
	if device == "" {
		return "", fmt.Errorf("no block device has the serial %s", blockID)
	}
	return device, nil
}

func (m *fakeMounter) probeDisk(device string) (superblock, error) {
	sb, ok := m.superblocks[device]
	if !ok {
		sb = superblock{fstype: fsExt4}
	}
	if err := m.do("probe " + device); err != nil {
		return superblock{}, err
	}
	if sb.fstype == "" {
		return superblock{}, errNotExt4OrErofs
	}
	return sb, nil
}

func (m *fakeMounter) mountDisk(root, device, fstype, place string, readOnly bool) error {
	return m.do(fmt.Sprintf("disk %s %s at %s ro=%v in %s", fstype, device, place, readOnly, root))
}

func (m *fakeMounter) moveRoot(root string) error { return m.do("move root " + root) }

// testGuestDirs are the act path, tool cache and temp directory the plugin
// sends.
func testGuestDirs() []*agentv1.GuestDir {
	return []*agentv1.GuestDir{{Path: "/var/run/act", Mode: 0o777}, {Path: "/opt/hostedtoolcache", Mode: 0o777}, {Path: "/tmp", Mode: 0o1777}}
}

func testBootConfig() *agentv1.BootConfig {
	return &agentv1.BootConfig{
		LayerDir:      "/dev/.runner-microvm/layers",
		LayerTag:      "the-layers",
		Layers:        []string{"0.erofs", "1.erofs", "2.erofs"},
		ScratchDevice: "/dev/vda",
		VsockPort:     1024,
	}
}

// The agent builds the root from the boot config: the scratch disk, the
// layer share read-only, by the boot config's tag, each layer as a file-backed EROFS, and one overlay
// of the layers, top first, over the boot root, with its upper and work
// directories on the scratch disk. It makes the API mounts in it, then moves
// it to / (GST-2).
func TestBootBuildsTheRoot(t *testing.T) {
	t.Parallel()
	m := &fakeMounter{}
	if _, err := boot(m, testBootConfig()); err != nil {
		t.Fatal(err)
	}
	const s = "/dev/.runner-microvm"
	const ro, api = syscall.MS_RDONLY, syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC
	want := []string{
		"mkdir " + s + "/scratch",
		fmt.Sprintf("mount /dev/vda %s/scratch ext4 %#x ", s, 0),
		"mkdir " + s + "/scratch/upper",
		"mkdir " + s + "/scratch/work",
		"mkdir " + s + "/layers",
		fmt.Sprintf("mount the-layers %s/layers virtiofs %#x ", s, ro),
		"mkdir " + s + "/l/0",
		fmt.Sprintf("mount %s/layers/0.erofs %s/l/0 erofs %#x ", s, s, ro),
		"mkdir " + s + "/l/1",
		fmt.Sprintf("mount %s/layers/1.erofs %s/l/1 erofs %#x ", s, s, ro),
		"mkdir " + s + "/l/2",
		fmt.Sprintf("mount %s/layers/2.erofs %s/l/2 erofs %#x ", s, s, ro),
		"mkdir " + s + "/root",
		fmt.Sprintf("overlay %s/l/2:%s/l/1:%s/l/0:/ upper=%s/scratch/upper work=%s/scratch/work on %s/root", s, s, s, s, s, s),
		"mkdir " + s + "/root/proc",
		fmt.Sprintf("mount proc %s/root/proc proc %#x ", s, api),
		"mkdir " + s + "/root/sys",
		fmt.Sprintf("mount sysfs %s/root/sys sysfs %#x ", s, api),
		"mkdir " + s + "/root/sys/fs/cgroup",
		fmt.Sprintf("mount cgroup2 %s/root/sys/fs/cgroup cgroup2 %#x ", s, api),
		"mkdir " + s + "/root/dev",
		fmt.Sprintf("mount devtmpfs %s/root/dev devtmpfs %#x ", s, syscall.MS_NOSUID),
		"mkdir " + s + "/root/dev/pts",
		fmt.Sprintf("mount devpts %s/root/dev/pts devpts %#x gid=5,mode=620,ptmxmode=666", s, syscall.MS_NOSUID|syscall.MS_NOEXEC),
		"mkdir " + s + "/root/dev/shm",
		fmt.Sprintf("mount tmpfs %s/root/dev/shm tmpfs %#x ", s, api),
		"move root " + s + "/root",
	}
	if !slices.Equal(m.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(m.calls, "\n"), strings.Join(want, "\n"))
	}
}

// The act path, tool cache and temp directory are made after the API
// mounts, so a scratch path on one of them finds it with its mode. The
// scratch paths are made next, each from the directory on the scratch disk
// named by its position in the list (GST-13). Every path is resolved
// before any is bound, so each keeps the image's owner and mode even below
// another scratch path; then they are bound parents first by where they
// land, and otherwise in the list's order. Then the act path, tool cache
// and temp directory are made again, before the root moves, so a scratch
// path at or above one, here /tmp, doesn't hide it.
func TestBootMakesTheScratchPaths(t *testing.T) {
	t.Parallel()
	cfg := testBootConfig()
	cfg.ScratchPaths = []string{"/var/lib/docker/volumes", "/tmp", "/var/lib/docker", "/a/b/c", "/srv"}
	cfg.GuestDirs = testGuestDirs()
	m := &fakeMounter{image: map[string]scratchTarget{
		"/var/lib/docker":         {resolved: "/var/lib/docker", mode: 0o711},
		"/var/lib/docker/volumes": {resolved: "/var/lib/docker/volumes", uid: 12, gid: 34, mode: 0o700},
	}}
	if _, err := boot(m, cfg); err != nil {
		t.Fatal(err)
	}
	const s = "/dev/.runner-microvm"
	want := []string{
		fmt.Sprintf("mount tmpfs %s/root/dev/shm tmpfs %#x ", s, syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC),
		"dir /var/run/act 777 in " + s + "/root",
		"dir /opt/hostedtoolcache 777 in " + s + "/root",
		"dir /tmp 1777 in " + s + "/root",
		"mkdir " + s + "/scratch/paths",
		"resolve /var/lib/docker/volumes in " + s + "/root",
		"resolve /tmp in " + s + "/root",
		"resolve /var/lib/docker in " + s + "/root",
		"resolve /a/b/c in " + s + "/root",
		"resolve /srv in " + s + "/root",
		"scratch /tmp at /tmp 0:0:755 from " + s + "/scratch/paths/1 in " + s + "/root",
		"scratch /var/lib/docker at /var/lib/docker 0:0:711 from " + s + "/scratch/paths/2 in " + s + "/root",
		"scratch /var/lib/docker/volumes at /var/lib/docker/volumes 12:34:700 from " + s + "/scratch/paths/0 in " + s + "/root",
		"scratch /a/b/c at /a/b/c 0:0:755 from " + s + "/scratch/paths/3 in " + s + "/root",
		"scratch /srv at /srv 0:0:755 from " + s + "/scratch/paths/4 in " + s + "/root",
		"dir /var/run/act 777 in " + s + "/root",
		"dir /opt/hostedtoolcache 777 in " + s + "/root",
		"dir /tmp 1777 in " + s + "/root",
		"move root " + s + "/root",
	}
	if got := m.calls[len(m.calls)-len(want):]; !slices.Equal(got, want) {
		t.Errorf("last calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// No scratch paths: no directory for them.
	m = &fakeMounter{}
	if _, err := boot(m, testBootConfig()); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(m.calls, func(c string) bool { return strings.Contains(c, "paths") }) {
		t.Errorf("calls without scratch paths: %v", m.calls)
	}
}

// The order goes by where each path lands, not by how it is listed: one
// under an image symlink is bound after a path above where it lands.
func TestBootOrdersScratchPathsByWhereTheyLand(t *testing.T) {
	t.Parallel()
	cfg := testBootConfig()
	// The image has /deep/x/y -> /top.
	cfg.ScratchPaths = []string{"/top/z", "/deep/x/y"}
	m := &fakeMounter{image: map[string]scratchTarget{"/deep/x/y": {resolved: "/top", mode: 0o755}}}
	if _, err := boot(m, cfg); err != nil {
		t.Fatal(err)
	}
	var binds []string
	for _, c := range m.calls {
		if strings.HasPrefix(c, "scratch ") {
			binds = append(binds, strings.Fields(c)[3])
		}
	}
	if want := []string{"/top", "/top/z"}; !slices.Equal(binds, want) {
		t.Errorf("bound %v, want %v", binds, want)
	}
}

// A scratch path that lands in /proc, /sys or /dev, through an image
// symlink, or where another one lands, fails the boot, naming it, before
// any is bound.
func TestBootRefusesAScratchPathLandingBadly(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		image map[string]scratchTarget
		want  string
	}{
		"dev":   {map[string]scratchTarget{"/var/lib/docker": {resolved: "/dev"}}, "scratch path /var/lib/docker: lands at /dev, in /dev"},
		"sys":   {map[string]scratchTarget{"/var/lib/docker": {resolved: "/sys/fs/cgroup/x"}}, "scratch path /var/lib/docker: lands at /sys/fs/cgroup/x, in /sys"},
		"proc":  {map[string]scratchTarget{"/var/lib/docker": {resolved: "/proc/1"}}, "scratch path /var/lib/docker: lands at /proc/1, in /proc"},
		"twice": {map[string]scratchTarget{"/var/lib/docker": {resolved: "/srv"}}, "scratch path /var/lib/docker: lands at /srv, as /srv does"},
	} {
		cfg := testBootConfig()
		cfg.ScratchPaths = []string{"/srv", "/var/lib/docker"}
		m := &fakeMounter{image: tc.image}
		_, err := boot(m, cfg)
		if err == nil || !strings.HasSuffix(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %s", name, err, tc.want)
		}
		if slices.ContainsFunc(m.calls, func(c string) bool { return strings.HasPrefix(c, "scratch ") || strings.HasPrefix(c, "move root") }) {
			t.Errorf("%s: bound or moved: %v", name, m.calls)
		}
	}
	// /devices is not in /dev.
	cfg := testBootConfig()
	cfg.ScratchPaths = []string{"/devices"}
	if _, err := boot(&fakeMounter{}, cfg); err != nil {
		t.Errorf("/devices: %v", err)
	}
}

// The mounts are made after the scratch paths and the second pass of the
// act path, tool cache and temp directory, just before the root moves:
// every target is resolved first, then each is mounted by its tag, at
// where it lands, in the list's order, read-only unless the entry is
// read-write (STO-2, STO-6).
func TestBootMakesTheMounts(t *testing.T) {
	t.Parallel()
	cfg := testBootConfig()
	cfg.ScratchPaths = []string{"/var/lib/docker"}
	cfg.GuestDirs = testGuestDirs()
	// The image has /link -> /real.
	cfg.Mounts = []*agentv1.Mount{
		{Tag: "mount0", Target: "/real/x"},
		{Tag: "mount1", Target: "/var/lib/docker/tools"},
		{Tag: "mount2", Target: "/link/sub", ReadWrite: true},
	}
	m := &fakeMounter{targets: map[string]string{"/link/sub": "/real/sub"}}
	if _, err := boot(m, cfg); err != nil {
		t.Fatal(err)
	}
	const r = "/dev/.runner-microvm/root"
	want := []string{
		"scratch /var/lib/docker at /var/lib/docker 0:0:755 from /dev/.runner-microvm/scratch/paths/0 in " + r,
		"dir /var/run/act 777 in " + r,
		"dir /opt/hostedtoolcache 777 in " + r,
		"dir /tmp 1777 in " + r,
		"resolve target /real/x in " + r,
		"resolve target /var/lib/docker/tools in " + r,
		"resolve target /link/sub in " + r,
		"share mount0 at /real/x ro=true in " + r,
		"share mount1 at /var/lib/docker/tools ro=true in " + r,
		"share mount2 at /real/sub ro=false in " + r,
		"move root " + r,
	}
	if got := m.calls[len(m.calls)-len(want):]; !slices.Equal(got, want) {
		t.Errorf("last calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Without scratch paths, the mounts still come after the directories.
	cfg = testBootConfig()
	cfg.GuestDirs = testGuestDirs()
	cfg.Mounts = []*agentv1.Mount{{Tag: "mount0", Target: "/opt/tools"}}
	m = &fakeMounter{}
	if _, err := boot(m, cfg); err != nil {
		t.Fatal(err)
	}
	want = []string{
		"dir /tmp 1777 in " + r,
		"resolve target /opt/tools in " + r,
		"share mount0 at /opt/tools ro=true in " + r,
		"move root " + r,
	}
	if got := m.calls[len(m.calls)-len(want):]; !slices.Equal(got, want) {
		t.Errorf("last calls without scratch paths:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A mount whose target lands in /proc, /sys or /dev, at the root, or at,
// under or above where another one lands, through an image symlink, fails
// the boot, naming it, before any is mounted: under another, its mount point
// would be made in the host directory mounted there (STO-6).
func TestBootRefusesAMountLandingBadly(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		targets map[string]string
		want    string
	}{
		"dev":   {map[string]string{"/opt/tools": "/dev/x"}, "mount /opt/tools: lands at /dev/x, in /dev"},
		"proc":  {map[string]string{"/opt/tools": "/proc"}, "mount /opt/tools: lands at /proc, in /proc"},
		"root":  {map[string]string{"/opt/tools": "/"}, "mount /opt/tools: lands at /, the root"},
		"twice": {map[string]string{"/opt/tools": "/srv"}, "mount /opt/tools: lands at /srv, as mount /srv does"},
		"under another": {
			map[string]string{"/opt/tools": "/srv/x"},
			"mount /opt/tools: lands at /srv/x, under /srv, where mount /srv lands: its mount point would be made in what is mounted there",
		},
		"above another": {
			map[string]string{"/srv": "/opt/tools/x"},
			"mount /opt/tools: lands at /opt/tools, above /opt/tools/x, where mount /srv lands: its mount point would be made in what is mounted there",
		},
	} {
		cfg := testBootConfig()
		cfg.Mounts = []*agentv1.Mount{{Tag: "mount0", Target: "/srv"}, {Tag: "mount1", Target: "/opt/tools"}}
		m := &fakeMounter{targets: tc.targets}
		_, err := boot(m, cfg)
		if err == nil || !strings.HasSuffix(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %s", name, err, tc.want)
		}
		if slices.ContainsFunc(m.calls, func(c string) bool { return strings.HasPrefix(c, "share ") || strings.HasPrefix(c, "move root") }) {
			t.Errorf("%s: mounted or moved: %v", name, m.calls)
		}
	}
}

// A mount whose target lands at or above where a scratch path or a guest
// directory landed, through an image symlink, fails the boot, naming both,
// before any is mounted: it would hide what is there (STO-6). One under
// them is mounted.
func TestBootRefusesAMountHidingAScratchPathOrDirectory(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		targets, dirs map[string]string
		want          string
	}{
		"at a scratch path": {
			map[string]string{"/opt/tools": "/srv"},
			nil,
			"mount /opt/tools: lands at /srv, at or above /srv, where scratch path /srv lands: it would hide it",
		},
		"above a scratch path": {
			map[string]string{"/opt/tools": "/var/lib"},
			nil,
			"mount /opt/tools: lands at /var/lib, at or above /var/lib/docker, where scratch path /var/lib/docker lands: it would hide it",
		},
		"at a directory": {
			map[string]string{"/opt/tools": "/tmp"},
			nil,
			"mount /opt/tools: lands at /tmp, at or above /tmp, where directory /tmp lands: it would hide it",
		},
		"above a directory": {
			map[string]string{"/opt/tools": "/run"},
			map[string]string{"/var/run/act": "/run/act"},
			"mount /opt/tools: lands at /run, at or above /run/act, where directory /var/run/act lands: it would hide it",
		},
	} {
		cfg := testBootConfig()
		cfg.ScratchPaths = []string{"/srv", "/var/lib/docker"}
		cfg.GuestDirs = testGuestDirs()
		cfg.Mounts = []*agentv1.Mount{{Tag: "mount0", Target: "/data"}, {Tag: "mount1", Target: "/opt/tools"}}
		m := &fakeMounter{targets: tc.targets, dirs: tc.dirs}
		_, err := boot(m, cfg)
		if err == nil || !strings.HasSuffix(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %s", name, err, tc.want)
		}
		if slices.ContainsFunc(m.calls, func(c string) bool { return strings.HasPrefix(c, "share ") || strings.HasPrefix(c, "move root") }) {
			t.Errorf("%s: mounted or moved: %v", name, m.calls)
		}
	}
	// Under them, and beside them sharing a prefix, is fine.
	cfg := testBootConfig()
	cfg.ScratchPaths = []string{"/srv"}
	cfg.GuestDirs = testGuestDirs()
	cfg.Mounts = []*agentv1.Mount{{Tag: "mount0", Target: "/srv/tools"}, {Tag: "mount1", Target: "/tmp/x"}, {Tag: "mount2", Target: "/srvx"}}
	if _, err := boot(&fakeMounter{}, cfg); err != nil {
		t.Errorf("under: %v", err)
	}
}

// The disks are made with the mounts: each is found by its serial and its
// superblock read before any target is resolved; then every target, mounts'
// then disks', is resolved, and each is mounted where it lands, mounts
// before disks in the lists' order, a disk by the filesystem its superblock
// names, read-only unless the entry is read-write (STO-4, STO-6).
func TestBootMakesTheDisks(t *testing.T) {
	t.Parallel()
	cfg := testBootConfig()
	cfg.GuestDirs = testGuestDirs()
	cfg.Mounts = []*agentv1.Mount{{Tag: "mount0", Target: "/srv/tools"}}
	// The image has /link -> /real.
	cfg.Disks = []*agentv1.Disk{
		{BlockId: "disk0", Target: "/link/store"},
		{BlockId: "disk1", Target: "/opt/tools"},
		{BlockId: "disk2", Target: "/var/lib/data", ReadWrite: true},
	}
	m := &fakeMounter{
		targets:     map[string]string{"/link/store": "/srv/store"},
		devices:     map[string]string{"disk0": "/dev/vdd", "disk2": "/dev/vdb"},
		superblocks: map[string]superblock{"/dev/vdc": {fstype: fsErofs}},
	}
	if _, err := boot(m, cfg); err != nil {
		t.Fatal(err)
	}
	const r = "/dev/.runner-microvm/root"
	want := []string{
		"dir /tmp 1777 in " + r,
		"find disk0 in " + r,
		"probe /dev/vdd",
		"find disk1 in " + r,
		"probe /dev/vdc",
		"find disk2 in " + r,
		"probe /dev/vdb",
		"resolve target /srv/tools in " + r,
		"resolve target /link/store in " + r,
		"resolve target /opt/tools in " + r,
		"resolve target /var/lib/data in " + r,
		"share mount0 at /srv/tools ro=true in " + r,
		"disk ext4 /dev/vdd at /srv/store ro=true in " + r,
		"disk erofs /dev/vdc at /opt/tools ro=true in " + r,
		"disk ext4 /dev/vdb at /var/lib/data ro=false in " + r,
		"move root " + r,
	}
	if got := m.calls[len(m.calls)-len(want):]; !slices.Equal(got, want) {
		t.Errorf("last calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A disk no device has the serial of, one that holds neither ext4 nor
// EROFS, a read-only ext4 whose journal needs recovery, a read-write EROFS,
// and a disk whose target lands badly, under or above a mount included,
// fail the boot, naming the disk's target, before anything is mounted
// (STO-4, STO-6).
func TestBootRefusesABadDisk(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		m    *fakeMounter
		rw   bool
		want string
	}{
		"no device": {
			&fakeMounter{devices: map[string]string{"disk1": ""}}, false,
			"disk /opt/tools: no block device has the serial disk1",
		},
		"neither": {
			&fakeMounter{superblocks: map[string]superblock{"/dev/vdc": {}}}, false,
			"disk /opt/tools: not ext4 or EROFS",
		},
		"needs recovery": {
			&fakeMounter{superblocks: map[string]superblock{"/dev/vdc": {fstype: fsExt4, needsRecovery: true}}}, false,
			"disk /opt/tools: its ext4 journal needs recovery, which a read-only mount can't do: mount the disk read-write once to replay it",
		},
		"read-write EROFS": {
			&fakeMounter{superblocks: map[string]superblock{"/dev/vdc": {fstype: fsErofs}}}, true,
			"disk /opt/tools: read-write disks must be ext4",
		},
		"lands where a mount does": {
			&fakeMounter{targets: map[string]string{"/opt/tools": "/srv"}}, false,
			"disk /opt/tools: lands at /srv, as mount /srv does",
		},
		"lands under a mount": {
			&fakeMounter{targets: map[string]string{"/opt/tools": "/srv/x"}}, false,
			"disk /opt/tools: lands at /srv/x, under /srv, where mount /srv lands: its mount point would be made in what is mounted there",
		},
		"lands above a mount": {
			&fakeMounter{targets: map[string]string{"/srv": "/opt/tools/x"}}, false,
			"disk /opt/tools: lands at /opt/tools, above /opt/tools/x, where mount /srv lands: its mount point would be made in what is mounted there",
		},
		"lands in /dev": {
			&fakeMounter{targets: map[string]string{"/opt/tools": "/dev/x"}}, false,
			"disk /opt/tools: lands at /dev/x, in /dev",
		},
	} {
		cfg := testBootConfig()
		cfg.Mounts = []*agentv1.Mount{{Tag: "mount0", Target: "/srv"}}
		cfg.Disks = []*agentv1.Disk{{BlockId: "disk0", Target: "/data"}, {BlockId: "disk1", Target: "/opt/tools", ReadWrite: tc.rw}}
		_, err := boot(tc.m, cfg)
		if err == nil || !strings.HasSuffix(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %s", name, err, tc.want)
		}
		if slices.ContainsFunc(tc.m.calls, func(c string) bool {
			return strings.HasPrefix(c, "share ") || strings.HasPrefix(c, "disk ") || strings.HasPrefix(c, "move root")
		}) {
			t.Errorf("%s: mounted or moved: %v", name, tc.m.calls)
		}
	}
	// A read-write ext4 whose journal needs recovery is mounted: the mount
	// replays it.
	cfg := testBootConfig()
	cfg.Disks = []*agentv1.Disk{{BlockId: "disk0", Target: "/data", ReadWrite: true}}
	if _, err := boot(&fakeMounter{superblocks: map[string]superblock{"/dev/vdb": {fstype: fsExt4, needsRecovery: true}}}, cfg); err != nil {
		t.Errorf("a read-write disk needing recovery: %v", err)
	}
}

// A failed step stops the boot there, naming it, and the root is never
// moved.
func TestBootStopsAtAFailedStep(t *testing.T) {
	t.Parallel()
	for _, fail := range []string{
		"ext4", "virtiofs", "1.erofs", "overlay", "cgroup2", "dir /opt/hostedtoolcache", "resolve /opt", "scratch /srv",
		"resolve target /data", "share mount0", "find disk0", "probe /dev/vdb", "disk ext4",
	} {
		m := &fakeMounter{fail: fail}
		cfg := testBootConfig()
		cfg.ScratchPaths = []string{"/srv", "/opt"}
		cfg.GuestDirs = testGuestDirs()
		cfg.Mounts = []*agentv1.Mount{{Tag: "mount0", Target: "/data"}}
		cfg.Disks = []*agentv1.Disk{{BlockId: "disk0", Target: "/disk"}}
		_, err := boot(m, cfg)
		if err == nil || !errors.Is(err, syscall.EINVAL) {
			t.Errorf("%s fails: err = %v, want EINVAL", fail, err)
		}
		if slices.ContainsFunc(m.calls, func(c string) bool { return strings.HasPrefix(c, "move root") }) {
			t.Errorf("%s fails: the root was moved", fail)
		}
	}
}

// The boot config refuses what would leave the staging directory or name no
// device: it comes from the plugin, but a layer name becomes a path.
func TestBootRefusesABadConfig(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*agentv1.BootConfig){
		"no scratch device":  func(c *agentv1.BootConfig) { c.ScratchDevice = "" },
		"no vsock port":      func(c *agentv1.BootConfig) { c.VsockPort = 0 },
		"relative layer dir": func(c *agentv1.BootConfig) { c.LayerDir = "layers" },
		"no layer tag":       func(c *agentv1.BootConfig) { c.LayerTag = "" },
		"layer path":         func(c *agentv1.BootConfig) { c.Layers = []string{"../x"} },
		"empty layer":        func(c *agentv1.BootConfig) { c.Layers = []string{""} },
		"relative scratch":   func(c *agentv1.BootConfig) { c.ScratchPaths = []string{"srv"} },
		"unclean scratch":    func(c *agentv1.BootConfig) { c.ScratchPaths = []string{"/srv/../../x"} },
		"root scratch":       func(c *agentv1.BootConfig) { c.ScratchPaths = []string{"/"} },
		"empty scratch":      func(c *agentv1.BootConfig) { c.ScratchPaths = []string{""} },
		"relative dir":       func(c *agentv1.BootConfig) { c.GuestDirs = []*agentv1.GuestDir{{Path: "tmp", Mode: 0o1777}} },
		"unclean dir":        func(c *agentv1.BootConfig) { c.GuestDirs = []*agentv1.GuestDir{{Path: "/tmp/", Mode: 0o1777}} },
		"root dir":           func(c *agentv1.BootConfig) { c.GuestDirs = []*agentv1.GuestDir{{Path: "/", Mode: 0o755}} },
		"dir mode":           func(c *agentv1.BootConfig) { c.GuestDirs = []*agentv1.GuestDir{{Path: "/tmp", Mode: 0o10777}} },
		"no mount tag":       func(c *agentv1.BootConfig) { c.Mounts = []*agentv1.Mount{{Target: "/opt"}} },
		"relative mount":     func(c *agentv1.BootConfig) { c.Mounts = []*agentv1.Mount{{Tag: "mount0", Target: "opt"}} },
		"unclean mount":      func(c *agentv1.BootConfig) { c.Mounts = []*agentv1.Mount{{Tag: "mount0", Target: "/opt/"}} },
		"root mount":         func(c *agentv1.BootConfig) { c.Mounts = []*agentv1.Mount{{Tag: "mount0", Target: "/"}} },
		"no block id":        func(c *agentv1.BootConfig) { c.Disks = []*agentv1.Disk{{Target: "/data"}} },
		"relative disk":      func(c *agentv1.BootConfig) { c.Disks = []*agentv1.Disk{{BlockId: "disk0", Target: "data"}} },
		"root disk":          func(c *agentv1.BootConfig) { c.Disks = []*agentv1.Disk{{BlockId: "disk0", Target: "/"}} },
	} {
		cfg := testBootConfig()
		edit(cfg)
		m := &fakeMounter{}
		if _, err := boot(m, cfg); err == nil {
			t.Errorf("%s: booted", name)
		}
		if len(m.calls) != 0 {
			t.Errorf("%s: made calls %v", name, m.calls)
		}
	}
}

func TestReadBootConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "boot")
	b, err := proto.Marshal(testBootConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadBootConfig(path)
	if err != nil || !proto.Equal(got, testBootConfig()) {
		t.Errorf("ReadBootConfig = %v, %v", got, err)
	}
	if _, err := ReadBootConfig(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing boot config was read")
	}
	if err := os.WriteFile(path, []byte("\xff\xff\xff"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBootConfig(path); err == nil {
		t.Error("a garbled boot config was read")
	}
}
