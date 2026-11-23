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
// when the agent starts: the BootConfig message, binary protobuf, beside the
// agent, /.runner-microvm/agent. The plugin writes it there
// (internal/plugin's bootConfigPath, which its tests check against this).
const BootConfigPath = "/.runner-microvm/boot-config.binpb"

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
	// resolveScratch finds where the scratch path guestPath lands in the
	// job's root at root, creating it if the image lacks it, and the owner
	// and mode the image gives it there (GST-13).
	resolveScratch(root, guestPath string) (scratchTarget, error)
	// bindScratch makes dir on the scratch disk, with t's owner and mode,
	// and bind-mounts it over t's place in the job's root at root.
	bindScratch(root, dir string, t scratchTarget) error
	// makeDir creates guestPath in the job's root at root, with mode, unless
	// the image has something there.
	makeDir(root, guestPath string, mode uint32) error
	// moveRoot moves the mount at root to / and chroots into it.
	moveRoot(root string) error
}

// apiMount is a mount the agent makes in the job's root.
type apiMount struct {
	source, target, fstype string
	flags                  uintptr
	data                   string
}

// apiMounts are the mounts the agent always makes in the job's root, in
// order: what container engines give a container, so images and steps find
// what they expect.
var apiMounts = []apiMount{
	{"proc", "/proc", "proc", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
	{"sysfs", "/sys", "sysfs", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
	{"cgroup2", "/sys/fs/cgroup", "cgroup2", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
	{"devtmpfs", "/dev", "devtmpfs", syscall.MS_NOSUID, ""},
	{"devpts", "/dev/pts", "devpts", syscall.MS_NOSUID | syscall.MS_NOEXEC, "gid=5,mode=620,ptmxmode=666"},
	{"tmpfs", "/dev/shm", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
}

// ReadBootConfig reads the boot config at path.
func ReadBootConfig(path string) (*agentv1.BootConfig, error) {
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
//  5. it makes the API mounts in it;
//  6. it creates the act path, tool cache and temp directory the image
//     lacks (PROTO-7), so a scratch path on one finds its mode;
//  7. it resolves every scratch path in it, then makes each one, parents
//     first, a fresh directory on the scratch disk, named by its position
//     in the list;
//  8. it creates the act path, tool cache and temp directory again, so one
//     at or below a scratch path, hidden by its fresh directory, is made on
//     the scratch disk; elsewhere they are there now, and left alone;
//  9. it moves it to / and chroots.
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
	guestDirs := func() error {
		for _, d := range cfg.GetGuestDirs() {
			if err := m.makeDir(root, d.GetPath(), d.GetMode()); err != nil {
				return err
			}
		}
		return nil
	}
	steps = append(steps, guestDirs)
	if paths := cfg.GetScratchPaths(); len(paths) > 0 {
		dirs := scratch + "/paths"
		steps = append(steps,
			func() error { return m.mkdirAll(dirs) },
			func() error { return makeScratchPaths(m, root, dirs, paths) },
			guestDirs,
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

// scratchTarget is where a scratch path lands in the job's root, and the
// owner and mode the image gives it there.
type scratchTarget struct {
	// guestPath is the path as the profile lists it.
	guestPath string
	// resolved is where it lands: absolute in the job's root, with every
	// symlink resolved.
	resolved string
	uid, gid int
	mode     uint32
}

// apiMountRoots are the API mounts no scratch path may land in: binding a
// fresh directory over them, or under them, would hide the kernel's files.
var apiMountRoots = []string{"/proc", "/sys", "/dev"}

// makeScratchPaths makes the scratch paths paths in the job's root at root,
// each from dirs/<i>, i its position in paths. It resolves every path before
// it binds any, so each one keeps the image's owner and mode even below
// another scratch path, whose fresh directory would hide the image's copy.
// Then it binds them parents first, by where they land.
func makeScratchPaths(m mounter, root, dirs string, paths []string) error {
	targets := make([]scratchTarget, len(paths))
	for i, p := range paths {
		t, err := m.resolveScratch(root, p)
		if err != nil {
			return err
		}
		if api, ok := inAPIMount(t.resolved); ok {
			return fmt.Errorf("scratch path %s: lands at %s, in %s", p, t.resolved, api)
		}
		for _, prev := range targets[:i] {
			if prev.resolved == t.resolved {
				return fmt.Errorf("scratch path %s: lands at %s, as %s does", p, t.resolved, prev.guestPath)
			}
		}
		targets[i] = t
	}
	for _, i := range parentsFirst(targets) {
		if err := m.bindScratch(root, dirs+"/"+strconv.Itoa(i), targets[i]); err != nil {
			return err
		}
	}
	return nil
}

// parentsFirst returns the indexes of targets, ordered so that a target
// comes after every target whose place is above its own, and otherwise in
// the list's order.
func parentsFirst(targets []scratchTarget) []int {
	order := make([]int, 0, len(targets))
	placed := make([]bool, len(targets))
	// waits reports whether i has an unplaced target above it.
	waits := func(i int) bool {
		for j, u := range targets {
			if !placed[j] && strings.HasPrefix(targets[i].resolved, u.resolved+"/") {
				return true
			}
		}
		return false
	}
	for len(order) < len(targets) {
		// The first unplaced target with nothing unplaced above it: one
		// always exists, since being above is a strict order.
		for i := range targets {
			if !placed[i] && !waits(i) {
				order, placed[i] = append(order, i), true
				break
			}
		}
	}
	return order
}

// checkBootConfig refuses a config whose layer names would leave the layer
// share, that names no scratch device or vsock port, whose scratch paths or
// guest directories aren't absolute, clean paths below /, or whose guest
// directory's mode isn't permission bits.
func checkBootConfig(cfg *agentv1.BootConfig) error {
	if cfg.GetVsockPort() == 0 {
		return fmt.Errorf("boot config: no vsock port")
	}
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
	for _, p := range cfg.GetScratchPaths() {
		if !belowRoot(p) {
			return fmt.Errorf("boot config: scratch path %q is not an absolute, clean path below /", p)
		}
	}
	for _, d := range cfg.GetGuestDirs() {
		if !belowRoot(d.GetPath()) {
			return fmt.Errorf("boot config: directory %q is not an absolute, clean path below /", d.GetPath())
		}
		if d.GetMode() > 0o7777 {
			return fmt.Errorf("boot config: directory %s: mode %o is more than permission bits", d.GetPath(), d.GetMode())
		}
	}
	return nil
}

// belowRoot reports whether p is an absolute, clean path below /.
func belowRoot(p string) bool {
	return path.IsAbs(p) && path.Clean(p) == p && p != "/" && !strings.ContainsRune(p, 0)
}
