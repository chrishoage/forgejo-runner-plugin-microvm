// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Mount is one of a profile's mounts: a host directory shared into the
// job's container at a guest path (STO-1).
type Mount struct {
	// Source is the host directory: absolute, cleaned, and a directory
	// when the file was read.
	Source string
	// Target is the guest path: absolute, cleaned, not / and not under
	// /proc, /sys or /dev (STO-5).
	Target string
	// ReadWrite lets the guest write the mount. Always false today: a
	// read-write mount stops startup until #89.
	ReadWrite bool
}

// amd64Storage is how many mounts plus disks a VM can have on amd64
// (CFG-10). libkrun 1.19.8 gives x86_64 VMs 11 virtio-mmio interrupt lines,
// IRQs 5 to 15, and every VM already uses 8 of them:
//
//  1. the console;
//  2. the balloon;
//  3. the rng;
//  4. the root share (the boot root);
//  5. the layer share;
//  6. the scratch disk;
//  7. the network;
//  8. vsock.
//
// A device removed from that list raises it by one. libkrun's own failure
// past the limit is a bare -EINVAL from krun_start_enter, its reason only in
// its log, so the config refuses it first.
const amd64Storage = 11 - 8

// The guest directories the agent makes in every job's container, Create's
// act path, tool cache and temp directory (PROTO-7). internal/plugin returns
// them from Create and sends them in the boot config; the config refuses a
// mount or disk target at or above one, which would hide it.
const (
	ActPath       = "/var/run/act"
	ToolCachePath = "/opt/hostedtoolcache"
	TempPath      = "/tmp"
)

// guestDirs names each of the guest's own directories, for the target
// checks' errors.
var guestDirs = []struct{ name, path string }{
	{"act path", ActPath},
	{"tool cache", ToolCachePath},
	{"temp directory", TempPath},
}

// storageLimit is the most mounts plus disks a profile may have on arch, a
// GOARCH, or 0 for no limit. arm64's 128 interrupt lines are far beyond any
// profile, and arm64 isn't supported.
func storageLimit(arch string) int {
	if arch == "amd64" {
		return amd64Storage
	}
	return 0
}

// guestPath is one guest path a profile key sets, with its line, for the
// checks across scratch_paths, mounts and disks.
type guestPath struct {
	// key is the profile key that sets it; raw is the path as written,
	// and clean cleaned.
	key, raw, clean string
	line            int
}

// storageEntry is one entry of a profile's mounts or disks, as written.
type storageEntry struct {
	source, target string
	readWrite      bool
	// rwLine is read_write's line, or 0 when it isn't set.
	rwLine int
	// claim is the target, for the checks across the lists.
	claim guestPath
}

// storageList decodes p's value, in the profile scope, a list of mounts or
// disks: each entry a mapping of source, target and read_write, decoded
// strictly, its errors in scope and the list's key. source checks a source
// that is an absolute, clean path, and returns what is wrong with it. It
// returns the valid entries and how many entries the list has, and reports
// whether every entry was valid. Every invalid entry is reported, at its own
// line.
func (d *decoder) storageList(p pair, scope string, source func(string) string) ([]storageEntry, int, bool) {
	v := deref(p.value)
	switch {
	case isNull(v):
		d.errorf(p.key.Line, scope, p.key.Value, "has no value")
		return nil, 0, false
	case v.Kind != yaml.SequenceNode:
		d.errorf(p.key.Line, scope, p.key.Value, "must be a list of entries")
		return nil, 0, false
	}
	inList := scope + ": " + p.key.Value
	var entries []storageEntry
	ok := true
	for _, n := range v.Content {
		n = deref(n)
		if n.Kind != yaml.MappingNode {
			d.errorf(n.Line, inList, "", "each entry must be a mapping of source, target and read_write")
			ok = false
			continue
		}
		e, valid := d.storageEntry(n, p.key.Value, inList, source)
		if valid {
			entries = append(entries, e)
		}
		ok = ok && valid
	}
	return entries, len(v.Content), ok
}

// storageEntry decodes the entry m of the list key, in scope.
func (d *decoder) storageEntry(m *yaml.Node, key, scope string, source func(string) string) (storageEntry, bool) {
	var e storageEntry
	ok, haveSource, haveTarget := true, false, false
	for _, kp := range d.pairs(m, scope, false, nil) {
		switch kp.key.Value {
		case "source":
			s, valid := d.str(kp, scope)
			if valid {
				if problem := hostPath(s, source); problem != "" {
					d.errorf(kp.key.Line, scope, kp.key.Value, "%q %s", s, problem)
					valid = false
				}
			}
			e.source, haveSource, ok = filepath.Clean(s), true, ok && valid
		case "target":
			s, valid := d.str(kp, scope)
			if valid {
				clean, problem := targetPath(s)
				if problem != "" {
					d.errorf(kp.key.Line, scope, kp.key.Value, "%q %s", s, problem)
					valid = false
				}
				e.target, e.claim = clean, guestPath{key: key, raw: s, clean: clean, line: kp.key.Line}
			}
			haveTarget, ok = true, ok && valid
		case "read_write":
			b, valid := d.boolean(kp, scope)
			e.readWrite, e.rwLine, ok = b, kp.key.Line, ok && valid
		default:
			d.errorf(kp.key.Line, scope, kp.key.Value, "unknown key")
			ok = false
		}
	}
	for _, missing := range []struct {
		have bool
		key  string
	}{{haveSource, "source"}, {haveTarget, "target"}} {
		if !missing.have {
			d.errorf(m.Line, scope, "", "an entry needs a %s", missing.key)
			ok = false
		}
	}
	return e, ok
}

// mounts decodes p's value, in the profile scope, a profile's mounts
// (STO-1), and returns them, their targets and their count. Each source must
// be a directory (STO-5).
func (d *decoder) mounts(p pair, scope string) ([]Mount, []guestPath, int, bool) {
	entries, count, ok := d.storageList(p, scope, dirSource)
	var ms []Mount
	var targets []guestPath
	for _, e := range entries {
		if e.readWrite {
			// SHORTCUT(#89): read-write mounts and their ownership rules
			// aren't built; refusing the key keeps a profile from silently
			// getting a read-only mount it asked to write.
			d.errorf(e.rwLine, scope+": "+p.key.Value, "read_write", "read-write mounts aren't supported yet")
			ok = false
			continue
		}
		ms = append(ms, Mount{Source: e.source, Target: e.target, ReadWrite: e.readWrite})
		targets = append(targets, e.claim)
	}
	return ms, targets, count, ok
}

// hostPath returns what is wrong with s as a source, or "": it must be an
// absolute path, and check, given it cleaned, finds nothing wrong with it.
func hostPath(s string, check func(string) string) string {
	if strings.ContainsRune(s, 0) {
		return "holds a NUL byte"
	}
	if !filepath.IsAbs(s) {
		return "is not an absolute path"
	}
	return check(filepath.Clean(s))
}

// dirSource returns what is wrong with p as a mount's source, or "": it
// must be a directory, or a symlink to one.
func dirSource(p string) string {
	fi, err := os.Stat(p)
	var pe *fs.PathError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "does not exist"
	case errors.As(err, &pe):
		return pe.Err.Error()
	case err != nil:
		return err.Error()
	case !fi.IsDir():
		return "is not a directory"
	}
	return ""
}

// checkTargets checks, in scope, the targets of a profile's mounts and
// disks against each other, against its scratch paths and against the
// guest's own directories (STO-5, STO-6), and their count against the limit
// on arch (CFG-10), whose error is at listLine, the line of the list that
// holds them, named key. A target may be used only once, and may not be
// under another target; it may not be one of the scratch paths or the
// guest's directories, nor above one, since the mount, made after them,
// would hide it. One under a scratch path or a guest directory is fine. It
// reports whether they pass.
func (d *decoder) checkTargets(scope string, scratch, targets []guestPath, count int, arch, key string, listLine int) bool {
	ok := true
	for i, t := range targets {
		in := scope + ": " + t.key
		for _, s := range scratch {
			switch {
			case t.clean == s.clean:
				d.errorf(t.line, in, "target", "%q is the scratch path on line %d: one would hide the other", t.raw, s.line)
				ok = false
			case within(s.clean, t.clean):
				d.errorf(t.line, in, "target", "%q is above the scratch path %q on line %d: it would hide it", t.raw, s.clean, s.line)
				ok = false
			}
		}
		for _, g := range guestDirs {
			switch {
			case t.clean == g.path:
				d.errorf(t.line, in, "target", "%q is the guest's %s, %s: it would hide it", t.raw, g.name, g.path)
				ok = false
			case within(g.path, t.clean):
				d.errorf(t.line, in, "target", "%q is above the guest's %s, %s: it would hide it", t.raw, g.name, g.path)
				ok = false
			}
		}
		for j, u := range targets {
			switch {
			case j < i && t.clean == u.clean:
				d.errorf(t.line, in, "target", "%q is already a target on line %d", t.raw, u.line)
				ok = false
			case strings.HasPrefix(t.clean, u.clean+"/"):
				d.errorf(t.line, in, "target", "%q is under the target %q on line %d", t.raw, u.clean, u.line)
				ok = false
			}
		}
	}
	if limit := storageLimit(arch); limit > 0 && count > limit {
		d.errorf(listLine, scope, key, "%d mounts and disks, more than %d, the most a VM can have on %s: "+
			"libkrun's x86_64 VMs have 11 virtio-mmio interrupt lines, and every VM already uses 8", count, limit, arch)
		ok = false
	}
	return ok
}
