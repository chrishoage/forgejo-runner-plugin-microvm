// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// addresses returns the addresses `probe lookup` printed.
func addresses(out string) []string {
	var addrs []string
	for line := range strings.Lines(out) {
		if a, ok := strings.CutPrefix(strings.TrimSpace(line), "address "); ok {
			addrs = append(addrs, a)
		}
	}
	return addrs
}

// In a booted VM, the hostname is the job's name made DNS-safe, and it
// resolves to the VM's own address on the job network, localhost to the
// loopback, and every other name through the gateway, as a step's programs
// resolve them: through /etc/hosts and /etc/resolv.conf (GST-12).
func TestGuestNames(t *testing.T) {
	t.Parallel()
	// A name only the host's resolver knows: the CI host's own name,
	// which the guest's /etc/hosts doesn't hold. The job process resolves
	// it with Go's own resolver, as the test does here.
	hostName, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	hostIPs, err := (&net.Resolver{PreferGo: true}).LookupNetIP(ctx, "ip4", hostName)
	if err != nil || len(hostIPs) == 0 {
		t.Fatalf("the CI host's resolver doesn't resolve its own name %s to IPv4: %v", hostName, err)
	}
	var hostAddrs []string
	for _, ip := range hostIPs {
		hostAddrs = append(hostAddrs, ip.Unmap().String())
	}
	// The gateway asks the host's resolver itself, which may answer with
	// more of the host's addresses than the lookup above: systemd-resolved
	// lists every interface's for the host's own name, docker0's included.
	ifAddrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	ownAddrs := slices.Clone(hostAddrs)
	for _, a := range ifAddrs {
		if n, ok := a.(*net.IPNet); ok {
			ownAddrs = append(ownAddrs, n.IP.String())
		}
	}

	probe := buildProbe(t)
	// Named as forgejo-runner v13 names a job's container:
	// FORGEJO-ACTIONS-TASK-<task id>_WORKFLOW-<sha256 of the run, in hex>_JOB-<job>.
	// The rule cuts it at 63 characters, inside the hash.
	sum := sha256.Sum256([]byte("Names test/build"))
	hash := hex.EncodeToString(sum[:])
	v := bootVM(t, startPlugin(t, buildPlugin(t)), "FORGEJO-ACTIONS-TASK-7_WORKFLOW-"+hash+"_JOB-build")
	v.copyIn(t, probe, probeDir)
	want := "forgejo-actions-task-7-workflow-" + hash[:31]
	if hostName == want {
		t.Fatalf("the CI host is named %s, as the guest is", hostName)
	}

	if code, out := v.exec(t, "hostname"); code != 0 || strings.TrimSpace(out) != want {
		t.Errorf("hostname: exit %d: %q, want %q", code, out, want)
	}
	if got := addresses(probeIn(t, v, "lookup", want)); !slices.Equal(got, []string{"198.18.0.2"}) {
		t.Errorf("%s resolves to %v, want the VM's 198.18.0.2", want, got)
	}
	if got := addresses(probeIn(t, v, "lookup", "localhost")); !slices.Equal(got, []string{"127.0.0.1"}) {
		t.Errorf("localhost resolves to %v, want 127.0.0.1", got)
	}
	// Resolved through the gateway: at least one address the host's own
	// lookup gave, and nothing that isn't one of the host's.
	got := addresses(probeIn(t, v, "lookup", hostName))
	if !slices.ContainsFunc(got, func(a string) bool { return slices.Contains(hostAddrs, a) }) ||
		slices.ContainsFunc(got, func(a string) bool { return !slices.Contains(ownAddrs, a) }) {
		t.Errorf("the CI host's name %s resolves to %v in the guest, want at least one of %v and only the host's own %v", hostName, got, hostAddrs, ownAddrs)
	}
}

// On an image whose /etc/resolv.conf is a symlink, here a dangling one as
// on Ubuntu without systemd-resolved running, the guest's /etc/resolv.conf
// is a regular file naming the gateway.
func TestResolvConfReplacesASymlink(t *testing.T) {
	t.Parallel()
	base, host := baseImage(t)
	img, err := mutate.AppendLayers(base, tarLayer(t, "", &tar.Header{
		Name: "etc/resolv.conf", Typeflag: tar.TypeSymlink, Linkname: "../run/systemd/resolve/stub-resolv.conf",
	}))
	if err != nil {
		t.Fatal(err)
	}
	v := bootImage(t, startPlugin(t, buildPlugin(t)), "resolv", pushImage(t, host, "resolv-symlink", img))
	code, out := v.exec(t, "sh", "-c", "test -f /etc/resolv.conf && test ! -L /etc/resolv.conf && cat /etc/resolv.conf")
	if code != 0 || out != "nameserver 198.18.0.1\n" {
		t.Errorf("/etc/resolv.conf: exit %d: %q, want a regular file holding %q", code, out, "nameserver 198.18.0.1\n")
	}
}
