// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// BootConfigPath is the boot config's file in the boot root, the VM's root
// when the agent starts.
//
// SHORTCUT(#6): provisional pick: its name, its encoding (the BootConfig
// message, binary protobuf) and the agent's path are #6's to settle; the
// plugin writes the same name (internal/plugin's bootConfigPath), and the
// protocol test fails if they part.
const BootConfigPath = "/.runner-microvm-boot"

// layerTag is the virtiofs tag of the VM's layer link directory.
//
// SHORTCUT(#6): provisional pick, and the same tag is what the VM child
// shares (internal/job/internal/vm's layerTag); the protocol test fails if
// they part.
const layerTag = "layers"

// stage holds the scratch disk, each layer and the new root while the agent
// builds it. devtmpfs is the one writable place before the root exists: the
// boot root is read-only.
const stage = "/dev/.runner-microvm"

// mounter is the kernel's side of the boot, so the boot's steps are tested
// without privilege.
type mounter interface {
	mkdirAll(path string) error
	mount(source, target, fstype string, flags uintptr, data string) error
	// overlay mounts an overlay of lowers, top first, at target.
	overlay(lowers []string, upper, work, target string) error
	// moveRoot moves the mount at root to / and chroots into it.
	moveRoot(root string) error
}

// apiMount is a mount the agent makes in the job's root.
type apiMount struct {
	source, target, fstype string
	flags                  uintptr
	data                   string
}

// apiMounts are the mounts the agent makes in the job's root, in order.
//
// SHORTCUT(#6): provisional set and options, from #1's prototype choices.
var apiMounts = []apiMount{
	{"proc", "/proc", "proc", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
	{"sysfs", "/sys", "sysfs", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
	{"cgroup2", "/sys/fs/cgroup", "cgroup2", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
	{"devtmpfs", "/dev", "devtmpfs", syscall.MS_NOSUID, ""},
	{"devpts", "/dev/pts", "devpts", syscall.MS_NOSUID | syscall.MS_NOEXEC, "gid=5,mode=620,ptmxmode=666"},
	{"tmpfs", "/dev/shm", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
}

// readBootConfig reads the boot config at path.
func readBootConfig(path string) (*agentv1.BootConfig, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the boot root's own file
	if err != nil {
		return nil, fmt.Errorf("boot config: %w", err)
	}
	cfg := &agentv1.BootConfig{}
	if err := proto.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("boot config %s: %w", path, err)
	}
	return cfg, nil
}

// boot builds the job's root and moves it to / (GST-2):
//  1. it mounts the scratch disk (ext4);
//  2. it mounts the layer share read-only;
//  3. it mounts each layer file as a file-backed EROFS, with no loop device;
//  4. it builds an overlay of the layers, top first, over the boot root, the
//     VM's / at boot, with its upper and work directories on the scratch
//     disk;
//  5. it makes the API mounts in it, then moves it to / and chroots.
//
// The host enforces read-only on both shares, so the guest can't write a
// layer even after remounting one read-write (IMG-9).
func boot(m mounter, cfg *agentv1.BootConfig) error {
	if err := checkBootConfig(cfg); err != nil {
		return err
	}
	scratch := stage + "/scratch"
	upper, work := scratch+"/upper", scratch+"/work"
	steps := []func() error{
		func() error { return m.mkdirAll(scratch) },
		func() error { return m.mount(cfg.GetScratchDevice(), scratch, "ext4", 0, "") },
		func() error { return m.mkdirAll(upper) },
		func() error { return m.mkdirAll(work) },
		func() error { return m.mkdirAll(cfg.GetLayerDir()) },
		func() error { return m.mount(layerTag, cfg.GetLayerDir(), "virtiofs", syscall.MS_RDONLY, "") },
	}
	// Lower layers top first, the boot root at the bottom.
	lowers := []string{"/"}
	for i, name := range cfg.GetLayers() {
		dir := stage + "/l/" + strconv.Itoa(i)
		src := path.Join(cfg.GetLayerDir(), name)
		steps = append(steps,
			func() error { return m.mkdirAll(dir) },
			func() error { return m.mount(src, dir, "erofs", syscall.MS_RDONLY, "") },
		)
		lowers = append([]string{dir}, lowers...)
	}
	root := stage + "/root"
	steps = append(steps,
		func() error { return m.mkdirAll(root) },
		func() error { return m.overlay(lowers, upper, work, root) },
	)
	for _, a := range apiMounts {
		target := root + a.target
		steps = append(steps,
			func() error { return m.mkdirAll(target) },
			func() error { return m.mount(a.source, target, a.fstype, a.flags, a.data) },
		)
	}
	steps = append(steps, func() error { return m.moveRoot(root) })
	for _, step := range steps {
		if err := step(); err != nil {
			return fmt.Errorf("build the root: %w", err)
		}
	}
	return nil
}

// checkBootConfig refuses a config whose layer names would leave the layer
// share, or that names no scratch device.
func checkBootConfig(cfg *agentv1.BootConfig) error {
	if cfg.GetScratchDevice() == "" {
		return fmt.Errorf("boot config: no scratch device")
	}
	if !path.IsAbs(cfg.GetLayerDir()) {
		return fmt.Errorf("boot config: layer directory %q is not absolute", cfg.GetLayerDir())
	}
	for _, name := range cfg.GetLayers() {
		if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
			return fmt.Errorf("boot config: layer %q is not a file name", name)
		}
	}
	return nil
}
