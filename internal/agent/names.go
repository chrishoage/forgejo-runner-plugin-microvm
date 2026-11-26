// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"strings"
	"syscall"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// owner is who owns the files the agent writes into the job's root.
type owner struct{ uid, gid int }

// rootOwner owns them in the guest.
var rootOwner = owner{0, 0}

// nameFile is a file the agent writes into the job's /etc.
type nameFile struct {
	name string
	data string
}

// configureNames gives the guest its hostname and name resolution from its
// boot config (GST-12): it writes /etc/hosts, /etc/hostname and
// /etc/resolv.conf into the job's root at root, owned by own, then sets the
// kernel's hostname with sethostname. There is no DNS zone and no search
// domain: the gateway is the only name server, and the job network's names
// are in /etc/hosts. A failure names its step.
func configureNames(sethostname func(string) error, root string, own owner, cfg *agentv1.BootConfig) error {
	files, err := nameFiles(cfg)
	if err != nil {
		return err
	}
	if err := writeEtc(root, own, files); err != nil {
		return fmt.Errorf("names: %w", err)
	}
	if err := sethostname(cfg.GetHostname()); err != nil {
		return fmt.Errorf("names: sethostname %s: %w", cfg.GetHostname(), err)
	}
	return nil
}

// sethostname sets the guest kernel's hostname.
func sethostname(name string) error { return syscall.Sethostname([]byte(name)) }

// nameFiles returns the contents of /etc/hosts, /etc/hostname and
// /etc/resolv.conf for cfg, refusing a hostname that isn't a DNS label and a
// host that isn't a host name at an IPv4 address (isHostName), so nothing
// the files hold can add a line of its own. /etc/hosts maps localhost to the
// loopback addresses, then each of the hosts list's names, every VM of the
// job, to its job-network address: the hostname is among them, at the
// address others reach the VM on, not 127.0.1.1.
func nameFiles(cfg *agentv1.BootConfig) ([]nameFile, error) {
	if !isLabel(cfg.GetHostname()) {
		return nil, fmt.Errorf("boot config: hostname %q is not a DNS label", cfg.GetHostname())
	}
	gw, err := netip.ParseAddr(cfg.GetNetwork().GetGateway())
	if err != nil || !gw.Is4() {
		return nil, fmt.Errorf("boot config: gateway %q is not an IPv4 address", cfg.GetNetwork().GetGateway())
	}
	var hosts strings.Builder
	hosts.WriteString("127.0.0.1 localhost\n::1 localhost\n")
	for _, h := range cfg.GetHosts() {
		addr, err := netip.ParseAddr(h.GetAddress())
		if err != nil || !addr.Is4() || !isHostName(h.GetName()) {
			return nil, fmt.Errorf("boot config: host %q at %q is not a name of a-z, 0-9, - and _ at an IPv4 address", h.GetName(), h.GetAddress())
		}
		fmt.Fprintf(&hosts, "%s %s\n", addr, h.GetName())
	}
	return []nameFile{
		{"hosts", hosts.String()},
		{"hostname", cfg.GetHostname() + "\n"},
		{"resolv.conf", "nameserver " + gw.String() + "\n"},
	}, nil
}

// isLabel reports whether s is a DNS label as the plugin makes them: 1 to
// 63 of a-z, 0-9 and `-`, with no `-` at either end.
func isLabel(s string) bool {
	if s == "" || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range []byte(s) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// isHostName reports whether s is a name the hosts list may hold: one or
// more of a-z, 0-9, `-` and `_`. A service's name is its workflow key by the
// Docker backend's alias rule, which keeps `_` and cuts nothing, so it
// needn't be a DNS label; glibc, musl, Go and Node all resolve it from
// /etc/hosts. Nothing else is let through, so a name can't add a line or a
// field of its own.
func isHostName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// writeEtc writes files into /etc in the job's root at root, creating /etc,
// owned by own and mode 0755, when the image lacks it. Every path is
// resolved inside root (os.Root), so a symlink in the image can't take a
// write out of it.
func writeEtc(root string, own owner, files []nameFile) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("open the root %s: %w", root, err)
	}
	defer func() { _ = r.Close() }()
	if err := r.Mkdir("etc", 0o700); err == nil {
		// Owner first, then the mode, explicitly, so the umask can't
		// narrow it.
		if err := r.Chown("etc", own.uid, own.gid); err != nil {
			return fmt.Errorf("chown /etc: %w", err)
		}
		if err := r.Chmod("etc", 0o755); err != nil {
			return fmt.Errorf("chmod /etc: %w", err)
		}
	} else if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("make /etc: %w", err)
	}
	etc, err := r.OpenRoot("etc")
	if err != nil {
		return fmt.Errorf("open /etc: %w", err)
	}
	defer func() { _ = etc.Close() }()
	for _, f := range files {
		if err := replaceFile(etc, f.name, f.data, own); err != nil {
			return fmt.Errorf("/etc/%s: %w", f.name, err)
		}
	}
	return nil
}

// replaceFile makes name in dir a regular file holding data, owned by own,
// mode 0644, whatever the image has there. It writes a temporary file
// beside it and renames it over name, so a symlink at name, Ubuntu's
// resolv.conf among them, dangling or not, is replaced rather than
// followed, and its target is left as it was.
func replaceFile(dir *os.Root, name, data string, own owner) error {
	tmp := ".runner-microvm-" + name
	if err := dir.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", tmp, err)
	}
	// O_EXCL: never through a symlink, nor into a file that is there.
	f, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = fillFile(f, data, own)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = dir.Rename(tmp, name)
	}
	if err != nil {
		_ = dir.Remove(tmp)
	}
	return err
}

// fillFile writes data to f and gives it its owner, then mode 0644,
// explicitly, so the umask can't narrow it.
func fillFile(f *os.File, data string, own owner) error {
	if _, err := f.WriteString(data); err != nil {
		return err
	}
	if err := f.Chown(own.uid, own.gid); err != nil {
		return err
	}
	return f.Chmod(0o644)
}
