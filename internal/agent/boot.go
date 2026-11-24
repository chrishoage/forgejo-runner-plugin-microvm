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
	// the image has something there, and returns where the directory lands,
	// or "" when the image has something else there.
	makeDir(root, guestPath string, mode uint32) (string, error)
	// resolveTarget finds where the mount or disk target guestPath lands
	// in the job's root at root, creating it if the image lacks it
	// (STO-5, STO-6).
	resolveTarget(root, guestPath string) (string, error)
	// mountShare mounts the virtiofs share tag at place, where a target
	// landed, in the job's root at root, read-only when readOnly.
	mountShare(root, tag, place string, readOnly bool) error
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
//  2. it mounts the layer share read-only, by the boot config's tag;
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
//  9. it resolves every mount's target in it, then mounts each share, in
//     the list's order, none landing under another (STO-6);
//  10. it moves it to / and chroots.
//
// The host enforces read-only on both shares, so the guest can't write a
// layer even after remounting one read-write (IMG-9), and on each read-only
// mount (STO-2).
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
		func() error { return m.mount(cfg.GetLayerTag(), cfg.GetLayerDir(), "virtiofs", syscall.MS_RDONLY, "") },
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
	// Where each scratch path and guest directory lands, which no mount
	// may hide.
	var scratchHeld, dirsHeld []landing
	guestDirs := func() error {
		dirsHeld = dirsHeld[:0]
		for _, d := range cfg.GetGuestDirs() {
			place, err := m.makeDir(root, d.GetPath(), d.GetMode())
			if err != nil {
				return err
			}
			if place != "" {
				dirsHeld = append(dirsHeld, landing{"directory " + d.GetPath(), place})
			}
		}
		return nil
	}
	steps = append(steps, guestDirs)
	if paths := cfg.GetScratchPaths(); len(paths) > 0 {
		dirs := scratch + "/paths"
		steps = append(steps,
			func() error { return m.mkdirAll(dirs) },
			func() error {
				targets, err := makeScratchPaths(m, root, dirs, paths)
				for _, t := range targets {
					scratchHeld = append(scratchHeld, landing{"scratch path " + t.guestPath, t.resolved})
				}
				return err
			},
			guestDirs,
		)
	}
	if mounts := cfg.GetMounts(); len(mounts) > 0 {
		steps = append(steps, func() error {
			return makeMounts(m, root, mounts, append(append([]landing(nil), scratchHeld...), dirsHeld...))
		})
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

// landing is where a scratch path or a guest directory, what, landed in the
// job's root: a place no mount may be at or above.
type landing struct{ what, place string }

// makeScratchPaths makes the scratch paths paths in the job's root at root,
// each from dirs/<i>, i its position in paths, and returns where each lands.
// It resolves every path before it binds any, so each one keeps the image's
// owner and mode even below another scratch path, whose fresh directory
// would hide the image's copy. Then it binds them parents first, by where
// they land.
func makeScratchPaths(m mounter, root, dirs string, paths []string) ([]scratchTarget, error) {
	targets := make([]scratchTarget, len(paths))
	for i, p := range paths {
		t, err := m.resolveScratch(root, p)
		if err != nil {
			return nil, err
		}
		if api, ok := inAPIMount(t.resolved); ok {
			return nil, fmt.Errorf("scratch path %s: lands at %s, in %s", p, t.resolved, api)
		}
		for _, prev := range targets[:i] {
			if prev.resolved == t.resolved {
				return nil, fmt.Errorf("scratch path %s: lands at %s, as %s does", p, t.resolved, prev.guestPath)
			}
		}
		targets[i] = t
	}
	places := make([]string, len(targets))
	for i, t := range targets {
		places[i] = t.resolved
	}
	for _, i := range parentsFirst(places) {
		if err := m.bindScratch(root, dirs+"/"+strconv.Itoa(i), targets[i]); err != nil {
			return nil, err
		}
	}
	return targets, nil
}

// makeMounts mounts each of mounts, the profile's mounts, in the job's root
// at root, after the scratch paths (STO-6). It resolves every target before
// it mounts any, each inside the root, creating one the image lacks, so a
// target under a scratch path lands on the scratch path's fresh directory;
// then it mounts them in the list's order. A target that lands in /proc,
// /sys or /dev, at the root, at or above a place in held, where a scratch
// path or a guest directory landed, or at, under or above where another
// target lands, fails, naming both: the mount would hide what is there, or
// its mount point would be made in the host directory mounted above it
// (STO-6).
func makeMounts(m mounter, root string, mounts []*agentv1.Mount, held []landing) error {
	places := make([]string, len(mounts))
	for i, mt := range mounts {
		place, err := m.resolveTarget(root, mt.GetTarget())
		if err != nil {
			return fmt.Errorf("mount %s: %w", mt.GetTarget(), err)
		}
		if api, ok := inAPIMount(place); ok {
			return fmt.Errorf("mount %s: lands at %s, in %s", mt.GetTarget(), place, api)
		}
		if place == "/" {
			return fmt.Errorf("mount %s: lands at /, the root", mt.GetTarget())
		}
		for _, h := range held {
			if h.place == place || strings.HasPrefix(h.place, place+"/") {
				return fmt.Errorf("mount %s: lands at %s, at or above %s, where %s lands: it would hide it",
					mt.GetTarget(), place, h.place, h.what)
			}
		}
		for j, prev := range places[:i] {
			if prev == place {
				return fmt.Errorf("mount %s: lands at %s, as %s does", mt.GetTarget(), place, mounts[j].GetTarget())
			}
			if rel := nested(place, prev); rel != "" {
				return fmt.Errorf("mount %s: lands at %s, %s %s, where mount %s lands: "+
					"its mount point would be made in the host directory", mt.GetTarget(), place, rel, prev, mounts[j].GetTarget())
			}
		}
		places[i] = place
	}
	for i, mt := range mounts {
		if err := m.mountShare(root, mt.GetTag(), places[i], !mt.GetReadWrite()); err != nil {
			return fmt.Errorf("mount %s: at %s: %w", mt.GetTarget(), places[i], err)
		}
	}
	return nil
}

// nested names how place relates to other, two places where targets land in
// the job's root, when one is strictly under the other: "under" or "above".
// It is "" otherwise. The config refuses such targets as written; only an
// image's symlinks, such as /data -> /srv/x with targets /srv and /data, can
// make two land nested, and then the inner one's mount point would be made
// in the host directory mounted at the outer one (STO-6).
func nested(place, other string) string {
	switch {
	case strings.HasPrefix(place, other+"/"):
		return "under"
	case strings.HasPrefix(other, place+"/"):
		return "above"
	}
	return ""
}

// parentsFirst returns the indexes of places, each where a path lands in
// the job's root, ordered so that a place comes after every place above
// it, and otherwise in the list's order.
func parentsFirst(places []string) []int {
	order := make([]int, 0, len(places))
	placed := make([]bool, len(places))
	// waits reports whether i has an unplaced place above it.
	waits := func(i int) bool {
		for j, u := range places {
			if !placed[j] && strings.HasPrefix(places[i], u+"/") {
				return true
			}
		}
		return false
	}
	for len(order) < len(places) {
		// The first unplaced place with nothing unplaced above it: one
		// always exists, since being above is a strict order.
		for i := range places {
			if !placed[i] && !waits(i) {
				order, placed[i] = append(order, i), true
				break
			}
		}
	}
	return order
}

// checkBootConfig refuses a config whose layer names would leave the layer
// share, that names no scratch device, vsock port or layer tag, whose scratch
// paths, mount targets or guest directories aren't absolute, clean paths
// below /, with a mount that names no tag, or whose guest directory's mode
// isn't permission bits.
func checkBootConfig(cfg *agentv1.BootConfig) error {
	if cfg.GetVsockPort() == 0 {
		return fmt.Errorf("boot config: no vsock port")
	}
	if cfg.GetScratchDevice() == "" {
		return fmt.Errorf("boot config: no scratch device")
	}
	if cfg.GetLayerTag() == "" {
		return fmt.Errorf("boot config: no layer tag")
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
	for _, mt := range cfg.GetMounts() {
		if mt.GetTag() == "" {
			return fmt.Errorf("boot config: mount %s has no tag", mt.GetTarget())
		}
		if !belowRoot(mt.GetTarget()) {
			return fmt.Errorf("boot config: mount target %q is not an absolute, clean path below /", mt.GetTarget())
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
