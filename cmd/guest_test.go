// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// An image of one layer holding a single statically linked program, with no
// shell, no libc and no /proc, /sys or /dev, boots, and an Exec of that
// program succeeds (GST-1).
func TestStaticProgramImageBoots(t *testing.T) {
	_, host := baseImage(t)
	probe, err := os.ReadFile(buildProbe(t))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "probe", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(probe))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(probe); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, bytesLayer(t, buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	v := bootImage(t, startPlugin(t, buildPlugin(t)), "static", pushImage(t, host, "static", img))
	code, out := v.exec(t, "/probe", "netinfo")
	if code != 0 || !strings.Contains(out, "eth0") {
		t.Fatalf("/probe netinfo exited %d:\n%s", code, out)
	}
}

// hostMarker is set in the plugin's environment only, so it reaches the
// guest only if the host's environment does.
const hostMarker = "RUNNER_MICROVM_TEST_HOST_ONLY=leaked"

// guestMount is one mount as a step sees it in /proc/self/mountinfo.
type guestMount struct {
	fstype string
	// opts are the mount's options and its filesystem's, together.
	opts []string
}

// parseMountinfo returns the mounts in a mountinfo listing by mount point,
// the last mount at each point, the one a step sees.
func parseMountinfo(t *testing.T, listing string) map[string]guestMount {
	t.Helper()
	mounts := map[string]guestMount{}
	for line := range strings.Lines(listing) {
		// id parent major:minor root point opts [optional...] - fstype source superopts
		before, after, ok := strings.Cut(strings.TrimSpace(line), " - ")
		f, g := strings.Fields(before), strings.Fields(after)
		if !ok || len(f) < 6 || len(g) < 3 {
			t.Fatalf("mountinfo line %q", line)
		}
		opts := strings.Split(f[5], ",")
		opts = append(opts, strings.Split(g[2], ",")...)
		mounts[f[4]] = guestMount{fstype: g[0], opts: opts}
	}
	return mounts
}

// In a booted VM, on an image without /tmp, /var/run or /opt, a step sees
// the agent's mounts with their options, the agent and its boot config in
// /.runner-microvm, the act path, tool cache and temp directory with modes
// 0777, 0777 and 1777, and the agent's own environment holds nothing from
// the host's.
func TestGuestSetup(t *testing.T) {
	base, host := baseImage(t)
	img := imageWithout(t, base, "tmp", "var/run", "opt")
	key, value, _ := strings.Cut(hostMarker, "=")
	t.Setenv(key, value)
	hostEnv := os.Environ()
	v := bootImage(t, startPlugin(t, buildPlugin(t)), "setup", pushImage(t, host, "setup", img))

	code, mountinfo := v.exec(t, "cat", "/proc/self/mountinfo")
	if code != 0 {
		t.Fatalf("cat /proc/self/mountinfo exited %d: %s", code, mountinfo)
	}
	mounts := parseMountinfo(t, mountinfo)
	for point, want := range map[string]guestMount{
		"/proc":          {"proc", []string{"nosuid", "nodev", "noexec"}},
		"/sys":           {"sysfs", []string{"nosuid", "nodev", "noexec"}},
		"/sys/fs/cgroup": {"cgroup2", []string{"nosuid", "nodev", "noexec"}},
		"/dev":           {"devtmpfs", []string{"nosuid"}},
		"/dev/pts":       {"devpts", []string{"nosuid", "noexec", "gid=5", "mode=620", "ptmxmode=666"}},
		"/dev/shm":       {"tmpfs", []string{"nosuid", "nodev", "noexec"}},
	} {
		got, ok := mounts[point]
		if !ok || got.fstype != want.fstype {
			t.Errorf("%s: %+v, want %s", point, got, want.fstype)
			continue
		}
		for _, o := range want.opts {
			if !slices.Contains(got.opts, o) {
				t.Errorf("%s (%s): options %v lack %s", point, got.fstype, got.opts, o)
			}
		}
	}

	code, out := v.exec(t, "stat", "-c", "%n %F %a", "/.runner-microvm/agent", "/.runner-microvm/boot-config.binpb",
		"/tmp", "/var/run/act", "/opt/hostedtoolcache")
	want := "/.runner-microvm/agent regular file 755\n/.runner-microvm/boot-config.binpb regular file 600\n" +
		"/tmp directory 1777\n/var/run/act directory 777\n/opt/hostedtoolcache directory 777\n"
	if code != 0 || out != want {
		t.Errorf("stat exited %d:\n%s\nwant:\n%s", code, out, want)
	}

	// The agent is libkrun init's workload, started by its path.
	code, out = v.exec(t, "sh", "-c", `
for d in /proc/[0-9]*; do
	if [ "$(tr '\0' '\n' < "$d/cmdline" | head -n 1)" = /.runner-microvm/agent ]; then
		echo "agent ${d#/proc/}"
		tr '\0' '\n' < "$d/environ"
	fi
done`)
	if code != 0 || !strings.HasPrefix(out, "agent ") {
		t.Fatalf("no agent process found (exit %d):\n%s", code, out)
	}
	t.Logf("the agent's process and environment:\n%s", out)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if n := len(slices.DeleteFunc(slices.Clone(lines), func(l string) bool { return !strings.HasPrefix(l, "agent ") })); n != 1 {
		t.Fatalf("%d agent processes:\n%s", n, out)
	}
	for _, kv := range lines[1:] {
		if slices.Contains(hostEnv, kv) {
			t.Errorf("the agent's environment holds the host's %s", kv)
		}
	}
	if slices.Contains(lines, hostMarker) {
		t.Errorf("the agent's environment holds the plugin's %s", hostMarker)
	}
}

// On an image whose /tmp and /opt/hostedtoolcache have modes of their own,
// the agent leaves them as they are.
func TestGuestKeepsTheImagesDirectories(t *testing.T) {
	base, host := baseImage(t)
	img, err := mutate.AppendLayers(base, tarLayer(t, "",
		&tar.Header{Name: "tmp/", Typeflag: tar.TypeDir, Mode: 0o700},
		&tar.Header{Name: "opt/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "opt/hostedtoolcache/", Typeflag: tar.TypeDir, Mode: 0o750}))
	if err != nil {
		t.Fatal(err)
	}
	v := bootImage(t, startPlugin(t, buildPlugin(t)), "keep", pushImage(t, host, "keep", img))
	code, out := v.exec(t, "stat", "-c", "%n %a", "/tmp", "/opt/hostedtoolcache")
	if want := "/tmp 700\n/opt/hostedtoolcache 750\n"; code != 0 || out != want {
		t.Errorf("stat exited %d:\n%s\nwant:\n%s", code, out, want)
	}
}

// A VM whose agent never answers fails Start with FailedPrecondition once
// the ready timeout has passed, naming the VM and the timeout, then its
// console's last lines, which the agent's stderr ends; the job is still
// removed (VM-8).
// The plugin is built with a ready timeout of 10 s, not the shipped 30 s,
// through the test-only link-time seam (internal/plugin's
// testReadyTimeout); the shipped timeout is a constant no config can change.
// Its extracted agent is replaced, before Start, with one that starts and
// then only waits.
func TestSilentAgentFailsStart(t *testing.T) {
	const ready = 10 * time.Second
	base, host := baseImage(t)
	p := startPlugin(t, buildPlugin(t, "-X "+module+"/internal/plugin.testReadyTimeout="+ready.String()))
	agents, err := filepath.Glob(filepath.Join(p.state, "bin", "runner-microvm-agent-*"))
	if err != nil || len(agents) != 1 {
		t.Fatalf("the plugin's agent: %v, %v", agents, err)
	}
	silent, err := os.ReadFile(buildStatic(t, "silentagent"))
	if err != nil {
		t.Fatal(err)
	}
	// A new file renamed into place: the boot root's hard link takes the
	// new one.
	tmp := agents[0] + ".silent"
	if err := os.WriteFile(tmp, silent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, agents[0]); err != nil {
		t.Fatal(err)
	}
	v := createImage(t, p, "silent", pushImage(t, host, "silent", base))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	begin := time.Now()
	err = v.start(ctx)
	took := time.Since(begin)
	msg := status.Convert(err).Message()
	if status.Code(err) != codes.FailedPrecondition || !strings.HasPrefix(msg, "VM silent: the agent did not answer within "+ready.String()+"\n") ||
		!strings.Contains(msg, "\nconsole:\n") || !strings.HasSuffix(msg, "\nsilentagent: started; it never answers") {
		t.Fatalf("Start = %v, want FailedPrecondition: the ready timeout, then the console's last lines", err)
	}
	if took < ready || took > ready+30*time.Second {
		t.Errorf("Start failed after %v, want just over %v", took, ready)
	}
	t.Logf("Start failed after %v: %v", took.Round(time.Millisecond), err)
}
