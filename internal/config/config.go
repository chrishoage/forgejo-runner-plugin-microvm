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
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// DefaultPath is the config file read when PathEnv is unset.
const DefaultPath = "/etc/runner-microvm/config.yaml"

// PathEnv names another config file. It is the plugin's only environment
// variable for an operator setting.
const PathEnv = "RUNNER_MICROVM_CONFIG"

// The plugin-level keys' built-in values (#1, Configuration and profiles).
const (
	builtinListen     = "/run/runner-microvm/runner-microvm.sock"
	builtinStateDir   = "/var/lib/runner-microvm"
	builtinCacheDir   = "/var/cache/runner-microvm"
	builtinRuntimeDir = "/run/runner-microvm/jobs"
)

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
	// Profiles are the profiles by name; never nil.
	Profiles map[string]Profile
}

// Profile is one profile. Each key arrives with the change that gives it
// meaning, so today a profile has none, and any key in one is unknown.
type Profile struct{}

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
	return load(abs, named)
}

// load reads the config file at path, which is absolute. named says whether
// PathEnv named it, so that a missing file is an error.
func load(path string, named bool) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator's config file
	if errors.Is(err, fs.ErrNotExist) && !named {
		if !noFile(path) {
			return nil, fmt.Errorf("config %s: a symlink on the path points to nothing: %w", path, err)
		}
		c := builtin()
		if err := c.checkDirs(fmt.Sprintf("config %s (no file: built-in values): ", path), nil, nil); err != nil {
			return nil, err
		}
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	d := &decoder{prefix: "config " + path + ": ", lines: map[string]int{}, invalid: map[string]bool{}}
	c := builtin()
	c.File = path
	if d.decode(c, data) {
		d.errs = append(d.errs, c.checkDirs(d.prefix, d.lines, d.invalid))
	}
	if err := errors.Join(d.errs...); err != nil {
		return nil, err
	}
	return c, nil
}

// noFile reports whether nothing is at path, which ReadFile found missing:
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
		Listen:     builtinListen,
		StateDir:   builtinStateDir,
		CacheDir:   builtinCacheDir,
		RuntimeDir: builtinRuntimeDir,
		Profiles:   map[string]Profile{},
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
		}
	}
	return true
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
		pv := deref(pp.value)
		switch {
		case isNull(pv):
		case pv.Kind != yaml.MappingNode:
			d.errorf(line, inProfile(name), "", "must be a mapping of profile keys")
			ok = false
		default:
			for _, kp := range d.pairs(pv, inProfile(name), false, nil) {
				d.errorf(kp.key.Line, inProfile(name), kp.key.Value, "unknown key")
				ok = false
			}
		}
		if ok {
			c.Profiles[name] = Profile{}
		}
	}
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
