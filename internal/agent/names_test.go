// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

func testNames() *agentv1.BootConfig {
	return &agentv1.BootConfig{
		Network:  testNetwork(),
		Hostname: "task-7-build",
		Hosts:    []*agentv1.Host{{Name: "task-7-build", Address: "198.18.0.2"}},
	}
}

// testOwner is who the test can make own a file: itself.
func testOwner() owner { return owner{os.Getuid(), os.Getgid()} }

// wantNameFiles fails t unless root's /etc holds the three files, regular,
// mode 0644, owned by own, and nothing else the agent wrote.
func wantNameFiles(t *testing.T, root string, own owner) {
	t.Helper()
	for name, want := range map[string]string{
		"hosts":       "127.0.0.1 localhost\n::1 localhost\n198.18.0.2 task-7-build\n",
		"hostname":    "task-7-build\n",
		"resolv.conf": "nameserver 198.18.0.1\n",
	} {
		p := filepath.Join(root, "etc", name)
		fi, err := os.Lstat(p)
		if err != nil {
			t.Error(err)
			continue
		}
		st := fi.Sys().(*syscall.Stat_t)
		if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o644 || fi.Mode()&^os.ModePerm != 0 ||
			int(st.Uid) != own.uid || int(st.Gid) != own.gid {
			t.Errorf("%s: %v, owner %d:%d; want a regular file, 0644, owner %d:%d", p, fi.Mode(), st.Uid, st.Gid, own.uid, own.gid)
		}
		if b, err := os.ReadFile(p); err != nil || string(b) != want {
			t.Errorf("%s holds %q (%v), want %q", p, b, err, want)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "etc"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".runner-microvm-") {
			t.Errorf("the temporary file %s is left in /etc", e.Name())
		}
	}
}

// The agent writes /etc/hosts, /etc/hostname and /etc/resolv.conf over
// whatever the image has there, a symlinked resolv.conf included, which it
// replaces with a file, leaving the symlink's target as it was; then it
// sets the hostname.
func TestConfigureNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		desc string
		// target is resolv.conf's symlink target; outside names a file
		// outside the root, which must be left as it was.
		target  string
		outside bool
	}{
		{"a symlink into the image", "../run/systemd/resolve/stub-resolv.conf", false},
		{"a dangling symlink", "../run/systemd/resolve/missing.conf", false},
		{"an absolute symlink naming a host file", "", true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			etc := filepath.Join(root, "etc")
			stub := filepath.Join(root, "run", "systemd", "resolve", "stub-resolv.conf")
			for _, d := range []string{etc, filepath.Dir(stub)} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			victims := []string{stub}
			target := tc.target
			if tc.outside {
				target = filepath.Join(t.TempDir(), "host-resolv.conf")
				victims = append(victims, target)
			}
			for _, v := range victims {
				if err := os.WriteFile(v, []byte("nameserver 127.0.0.53\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, filepath.Join(etc, "resolv.conf")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(etc, "hosts"), []byte("127.0.1.1 image\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var set []string
			err := configureNames(func(name string) error { set = append(set, name); return nil }, root, testOwner(), testNames())
			if err != nil {
				t.Fatal(err)
			}
			wantNameFiles(t, root, testOwner())
			for _, v := range victims {
				if b, err := os.ReadFile(v); err != nil || string(b) != "nameserver 127.0.0.53\n" {
					t.Errorf("the symlink's target %s now holds %q (%v)", v, b, err)
				}
			}
			if len(set) != 1 || set[0] != "task-7-build" {
				t.Errorf("sethostname calls: %q", set)
			}
		})
	}
}

// A root without /etc gets one, mode 0755, owned like the files.
func TestConfigureNamesMakesEtc(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := configureNames(func(string) error { return nil }, root, testOwner(), testNames()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(root, "etc"))
	if err != nil {
		t.Fatal(err)
	}
	if st := fi.Sys().(*syscall.Stat_t); !fi.IsDir() || fi.Mode().Perm() != 0o755 || int(st.Uid) != os.Getuid() || int(st.Gid) != os.Getgid() {
		t.Errorf("/etc: %v, owner %d:%d", fi.Mode(), st.Uid, st.Gid)
	}
	wantNameFiles(t, root, testOwner())
}

// An /etc file the agent can't replace fails the boot, naming it, and
// leaves no temporary file; a temporary file an image left is replaced.
func TestConfigureNamesFailures(t *testing.T) {
	t.Parallel()
	t.Run("a directory at /etc/hosts", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "etc", "hosts", "x"), 0o755); err != nil {
			t.Fatal(err)
		}
		err := configureNames(func(string) error { t.Error("sethostname after a failure"); return nil }, root, testOwner(), testNames())
		if err == nil || !strings.Contains(err.Error(), "names: /etc/hosts: ") {
			t.Fatalf("err = %v, want it to name /etc/hosts", err)
		}
		if _, err := os.Lstat(filepath.Join(root, "etc", ".runner-microvm-hosts")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the temporary file is left: %v", err)
		}
	})
	t.Run("a file at /etc", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "etc"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		err := configureNames(func(string) error { return nil }, root, testOwner(), testNames())
		if err == nil || !strings.Contains(err.Error(), "names: open /etc: ") {
			t.Fatalf("err = %v, want it to name /etc", err)
		}
	})
	t.Run("a temporary file left in the image", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/nowhere", filepath.Join(root, "etc", ".runner-microvm-hostname")); err != nil {
			t.Fatal(err)
		}
		if err := configureNames(func(string) error { return nil }, root, testOwner(), testNames()); err != nil {
			t.Fatal(err)
		}
		wantNameFiles(t, root, testOwner())
	})
	t.Run("sethostname fails", func(t *testing.T) {
		t.Parallel()
		err := configureNames(func(string) error { return syscall.EPERM }, t.TempDir(), testOwner(), testNames())
		if err == nil || err.Error() != "names: sethostname task-7-build: operation not permitted" {
			t.Fatalf("err = %v", err)
		}
	})
}

// A hostname that isn't a DNS label, `_` included, or a host whose name
// holds anything outside a-z, 0-9, `-` and `_` or that isn't at an IPv4
// address, which could add a line of its own to /etc/hosts, fails before
// anything is written.
func TestConfigureNamesRefusesBadNames(t *testing.T) {
	t.Parallel()
	for desc, mutate := range map[string]func(*agentv1.BootConfig){
		"no hostname":           func(c *agentv1.BootConfig) { c.Hostname = "" },
		"a newline in hostname": func(c *agentv1.BootConfig) { c.Hostname = "a\n1.2.3.4 evil" },
		"upper case":            func(c *agentv1.BootConfig) { c.Hostname = "Job" },
		"a leading hyphen":      func(c *agentv1.BootConfig) { c.Hostname = "-job" },
		"64 characters":         func(c *agentv1.BootConfig) { c.Hostname = strings.Repeat("a", 64) },
		"a _ in hostname":       func(c *agentv1.BootConfig) { c.Hostname = "my_job" },
		"a dot in hostname":     func(c *agentv1.BootConfig) { c.Hostname = "a.b" },
		"a host's name":         func(c *agentv1.BootConfig) { c.Hosts[0].Name = "a b" },
		"a newline in a host":   func(c *agentv1.BootConfig) { c.Hosts[0].Name = "my_db\n1.2.3.4 evil" },
		"a dot in a host":       func(c *agentv1.BootConfig) { c.Hosts[0].Name = "my.db" },
		"upper case in a host":  func(c *agentv1.BootConfig) { c.Hosts[0].Name = "My_db" },
		"an empty host":         func(c *agentv1.BootConfig) { c.Hosts[0].Name = "" },
		"a host's address":      func(c *agentv1.BootConfig) { c.Hosts[0].Address = "198.18.0.2\n1.2.3.4" },
		"an IPv6 host":          func(c *agentv1.BootConfig) { c.Hosts[0].Address = "::1" },
		"no gateway":            func(c *agentv1.BootConfig) { c.Network = nil },
	} {
		cfg := testNames()
		mutate(cfg)
		root := t.TempDir()
		if err := configureNames(func(string) error { return nil }, root, testOwner(), cfg); err == nil || !strings.HasPrefix(err.Error(), "boot config: ") {
			t.Errorf("%s: err = %v", desc, err)
		}
		if _, err := os.Lstat(filepath.Join(root, "etc")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: /etc was made: %v", desc, err)
		}
	}
}

// The hosts list may hold several entries, every VM of the job, each
// written on a line of its own in the list's order; a service's name may
// hold `_`, as the Docker backend's alias rule keeps it.
func TestConfigureNamesWritesEveryHost(t *testing.T) {
	t.Parallel()
	cfg := testNames()
	cfg.Hosts = append(cfg.Hosts,
		&agentv1.Host{Name: "my_db", Address: "198.18.0.3"},
		&agentv1.Host{Name: "_cache-2_", Address: "198.18.0.4"})
	root := t.TempDir()
	if err := configureNames(func(string) error { return nil }, root, testOwner(), cfg); err != nil {
		t.Fatal(err)
	}
	want := "127.0.0.1 localhost\n::1 localhost\n198.18.0.2 task-7-build\n198.18.0.3 my_db\n198.18.0.4 _cache-2_\n"
	if b, err := os.ReadFile(filepath.Join(root, "etc", "hosts")); err != nil || string(b) != want {
		t.Errorf("/etc/hosts holds %q (%v), want %q", b, err, want)
	}
}
