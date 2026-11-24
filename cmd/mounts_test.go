// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"archive/tar"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// mountImage is the base with a symlink /link to /real and a file /notadir.
func mountImage(t *testing.T) (v1.Image, string) {
	t.Helper()
	base, host := baseImage(t)
	img, err := mutate.AppendLayers(base, tarLayer(t, "seed",
		dir("real/", 0, 0, 0o755),
		&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/real", Mode: 0o777},
		regular("notadir"),
	))
	if err != nil {
		t.Fatal(err)
	}
	return img, host
}

// hostDirs makes a host directory for each name, holding a file `hello`
// whose content is the name, and returns their paths.
func hostDirs(t *testing.T, names ...string) []string {
	t.Helper()
	base := t.TempDir()
	var dirs []string
	for _, name := range names {
		d := filepath.Join(base, name)
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "hello"), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, d)
	}
	return dirs
}

// onLabel puts job on label instead of microvm.
func onLabel(job, label string) string {
	return strings.Replace(job, "runs-on: microvm\n", "runs-on: "+label+"\n", 1)
}

// A profile's mounts are read-only virtiofs shares at their targets: a step
// reads the host's file there, and as root can't write it, also after
// remounting it read-write or mounting its tag again read-write elsewhere;
// /proc/mounts shows it nosuid,nodev. A target the image lacks is created,
// one under an absolute symlink lands at the link's target inside the job's
// root, and one under a scratch path sits on the scratch path's fresh
// directory (STO-1, STO-2, STO-5, STO-6).
func TestReadOnlyMounts(t *testing.T) {
	img, host := mountImage(t)
	src := hostDirs(t, "tools", "linked", "nested")
	p := startPluginWith(t, buildPlugin(t), fmt.Sprintf(`profiles:
  mounts:
    scratch_paths: [/var/lib/docker]
    mounts:
      - {source: %s, target: /new/tools}
      - {source: %s, target: /link/sub}
      - {source: %s, target: /var/lib/docker/tools}
`, src[0], src[1], src[2]))
	wf := writeWorkflow(t, onLabel(step("mounts", `
fstype() { awk -v m="$1" '$2 == m { t = $3 } END { print t }' /proc/mounts; }
opts() { awk -v m="$1" '$2 == m { o = $4 } END { print o }' /proc/mounts; }
test "$(id -u)" = 0
test "$(cat /new/tools/hello)" = tools
test "$(fstype /new/tools)" = virtiofs
case ",$(opts /new/tools)," in *,ro,*nosuid,*nodev,*) ;; *) echo "options: $(opts /new/tools)"; exit 1;; esac
if touch /new/tools/x; then echo "wrote the mount"; exit 1; fi
if echo x >> /new/tools/hello; then echo "wrote the mount's file"; exit 1; fi
mount -o remount,rw /new/tools
case ",$(opts /new/tools)," in *,rw,*) ;; *) echo "not remounted: $(opts /new/tools)"; exit 1;; esac
if touch /new/tools/x; then echo "wrote the remounted mount"; exit 1; fi
mkdir /again
mount -t virtiofs -o rw mount0 /again
test "$(cat /again/hello)" = tools
if touch /again/x; then echo "wrote the tag mounted again"; exit 1; fi
test "$(cat /real/sub/hello)" = linked
test "$(fstype /real/sub)" = virtiofs
test "$(fstype /link/sub)" = ""
test "$(fstype /var/lib/docker)" = ext4
test "$(cat /var/lib/docker/tools/hello)" = nested
test "$(fstype /var/lib/docker/tools)" = virtiofs
echo "mounts ok: $((6 * 7))"
`), "microvm-mounts"))
	log, err := runWorkflowOn(t, p, wf, map[string]string{"microvm-mounts": pushImage(t, host, "mounts", img) + ";profile=mounts"})
	if err != nil {
		t.Fatalf("the workflow failed: %v", err)
	}
	if !strings.Contains(log, "mounts ok: 42") {
		t.Error("the job log does not show the step's success")
	}
	for _, d := range src {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Errorf("the guest changed %s: %v", d, entries)
		}
		if b, err := os.ReadFile(filepath.Join(d, "hello")); err != nil || string(b) != filepath.Base(d)+"\n" {
			t.Errorf("%s/hello is %q, %v", d, b, err)
		}
	}
}

// A mount whose target is a file in the image fails Start, naming the
// target; so does one whose source is deleted after the config was read,
// naming the source (PROTO-10).
func TestMountFailsStart(t *testing.T) {
	img, host := mountImage(t)
	src := hostDirs(t, "tools", "gone")
	p := startPluginWith(t, buildPlugin(t), fmt.Sprintf(`profiles:
  file:
    mounts: [{source: %s, target: /notadir}]
  gone:
    mounts: [{source: %s, target: /opt/gone}]
`, src[0], src[1]))
	if err := os.RemoveAll(src[1]); err != nil {
		t.Fatal(err)
	}
	ref := pushImage(t, host, "mount-fails", img)
	for profile, msg := range map[string]string{
		"file": `mount /notadir: /notadir is not a directory in the image`,
		"gone": fmt.Sprintf(`mount %s: does not exist`, src[1]),
	} {
		label := "microvm-" + profile
		wf := writeWorkflow(t, onLabel(step(profile, "echo unreachable"), label))
		if _, err := runWorkflowOn(t, p, wf, map[string]string{label: ref + ";profile=" + profile}); err == nil {
			t.Fatalf("%s: the workflow succeeded", profile)
		}
		failed := regexp.MustCompile(`msg="start failed".*` + regexp.QuoteMeta(msg))
		if !failed.MatchString(p.log.String()) {
			t.Errorf("%s: no failed Start naming %q in the plugin log:\n%s", profile, msg, p.log.String())
		}
	}
}
