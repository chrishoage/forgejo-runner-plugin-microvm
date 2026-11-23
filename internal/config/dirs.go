// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// dirKey is one of the directories checkDirs compares.
type dirKey struct {
	// key is the config key; subject and object word it as an error's
	// subject and object.
	key, subject, object string
	// path is the configured path; resolved is it with symlinks resolved.
	path, resolved string
	// line is the key's line, or 0 for a built-in value.
	line int
}

// where says where k's value comes from.
func (k dirKey) where() string {
	if k.line == 0 {
		return "built-in value " + k.path
	}
	return fmt.Sprintf("line %d", k.line)
}

// lockFile is the name of the lock file serve takes in each of the plugin's
// directories (internal/plugin, which this package doesn't import).
const lockFile = "lock"

// checkDirs checks the rule for the plugin's directories (docs/architecture.md,
// Failure handling and startup cleanup), comparing absolute paths with
// symlinks resolved; it only reads. The state, cache and runtime directories
// must be distinct, and none may be inside another. The listen socket's
// directory may be the runtime directory or its direct parent, as the
// built-in values have it; otherwise it must be distinct from the three and
// neither inside nor holding one. No directory, and not the socket either,
// may be a locked directory's lock file, nor may the socket be one of the
// directories. Each error names both keys. lines holds the line of each key the file sets; the keys in skip
// failed to decode, so they aren't checked.
func (c *Config) checkDirs(prefix string, lines map[string]int, skip map[string]bool) error {
	d := &decoder{prefix: prefix}
	var keys []*dirKey
	var runtime, listen *dirKey
	for _, k := range []*dirKey{
		{key: "state_dir", path: c.StateDir},
		{key: "cache_dir", path: c.CacheDir},
		{key: "runtime_dir", path: c.RuntimeDir},
		{key: "listen", path: filepath.Dir(c.Listen)},
	} {
		if skip[k.key] {
			continue
		}
		k.subject, k.object, k.line = k.key+": ", k.key, lines[k.key]
		switch k.key {
		case "listen":
			k.subject, k.object = "listen: the socket's directory ", "the directory of listen"
			listen = k
		case "runtime_dir":
			runtime = k
		}
		resolved, err := resolve(k.path)
		if err != nil {
			d.errorf(k.line, "", k.key, "resolve symlinks in %s: %v", k.path, err)
			continue
		}
		k.resolved = resolved
		keys = append(keys, k)
	}
	if len(d.errs) > 0 {
		return errors.Join(d.errs...)
	}
	for j := 1; j < len(keys); j++ {
		for i := range j {
			a, b := keys[i], keys[j]
			if d.checkLockFile(a, b) || d.checkLockFile(b, a) {
				continue
			}
			if b == listen && a == runtime && (a.resolved == b.resolved || filepath.Dir(a.resolved) == b.resolved) {
				continue
			}
			d.compare(a, b)
		}
	}
	if listen != nil {
		d.checkSocket(c.Listen, listen, keys)
	}
	return errors.Join(d.errs...)
}

// checkLockFile records an error, and reports true, when x is the lock file
// serve takes in y. The pair needs that check of its own: the listen
// socket's directory may be the runtime directory's direct parent, so
// compare skips that pair, and serve would then fail to open the lock file
// that is a directory.
func (d *decoder) checkLockFile(x, y *dirKey) bool {
	if x.resolved != filepath.Join(y.resolved, lockFile) {
		return false
	}
	d.errs = append(d.errs, fmt.Errorf("%s%sis the lock file of %s (%s), %s", d.lineprefix(x.line), x.subject, y.object, y.where(), x.resolved))
	return true
}

// checkSocket records an error when the socket is one of keys' directories
// or the lock file in one.
func (d *decoder) checkSocket(socket string, listen *dirKey, keys []*dirKey) {
	sock, err := resolve(socket)
	if err != nil {
		// The socket's directory resolved, so only a symlink at the
		// socket's own path can fail here.
		d.errorf(listen.line, "", "listen", "resolve symlinks in %s: %v", socket, err)
		return
	}
	for _, k := range keys {
		switch {
		case k != listen && sock == k.resolved:
			d.errorf(listen.line, "", "listen", "the socket is %s (%s), %s", k.key, k.where(), k.resolved)
		case sock == filepath.Join(k.resolved, lockFile) && k == listen:
			d.errorf(listen.line, "", "listen", "the socket is the lock file of its own directory, %s", sock)
		case sock == filepath.Join(k.resolved, lockFile):
			d.errorf(listen.line, "", "listen", "the socket is the lock file of %s (%s), %s", k.key, k.where(), sock)
		default:
			continue
		}
		return
	}
}

// compare records an error when a and b are the same directory or one is
// inside the other. The error is b's unless only a is set in the file.
func (d *decoder) compare(a, b *dirKey) {
	var rel string
	switch {
	case a.resolved == b.resolved:
		rel = "is the same directory as"
	case within(b.resolved, a.resolved):
		rel = "is inside"
	case within(a.resolved, b.resolved):
		rel = "contains"
	default:
		return
	}
	if b.line == 0 && a.line != 0 {
		a, b = b, a
		switch rel {
		case "is inside":
			rel = "contains"
		case "contains":
			rel = "is inside"
		}
	}
	msg := fmt.Sprintf("%s%s %s (%s)", b.subject, rel, a.object, a.where())
	if rel == "is the same directory as" {
		msg += ", " + b.resolved
		if a.resolved != a.path || b.resolved != b.path {
			msg += " with symlinks resolved"
		}
	} else {
		msg += fmt.Sprintf(": %s and %s", b.resolved, a.resolved)
	}
	d.errs = append(d.errs, errors.New(d.lineprefix(b.line)+msg))
}

// lineprefix is the start of an error at line, or at no line when it is 0.
func (d *decoder) lineprefix(line int) string {
	if line == 0 {
		return d.prefix
	}
	return fmt.Sprintf("%sline %d: ", d.prefix, line)
}

// within reports whether path is strictly inside dir; both are clean and
// absolute.
func within(path, dir string) bool {
	if dir == "/" {
		return path != "/"
	}
	return strings.HasPrefix(path, dir+"/")
}

// maxLinks bounds the symlinks resolve follows, as the kernel's
// MAXSYMLINKS does.
const maxLinks = 40

// errTooManyLinks is resolve's error for a symlink loop.
var errTooManyLinks = errors.New("too many links")

// resolve returns path, which is absolute, cleaned and with every symlink in
// it resolved, also a symlink whose target doesn't exist yet. Where the
// path doesn't exist, the missing part is kept as written: the plugin
// creates the missing directories at startup.
func resolve(path string) (string, error) {
	resolved := "/"
	rest := strings.Split(path, "/")
	links := 0
	for len(rest) > 0 {
		name := rest[0]
		rest = rest[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, name)
		fi, err := os.Lstat(next)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			if links++; links > maxLinks {
				return "", errTooManyLinks
			}
			target, err := os.Readlink(next)
			if err != nil {
				return "", err
			}
			if filepath.IsAbs(target) {
				resolved = "/"
			}
			rest = append(strings.Split(target, "/"), rest...)
			continue
		}
		resolved = next
	}
	return resolved, nil
}
