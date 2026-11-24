// SPDX-License-Identifier: GPL-3.0-or-later

// Package config reads and validates the config file: the plugin-level keys
// and the profiles (docs/architecture.md, Configuration). It only reads: it
// writes nothing to disk, and imports none of the project's packages.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/big"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
	"golang.org/x/sys/unix"
)

// DefaultPath is the config file read when PathEnv is unset.
const DefaultPath = "/etc/runner-microvm/config.yaml"

// PathEnv names another config file. It is the plugin's only environment
// variable for an operator setting.
const PathEnv = "RUNNER_MICROVM_CONFIG"

// The plugin-level keys' built-in values (#1, Configuration and profiles).
const (
	builtinListen      = "/run/runner-microvm/runner-microvm.sock"
	builtinStateDir    = "/var/lib/runner-microvm"
	builtinCacheDir    = "/var/cache/runner-microvm"
	builtinRuntimeDir  = "/run/runner-microvm/jobs"
	builtinCacheMaxGiB = 20
)

// MaxCacheMaxGiB is the largest cache_max_gib: the image cache takes its cap
// as an int64 of bytes, and a larger value's byte count overflows one.
const MaxCacheMaxGiB = math.MaxInt64 >> 30

// Config is a validated config file, with every setting the file leaves out
// at its built-in value.
type Config struct {
	// File is the absolute path of the config file read, or empty when
	// there was none at the default path.
	File string
	// Listen is the listen socket's absolute path: the listen key without
	// its unix:// scheme.
	Listen string
	// StateDir, CacheDir and RuntimeDir are the plugin's directories,
	// absolute and cleaned. They are distinct, and none is inside another.
	StateDir, CacheDir, RuntimeDir string
	// MaxMemoryMiB caps the memory of every running VM; 0 means no limit.
	MaxMemoryMiB uint64
	// CacheMaxGiB is the image cache's size cap, in GiB, at most
	// MaxCacheMaxGiB; 0 is taken literally (IMG-14).
	CacheMaxGiB uint64
	// Profiles are the profiles the file defines, by name; never nil.
	Profiles Profiles

	// path is the absolute path Load resolved, which Reload reads again,
	// and named says whether PathEnv named it.
	path  string
	named bool
}

// CacheMaxBytes is the image cache's size cap in bytes.
func (c *Config) CacheMaxBytes() int64 { return int64(c.CacheMaxGiB) << 30 } //nolint:gosec // G115: CacheMaxGiB is at most MaxCacheMaxGiB, whose byte count fits an int64

// DefaultProfile is the profile a label that names none uses (CFG-4).
const DefaultProfile = "default"

// Profile is one profile: the VM shape a label picks (VM-4, VM-5). Each key
// the file leaves out has its built-in value (CFG-5).
type Profile struct {
	// CPUs is the VM's vCPU count, 1 to MaxCPUs.
	CPUs uint32
	// MemoryMiB is the VM's memory, at least 1.
	MemoryMiB uint32
	// ScratchGiB is the scratch disk's size, at least 1 and at most
	// MaxScratchGiB.
	ScratchGiB uint64
	// ScratchPaths are the guest directories that live on the scratch disk
	// instead of the overlay, each empty at every boot (GST-13): absolute,
	// cleaned, distinct, not / and not under /proc, /sys or /dev. Nil when
	// there are none, the built-in value. Never changed once loaded: a job
	// keeps its profile's list as it was at Create.
	ScratchPaths []string
	// Mounts are the host directories shared into the job's container,
	// read-only, in the file's order (STO-1, STO-2). Nil when there are
	// none, the built-in value. Never changed once loaded, like
	// ScratchPaths.
	Mounts []Mount
	// ForcePull looks the job's tag up in the registry at every Start,
	// even when the image cache holds it (IMG-6). False by default.
	ForcePull bool
}

// The profile keys' built-in values (#1, Configuration and profiles).
const (
	builtinCPUs       = 2
	builtinMemoryMiB  = 1024
	builtinScratchGiB = 10
)

// The profile keys' bounds: only those a larger value can't work past
// (CFG-10). libkrun takes the vCPU count in one byte and the memory as a
// uint32 of MiB, and the job process makes the scratch file with an int64
// of bytes.
const (
	MaxCPUs       = 255
	MaxMemoryMiB  = math.MaxUint32
	MaxScratchGiB = math.MaxInt64 >> 30
)

// BuiltinProfile returns the profile with every built-in value: the implicit
// default profile, and what each profile starts from.
func BuiltinProfile() Profile {
	return Profile{CPUs: builtinCPUs, MemoryMiB: builtinMemoryMiB, ScratchGiB: builtinScratchGiB}
}

// ScratchBytes is the scratch disk's size in bytes.
func (p Profile) ScratchBytes() uint64 { return p.ScratchGiB << 30 }

// Profiles are profiles by name.
type Profiles map[string]Profile

// Lookup returns the profile name names. DefaultProfile always exists: when
// the file defines none, it is BuiltinProfile (CFG-4).
func (ps Profiles) Lookup(name string) (Profile, bool) {
	if p, ok := ps[name]; ok {
		return p, true
	}
	if name == DefaultProfile {
		return BuiltinProfile(), true
	}
	return Profile{}, false
}

// profileName is the rule for a profile's name: one a label can carry.
var profileName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Load reads and validates the config file: the file PathEnv names, made
// absolute once, or else DefaultPath. No file at DefaultPath gives every
// built-in value; a missing file that PathEnv names is an error. Every
// error is reported, each naming the file, the line and the key, and inside
// profiles the profile; they are joined, one per line.
func Load(getenv func(string) string) (*Config, error) {
	path, named := getenv(PathEnv), true
	if path == "" {
		path, named = DefaultPath, false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("%s=%s: %w", PathEnv, path, err)
	}
	return load(abs, named, nil)
}

// Change is a plugin-level key whose value in the config file differs, at a
// reload, from the value the plugin runs with. Running and File are its two
// values, as the file would write them.
type Change struct {
	Key, Running, File string
}

// Reload reads the config file again for a plugin running with c: the same
// absolute path Load resolved, under the same rules, without reading PathEnv
// again. The whole file is validated, and its profiles are checked against
// c's plugin-level values, which a reload never changes (CFG-7, CFG-8). It
// returns a new config: c with the file's profiles in place of c's, and the
// plugin-level keys whose value in the file is not c's, which keep c's value
// until a restart. On any error it returns only the error, every one as
// Load reports them, and c is left as it is.
func (c *Config) Reload() (*Config, []Change, error) {
	f, err := load(c.path, c.named, c)
	if err != nil {
		return nil, nil, err
	}
	var changes []Change
	for _, k := range []struct {
		key           string
		running, file string
	}{
		{"listen", "unix://" + c.Listen, "unix://" + f.Listen},
		{"state_dir", c.StateDir, f.StateDir},
		{"cache_dir", c.CacheDir, f.CacheDir},
		{"runtime_dir", c.RuntimeDir, f.RuntimeDir},
		{"max_memory_mib", strconv.FormatUint(c.MaxMemoryMiB, 10), strconv.FormatUint(f.MaxMemoryMiB, 10)},
		{"cache_max_gib", strconv.FormatUint(c.CacheMaxGiB, 10), strconv.FormatUint(f.CacheMaxGiB, 10)},
	} {
		if k.running != k.file {
			changes = append(changes, Change{Key: k.key, Running: k.running, File: k.file})
		}
	}
	next := *c
	next.File, next.Profiles = f.File, f.Profiles
	return &next, changes, nil
}

// load reads the config file at path, which is absolute. named says whether
// PathEnv named it, so that a missing file is an error. running is the
// config a reload replaces, or nil at startup: a reload checks the file's
// profiles against running's plugin-level values, which stay in effect.
func load(path string, named bool, running *Config) (*Config, error) {
	return loadOn(path, named, running, runtime.GOARCH)
}

// loadOn is load for a plugin running on arch, a GOARCH.
func loadOn(path string, named bool, running *Config, arch string) (*Config, error) {
	data, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) && !named {
		if !noFile(path) {
			return nil, fmt.Errorf("config %s: a symlink on the path points to nothing: %w", path, err)
		}
		c := builtin()
		c.path = path
		if err := c.checkDirs(fmt.Sprintf("config %s (no file: built-in values): ", path), nil, nil); err != nil {
			return nil, err
		}
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	d := &decoder{prefix: "config " + path + ": ", lines: map[string]int{}, invalid: map[string]bool{}, running: running, arch: arch}
	c := builtin()
	c.File, c.path, c.named = path, path, named
	if d.decode(c, data) {
		d.errs = append(d.errs, c.checkDirs(d.prefix, d.lines, d.invalid))
	}
	if err := errors.Join(d.errs...); err != nil {
		return nil, err
	}
	return c, nil
}

// readFile reads the config file at path, which must be a regular file. It
// opens it without blocking and refuses anything else before reading, so a
// FIFO with no writer, or a device, can't hang startup or a reload: that
// would leave a reload nobody can stop, and serve waits for it as it exits.
// A stalled network mount can still block the read.
func readFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY, 0) //nolint:gosec // G304: the operator's config file
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (%s)", fi.Mode().Type())
	}
	return io.ReadAll(f)
}

// noFile reports whether nothing is at path, which readFile found missing:
// the deepest part of it that exists is a directory, or a symlink to one. A
// symlink whose target is gone, at path or on the way to it, is a file that
// can't be read, not a missing one.
func noFile(path string) bool {
	for p := path; ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err == nil {
			if fi.Mode()&fs.ModeSymlink == 0 {
				return true
			}
			_, err = os.Stat(p)
			return err == nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false
		}
		if p == "/" {
			return true
		}
	}
}

// builtin returns the config with every built-in value.
func builtin() *Config {
	return &Config{
		Listen:      builtinListen,
		StateDir:    builtinStateDir,
		CacheDir:    builtinCacheDir,
		RuntimeDir:  builtinRuntimeDir,
		CacheMaxGiB: builtinCacheMaxGiB,
		Profiles:    Profiles{},
	}
}

// decoder decodes one config file from its YAML nodes, so every error,
// decoding and validation alike, carries the line of the key at fault.
type decoder struct {
	// prefix starts every error: "config <path>: ".
	prefix string
	errs   []error
	// lines holds the line of each plugin-level key the file sets, and
	// invalid each one whose value failed to decode.
	lines   map[string]int
	invalid map[string]bool
	// merged counts the key-value pairs merge keys have expanded, against
	// maxMerged.
	merged int
	// memLines holds, for each profile decoded, the line its memory_mib is
	// set on, or else its name's line, in the file's order, for the check
	// against max_memory_mib.
	memLines []profileLine
	// running is the config a reload replaces, or nil at startup.
	running *Config
	// arch is the GOARCH the plugin runs on, for the mounts-plus-disks
	// limit (CFG-10).
	arch string
}

// profileLine is a line in the profile name.
type profileLine struct {
	name string
	line int
}

// maxMerged bounds what merge keys may expand to, so a file that nests
// merges can't make decoding exponential.
const maxMerged = 100000

// errorf records an error at line, in scope when it is not empty (a
// profile, worded by inProfile), for key when it is not empty.
func (d *decoder) errorf(line int, scope, key, format string, args ...any) {
	var b strings.Builder
	b.WriteString(d.prefix)
	if line > 0 {
		fmt.Fprintf(&b, "line %d: ", line)
	}
	if scope != "" {
		b.WriteString(scope + ": ")
	}
	if key != "" {
		b.WriteString(key + ": ")
	}
	fmt.Fprintf(&b, format, args...)
	d.errs = append(d.errs, errors.New(b.String()))
}

// decode decodes data into c, which holds the built-in values, recording
// its errors. It reports whether the file parsed as YAML, so that the keys
// it decoded can be checked further.
func (d *decoder) decode(c *Config, data []byte) bool {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
		return true // an empty file, or only comments
	} else if err != nil {
		d.errs = append(d.errs, fmt.Errorf("%s%w", d.prefix, err))
		return false
	}
	var next yaml.Node
	if err := dec.Decode(&next); err == nil {
		d.errorf(next.Line, "", "", "a second YAML document: the config file holds one")
	} else if !errors.Is(err, io.EOF) {
		d.errs = append(d.errs, fmt.Errorf("%s%w", d.prefix, err))
		return false
	}
	if len(doc.Content) == 1 {
		root := deref(doc.Content[0])
		switch {
		case isNull(root):
		case root.Kind != yaml.MappingNode:
			d.errorf(root.Line, "", "", "must be a mapping of keys")
		default:
			d.top(c, root)
			d.checkMemory(c)
		}
	}
	return true
}

// checkMemory fails each profile the file defines whose memory_mib is more
// than a max_memory_mib that is set and valid: no job on it could ever run.
// The implicit default profile isn't checked: the file doesn't define it.
// Nothing refuses a job on it yet; #10's admission check will, at Create
// (Admission, docs/architecture.md).
//
// At a reload, the limit is the running plugin's: a max_memory_mib the file
// changes applies only at a restart.
func (d *decoder) checkMemory(c *Config) {
	if d.running != nil {
		limit := d.running.MaxMemoryMiB
		for _, pl := range d.memLines {
			if mem := uint64(c.Profiles[pl.name].MemoryMiB); limit != 0 && mem > limit {
				d.errorf(pl.line, inProfile(pl.name), "memory_mib", "%d is more than max_memory_mib in effect, %d: no job on this profile could ever run",
					mem, limit)
			}
		}
		return
	}
	maxLine, ok := d.lines["max_memory_mib"]
	if !ok || c.MaxMemoryMiB == 0 {
		return
	}
	for _, pl := range d.memLines {
		if mem := uint64(c.Profiles[pl.name].MemoryMiB); mem > c.MaxMemoryMiB {
			d.errorf(pl.line, inProfile(pl.name), "memory_mib", "%d is more than max_memory_mib, %d (line %d): no job on this profile could ever run",
				mem, c.MaxMemoryMiB, maxLine)
		}
	}
}

// top decodes the top-level mapping.
func (d *decoder) top(c *Config, root *yaml.Node) {
	for _, p := range d.pairs(root, "", false, nil) {
		key, line := p.key.Value, p.key.Line
		var ok bool
		switch key {
		case "listen":
			var s string
			if s, ok = d.str(p, ""); ok {
				if path, valid := strings.CutPrefix(s, "unix://"); valid && filepath.IsAbs(path) && filepath.Clean(path) != "/" {
					c.Listen = filepath.Clean(path)
				} else {
					d.errorf(line, "", key, "must be unix:// followed by an absolute path, not %q", s)
					ok = false
				}
			}
		case "state_dir":
			ok = d.absPath(p, &c.StateDir)
		case "cache_dir":
			ok = d.absPath(p, &c.CacheDir)
		case "runtime_dir":
			ok = d.absPath(p, &c.RuntimeDir)
		case "max_memory_mib":
			ok = d.uint(p, &c.MaxMemoryMiB)
		case "cache_max_gib":
			c.CacheMaxGiB, ok = d.integer(p, "", 0, MaxCacheMaxGiB)
		case "profiles":
			d.profiles(c, p)
		default:
			d.errorf(line, "", key, "unknown key")
		}
		if ok {
			d.lines[key] = line
		} else {
			d.invalid[key] = true
		}
	}
}

// profiles decodes the profiles mapping. Each profile's errors name it.
func (d *decoder) profiles(c *Config, p pair) {
	v := deref(p.value)
	if isNull(v) {
		return
	}
	if v.Kind != yaml.MappingNode {
		d.errorf(p.key.Line, "", p.key.Value, "must be a mapping of profile names to profiles")
		return
	}
	for _, pp := range d.pairs(v, "", true, nil) {
		name, line := pp.key.Value, pp.key.Line
		ok := true
		if !profileName.MatchString(name) {
			d.errorf(line, inProfile(name), "", "the name must match %s", strings.Trim(profileName.String(), "^$"))
			ok = false
		}
		prof, memLine := BuiltinProfile(), line
		pv := deref(pp.value)
		switch {
		case isNull(pv):
		case pv.Kind != yaml.MappingNode:
			d.errorf(line, inProfile(name), "", "must be a mapping of profile keys")
			ok = false
		default:
			ok = d.profile(name, pv, &prof, &memLine) && ok
		}
		if ok {
			c.Profiles[name] = prof
			d.memLines = append(d.memLines, profileLine{name, memLine})
		}
	}
}

// profile decodes the keys of the profile name, from m, into prof, which
// holds the built-in values, and sets *memLine to memory_mib's line. It
// reports whether every key was valid.
func (d *decoder) profile(name string, m *yaml.Node, prof *Profile, memLine *int) bool {
	scope, ok := inProfile(name), true
	// The guest paths the profile sets, and how many mounts and disks it
	// has, for the checks across its keys (checkTargets).
	var scratch, targets []guestPath
	count, listLine, listKey := 0, 0, ""
	for _, kp := range d.pairs(m, scope, false, nil) {
		switch kp.key.Value {
		case "cpus":
			n, valid := d.integer(kp, scope, 1, MaxCPUs)
			prof.CPUs, ok = uint32(n), ok && valid //nolint:gosec // G115: integer bounds n by MaxCPUs
		case "memory_mib":
			n, valid := d.integer(kp, scope, 1, MaxMemoryMiB)
			prof.MemoryMiB, ok = uint32(n), ok && valid //nolint:gosec // G115: integer bounds n by MaxMemoryMiB, a uint32
			*memLine = kp.key.Line
		case "scratch_gib":
			n, valid := d.integer(kp, scope, 1, MaxScratchGiB)
			prof.ScratchGiB, ok = n, ok && valid
		case "scratch_paths":
			paths, claims, valid := d.scratchPaths(kp, scope)
			prof.ScratchPaths, scratch, ok = paths, claims, ok && valid
		case "mounts":
			ms, claims, n, valid := d.mounts(kp, scope)
			prof.Mounts, ok = ms, ok && valid
			targets, count = append(targets, claims...), count+n
			listLine, listKey = kp.key.Line, kp.key.Value
		case "force_pull":
			b, valid := d.boolean(kp, scope)
			prof.ForcePull, ok = b, ok && valid
		default:
			d.errorf(kp.key.Line, scope, kp.key.Value, "unknown key")
			ok = false
		}
	}
	return d.checkTargets(scope, scratch, targets, count, d.arch, listKey, listLine) && ok
}

// apiDirs are the guest directories the agent's own mounts hold, under
// which no scratch path may be: STO-5's target rule, which mounts and disks
// share.
var apiDirs = []string{"/proc", "/sys", "/dev"}

// scratchPaths returns p's value, in scope, a list of guest paths, each
// cleaned, nil for an empty list, and each valid path with its line. Every
// invalid path is reported, at its own line.
func (d *decoder) scratchPaths(p pair, scope string) ([]string, []guestPath, bool) {
	v := deref(p.value)
	switch {
	case isNull(v):
		d.errorf(p.key.Line, scope, p.key.Value, "has no value")
		return nil, nil, false
	case v.Kind != yaml.SequenceNode:
		d.errorf(p.key.Line, scope, p.key.Value, "must be a list of paths")
		return nil, nil, false
	}
	var paths []string
	var claims []guestPath
	seen := map[string]int{}
	ok := true
	for _, n := range v.Content {
		n = deref(n)
		if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!str" {
			if n.Kind == yaml.ScalarNode && !isNull(n) {
				d.errorf(n.Line, scope, p.key.Value, "each path must be a string, not %q", n.Value)
			} else {
				d.errorf(n.Line, scope, p.key.Value, "each path must be a string")
			}
			ok = false
			continue
		}
		clean, problem := targetPath(n.Value)
		if problem == "" {
			if first, dup := seen[clean]; dup {
				problem = fmt.Sprintf("is already listed on line %d", first)
			}
		}
		if problem != "" {
			d.errorf(n.Line, scope, p.key.Value, "%q %s", n.Value, problem)
			ok = false
			continue
		}
		seen[clean] = n.Line
		paths = append(paths, clean)
		claims = append(claims, guestPath{key: p.key.Value, raw: n.Value, clean: clean, line: n.Line})
	}
	return paths, claims, ok
}

// targetPath returns the guest path s cleaned, or what is wrong with it as a
// scratch path or a mount's or a disk's target: STO-5's rules, which all
// three share.
func targetPath(s string) (string, string) {
	if strings.ContainsRune(s, 0) {
		return "", "holds a NUL byte"
	}
	if !path.IsAbs(s) {
		return "", "is not an absolute path"
	}
	clean := path.Clean(s)
	if clean == "/" {
		return "", "is the root"
	}
	for _, dir := range apiDirs {
		if clean == dir || strings.HasPrefix(clean, dir+"/") {
			return "", "is under " + dir
		}
	}
	return clean, ""
}

// inProfile is the scope of an error inside the profile name.
func inProfile(name string) string { return fmt.Sprintf("profile %q", name) }

// pair is one key and its value in a mapping.
type pair struct{ key, value *yaml.Node }

// pairs returns m's pairs with merge keys (<<) expanded: m's own keys first,
// in order, then each merged mapping's keys that m doesn't already have, in
// the order the merge key lists them. A key m sets twice, or a key that
// isn't a scalar, is an error, and is dropped; names says m's keys are
// profile names, for the error's wording. merging holds the mappings
// being merged into, so a mapping that merges itself fails rather than
// recursing forever.
func (d *decoder) pairs(m *yaml.Node, scope string, names bool, merging []*yaml.Node) []pair {
	if d.merged > maxMerged {
		return nil
	}
	merging = append(merging, m)
	var own []pair
	var merges []*yaml.Node
	seen := map[string]int{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			d.errorf(k.Line, scope, "", "a key must be a scalar")
			continue
		}
		if k.ShortTag() == "!!merge" {
			merges = append(merges, k, v)
			continue
		}
		if first, dup := seen[k.Value]; dup {
			if names {
				d.errorf(k.Line, inProfile(k.Value), "", "is already defined on line %d", first)
			} else {
				d.errorf(k.Line, scope, k.Value, "is already set on line %d", first)
			}
			continue
		}
		seen[k.Value] = k.Line
		own = append(own, pair{k, v})
	}
	for i := 0; i < len(merges); i += 2 {
		k, v := merges[i], deref(merges[i+1])
		var sources []*yaml.Node
		switch v.Kind {
		case yaml.MappingNode:
			sources = []*yaml.Node{v}
		case yaml.SequenceNode:
			for _, s := range v.Content {
				sources = append(sources, deref(s))
			}
		case yaml.DocumentNode, yaml.ScalarNode, yaml.AliasNode:
			// Not a mapping: sources stays empty.
		}
		if len(sources) == 0 {
			d.errorf(k.Line, scope, k.Value, "must be a mapping, an alias of one, or a list of them")
			continue
		}
		for _, s := range sources {
			if s.Kind != yaml.MappingNode {
				d.errorf(k.Line, scope, k.Value, "must be a mapping, an alias of one, or a list of them")
				continue
			}
			if containsNode(merging, s) {
				d.errorf(k.Line, scope, k.Value, "merges a mapping into itself")
				continue
			}
			for _, mp := range d.pairs(s, scope, names, merging) {
				if d.merged++; d.merged > maxMerged {
					if d.merged == maxMerged+1 {
						d.errorf(k.Line, scope, k.Value, "merge keys expand to more than %d keys", maxMerged)
					}
					return own
				}
				if _, have := seen[mp.key.Value]; !have {
					seen[mp.key.Value] = mp.key.Line
					own = append(own, mp)
				}
			}
		}
	}
	return own
}

func containsNode(ns []*yaml.Node, n *yaml.Node) bool {
	for _, x := range ns {
		if x == n {
			return true
		}
	}
	return false
}

// deref follows aliases to the node they name.
func deref(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

func isNull(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null" }

// str returns p's value as a string.
func (d *decoder) str(p pair, scope string) (string, bool) {
	v := deref(p.value)
	switch {
	case isNull(v):
		d.errorf(p.key.Line, scope, p.key.Value, "has no value")
	case v.Kind != yaml.ScalarNode || v.ShortTag() != "!!str":
		d.errorf(p.key.Line, scope, p.key.Value, "must be a string")
	default:
		return v.Value, true
	}
	return "", false
}

// absPath sets *dst to p's value, which must be an absolute path, cleaned.
func (d *decoder) absPath(p pair, dst *string) bool {
	s, ok := d.str(p, "")
	if !ok {
		return false
	}
	if !filepath.IsAbs(s) {
		d.errorf(p.key.Line, "", p.key.Value, "must be an absolute path, not %q", s)
		return false
	}
	*dst = filepath.Clean(s)
	return true
}

// integer returns p's value, in scope, an integer from lo to hi. 0 is no
// different from any other value out of range: a key left out is how a
// profile takes a built-in value.
func (d *decoder) integer(p pair, scope string, lo, hi uint64) (uint64, bool) {
	v := deref(p.value)
	if isNull(v) {
		d.errorf(p.key.Line, scope, p.key.Value, "has no value")
		return 0, false
	}
	if v.Kind != yaml.ScalarNode {
		d.errorf(p.key.Line, scope, p.key.Value, "must be an integer")
		return 0, false
	}
	if v.ShortTag() != "!!int" {
		// YAML resolves an integer too large for 64 bits as a float.
		if _, isInt := new(big.Int).SetString(v.Value, 0); !isInt || v.Style != 0 {
			d.errorf(p.key.Line, scope, p.key.Value, "must be an integer, not %q", v.Value)
			return 0, false
		}
	}
	// An integer too large for an int64 may still fit a uint64; one that
	// fits neither is out of range, whatever its sign.
	var n int64
	var u uint64
	if v.Decode(&n) == nil {
		if n >= 0 && uint64(n) >= lo && uint64(n) <= hi {
			return uint64(n), true
		}
	} else if v.Decode(&u) == nil && u >= lo && u <= hi {
		return u, true
	}
	d.errorf(p.key.Line, scope, p.key.Value, "must be from %d to %d, not %s", lo, hi, v.Value)
	return 0, false
}

// boolean returns p's value, in scope, true or false. Only YAML's plain
// booleans count: a quoted one is a string, and YAML 1.1's yes and on are
// strings in YAML 1.2.
func (d *decoder) boolean(p pair, scope string) (bool, bool) {
	v := deref(p.value)
	var b bool
	switch {
	case isNull(v):
		d.errorf(p.key.Line, scope, p.key.Value, "has no value")
	case v.Kind != yaml.ScalarNode:
		d.errorf(p.key.Line, scope, p.key.Value, "must be true or false")
	case v.ShortTag() != "!!bool" || v.Decode(&b) != nil:
		d.errorf(p.key.Line, scope, p.key.Value, "must be true or false, not %q", v.Value)
	default:
		return b, true
	}
	return false, false
}

// uint sets *dst to p's value, an integer of 0 or more.
func (d *decoder) uint(p pair, dst *uint64) bool {
	v := deref(p.value)
	var n int64
	switch {
	case isNull(v):
		d.errorf(p.key.Line, "", p.key.Value, "has no value")
		return false
	case v.Kind != yaml.ScalarNode || v.ShortTag() != "!!int" || v.Decode(&n) != nil:
		d.errorf(p.key.Line, "", p.key.Value, "must be an integer, not %q", v.Value)
		return false
	case n < 0:
		d.errorf(p.key.Line, "", p.key.Value, "must be 0 or more, not %d", n)
		return false
	}
	*dst = uint64(n)
	return true
}
