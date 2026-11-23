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

	"github.com/moby/sys/user"
)

// identity is the user an Exec runs as.
type identity struct {
	uid, gid uint32
	// groups are the supplementary groups.
	groups []uint32
	// home is the user's home directory, the default HOME.
	home string
}

// resolveUser resolves spec, a user name or id, optionally with `:group`,
// against the /etc/passwd and /etc/group of the job's root at root, by
// runc's rules (GST-5): a numeric uid with no passwd entry gets gid 0 and
// home /; a user found by name or id gets the groups that list it as a
// member, root included; an explicit group replaces the user's own and its
// supplementary groups. Empty spec is root. A missing passwd or group file
// is an empty one.
func resolveUser(root, spec string) (identity, error) {
	u, err := user.GetExecUserPath(spec, &user.ExecUser{Home: "/"},
		filepath.Join(root, "etc", "passwd"), filepath.Join(root, "etc", "group"))
	if err != nil {
		if spec == "" {
			spec = "root"
		}
		return identity{}, fmt.Errorf("user %s: %w", spec, err)
	}
	// moby/sys/user keeps ids within 0 to MaxInt32.
	id := identity{uid: uint32(u.Uid), gid: uint32(u.Gid), home: u.Home} //nolint:gosec // G115: ids are 0 to MaxInt32
	for _, g := range u.Sgids {
		id.groups = append(id.groups, uint32(g)) //nolint:gosec // G115: ids are 0 to MaxInt32
	}
	if id.home == "" {
		id.home = "/"
	}
	return id, nil
}

// credential returns the ids a command runs with as id, or nil when they are
// the agent's own and nothing needs changing. The agent in a VM runs as root
// and can take any; one on the host, in tests, can only keep its own.
func credential(id identity) (*syscall.Credential, error) {
	own, err := os.Getgroups()
	if err != nil {
		return nil, err
	}
	if uint32(os.Geteuid()) == id.uid && uint32(os.Getegid()) == id.gid && sameIDs(own, id.groups) { //nolint:gosec // G115: ids are 0 to MaxUint32
		return nil, nil
	}
	return &syscall.Credential{Uid: id.uid, Gid: id.gid, Groups: id.groups}, nil
}

// sameIDs says whether a and b hold the same ids, in any order.
func sameIDs(a []int, b []uint32) bool {
	as := make([]uint32, 0, len(a))
	for _, g := range a {
		as = append(as, uint32(g)) //nolint:gosec // G115: ids are 0 to MaxUint32
	}
	slices.Sort(as)
	bs := slices.Sorted(slices.Values(b))
	return slices.Equal(slices.Compact(as), slices.Compact(bs))
}

// makeWorkDir makes sure the working directory dir exists, creating it and
// its missing parents, mode 0755, owned by id, the user the command runs as,
// so a step can write its own working directory (GST-6). A directory that
// exists keeps its owner and mode: re-owning an image's directory would
// change the image.
func makeWorkDir(dir string, id identity) error {
	return makeDirs(dir, id, nil)
}

// makeDirs is makeWorkDir, calling created, when not nil, with the path of
// each directory it makes, before it re-owns it: a test's way to swap it.
//
// It walks dir one component at a time from an open parent, so each step
// acts on the directory it holds, never on what a path names by then. An
// existing component is followed, symlink or not, as the image means it; one
// it made is never followed: a step's background process, running as id,
// may swap it for a symlink, and the chown and chmod would then land on the
// symlink's target.
func makeDirs(dir string, id identity, created func(string)) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("working directory %q: not an absolute path", dir)
	}
	const dirFlags = syscall.O_RDONLY | syscall.O_DIRECTORY | syscall.O_CLOEXEC
	fd, err := syscall.Open("/", dirFlags, 0)
	if err != nil {
		return fmt.Errorf("working directory %s: %w", dir, os.NewSyscallError("open /", err))
	}
	defer func() { _ = syscall.Close(fd) }()
	p := "/"
	for part := range strings.SplitSeq(filepath.Clean(dir), "/") {
		if part == "" {
			continue
		}
		p = filepath.Join(p, part)
		err := syscall.Mkdirat(fd, part, 0o755)
		made := err == nil
		if err != nil && !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("working directory %s: %w", dir, &os.PathError{Op: "mkdir", Path: p, Err: err})
		}
		if made && created != nil {
			created(p)
		}
		flags := dirFlags
		if made {
			flags |= syscall.O_NOFOLLOW
		}
		next, err := syscall.Openat(fd, part, flags, 0)
		if errors.Is(err, syscall.ENOTDIR) || (made && errors.Is(err, syscall.ELOOP)) {
			return fmt.Errorf("working directory %s: %s is not a directory", dir, p)
		}
		if err != nil {
			return fmt.Errorf("working directory %s: %w", dir, &os.PathError{Op: "open", Path: p, Err: err})
		}
		_ = syscall.Close(fd)
		fd = next
		if !made {
			continue
		}
		// The owner first, then the mode, so neither the umask nor the
		// chown drops a bit.
		if err := syscall.Fchown(fd, int(id.uid), int(id.gid)); err != nil {
			return fmt.Errorf("working directory %s: %w", dir, &os.PathError{Op: "chown", Path: p, Err: err})
		}
		if err := syscall.Fchmod(fd, 0o755); err != nil {
			return fmt.Errorf("working directory %s: %w", dir, &os.PathError{Op: "chmod", Path: p, Err: err})
		}
	}
	return nil
}
