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
// mode 0755, unless image names its target.
type fakeMounter struct {
	calls []string
	fail  string
	image map[string]scratchTarget
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

func (m *fakeMounter) makeDir(root, guestPath string, mode uint32) error {
	return m.do(fmt.Sprintf("dir %s %o in %s", guestPath, mode, root))
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
		Layers:        []string{"0.erofs", "1.erofs", "2.erofs"},
		ScratchDevice: "/dev/vda",
		VsockPort:     1024,
	}
}

// The agent builds the root from the boot config: the scratch disk, the
// layer share read-only, each layer as a file-backed EROFS, and one overlay
// of the layers, top first, over the boot root, with its upper and work
// directories on the scratch disk. It makes the API mounts in it, then moves
// it to / (GST-2).
func TestBootBuildsTheRoot(t *testing.T) {
	t.Parallel()
	m := &fakeMounter{}
	if err := boot(m, testBootConfig()); err != nil {
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
		fmt.Sprintf("mount layers %s/layers virtiofs %#x ", s, ro),
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
	if err := boot(m, cfg); err != nil {
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
	if err := boot(m, testBootConfig()); err != nil {
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
	if err := boot(m, cfg); err != nil {
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
		err := boot(m, cfg)
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
	if err := boot(&fakeMounter{}, cfg); err != nil {
		t.Errorf("/devices: %v", err)
	}
}

// A failed step stops the boot there, naming it, and the root is never
// moved.
func TestBootStopsAtAFailedStep(t *testing.T) {
	t.Parallel()
	for _, fail := range []string{"ext4", "virtiofs", "1.erofs", "overlay", "cgroup2", "dir /opt/hostedtoolcache", "resolve /opt", "scratch /srv"} {
		m := &fakeMounter{fail: fail}
		cfg := testBootConfig()
		cfg.ScratchPaths = []string{"/srv", "/opt"}
		cfg.GuestDirs = testGuestDirs()
		err := boot(m, cfg)
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
	} {
		cfg := testBootConfig()
		edit(cfg)
		m := &fakeMounter{}
		if err := boot(m, cfg); err == nil {
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
