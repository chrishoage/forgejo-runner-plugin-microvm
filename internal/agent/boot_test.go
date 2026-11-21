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
// description contains fail.
type fakeMounter struct {
	calls []string
	fail  string
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

func (m *fakeMounter) moveRoot(root string) error { return m.do("move root " + root) }

func testBootConfig() *agentv1.BootConfig {
	return &agentv1.BootConfig{
		LayerDir:      "/dev/.runner-microvm/layers",
		Layers:        []string{"0.erofs", "1.erofs", "2.erofs"},
		ScratchDevice: "/dev/vda",
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

// A failed step stops the boot there, naming it, and the root is never
// moved.
func TestBootStopsAtAFailedStep(t *testing.T) {
	t.Parallel()
	for _, fail := range []string{"ext4", "virtiofs", "1.erofs", "overlay", "cgroup2"} {
		m := &fakeMounter{fail: fail}
		err := boot(m, testBootConfig())
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
		"relative layer dir": func(c *agentv1.BootConfig) { c.LayerDir = "layers" },
		"layer path":         func(c *agentv1.BootConfig) { c.Layers = []string{"../x"} },
		"empty layer":        func(c *agentv1.BootConfig) { c.Layers = []string{""} },
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
	got, err := readBootConfig(path)
	if err != nil || !proto.Equal(got, testBootConfig()) {
		t.Errorf("readBootConfig = %v, %v", got, err)
	}
	if _, err := readBootConfig(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing boot config was read")
	}
	if err := os.WriteFile(path, []byte("\xff\xff\xff"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBootConfig(path); err == nil {
		t.Error("a garbled boot config was read")
	}
}
