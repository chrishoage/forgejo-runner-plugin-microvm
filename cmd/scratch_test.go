// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"archive/tar"
	"regexp"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// scratchProfiles are the profiles the scratch path tests' plugin defines:
// scratch lists a nested pair child first, a directory the image owns with
// another owner and mode, a sticky one, one under an absolute symlink and
// one the image lacks; file lists a path that is a file in the image, and
// dev one under a symlink to /dev.
const scratchProfiles = `profiles:
  scratch:
    scratch_paths:
      - /var/lib/docker/volumes
      - /var/lib/docker
      - /srv/data
      - /srv/sticky
      - /link/sub
      - /new/deep
  file:
    scratch_paths: [/notadir]
  dev:
    scratch_paths: [/devlink]
`

func dir(name string, uid, gid int, mode int64) *tar.Header {
	return &tar.Header{Name: name, Typeflag: tar.TypeDir, Uid: uid, Gid: gid, Mode: mode}
}

// scratchImage is the base with a file in every scratch path it has, a
// symlink /link to /real, a symlink /devlink to /dev, and a file /notadir.
func scratchImage(t *testing.T) (v1.Image, string) {
	t.Helper()
	base, host := baseImage(t)
	img, err := mutate.AppendLayers(base, tarLayer(t, "seed",
		dir("var/lib/docker/", 0, 0, 0o711),
		regular("var/lib/docker/seed"),
		dir("var/lib/docker/volumes/", 0, 0, 0o700),
		regular("var/lib/docker/volumes/seed"),
		dir("srv/data/", 1234, 2345, 0o750),
		regular("srv/data/seed"),
		dir("srv/sticky/", 0, 0, 0o1777),
		regular("srv/sticky/seed"),
		dir("real/", 0, 0, 0o755),
		dir("real/sub/", 0, 0, 0o755),
		regular("real/sub/seed"),
		&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/real", Mode: 0o777},
		&tar.Header{Name: "devlink", Typeflag: tar.TypeSymlink, Linkname: "/dev", Mode: 0o777},
		regular("notadir"),
	))
	if err != nil {
		t.Fatal(err)
	}
	return img, host
}

// Each scratch path is a directory on the scratch disk's ext4, not on the
// overlay, and empty although the image has a file there. It has the
// image directory's owner and mode, even below another scratch path, or root's and 0755 where the image has
// none; one under an absolute symlink lands at the symlink's target in the
// job's root; and a file written to one in a job isn't there in the next
// (GST-13).
func TestScratchPaths(t *testing.T) {
	t.Parallel()
	img, host := scratchImage(t)
	p := startPluginWith(t, buildPlugin(t), scratchProfiles)
	run := func(job, script string) string {
		return strings.Replace(step(job, script), "runs-on: microvm\n", "runs-on: microvm-scratch\n", 1)
	}
	second := strings.Replace(run("second", `
test ! -e /var/lib/docker/left-behind
echo "fresh scratch: $((6 * 7))"
`), "    runs-on: microvm-scratch\n", "    runs-on: microvm-scratch\n    needs: first\n", 1)
	wf := writeWorkflow(t, run("first", `
fstype() { awk -v m="$1" '{ for (i = 7; i <= NF; i++) if ($i == "-") { t[$5] = $(i + 1); break } } END { print t[m] }' /proc/self/mountinfo; }
test "$(fstype /)" = overlay
for p in /var/lib/docker /var/lib/docker/volumes /srv/data /srv/sticky /real/sub /new/deep; do
  test "$(fstype $p)" = ext4 || { echo "$p is on $(fstype $p)"; exit 1; }
done
test "$(ls -A /var/lib/docker)" = volumes
for p in /var/lib/docker/volumes /srv/data /srv/sticky /real/sub /link/sub /new/deep; do
  test -z "$(ls -A $p)" || { echo "$p holds $(ls -A $p)"; exit 1; }
done
test -z "$(fstype /link/sub)"
echo "modes: $(for p in /var/lib/docker /var/lib/docker/volumes /srv/data /srv/sticky /real/sub /new /new/deep; do stat -c %u:%g:%a $p; done | tr '\n' ' ')"
echo kept > /var/lib/docker/left-behind
echo "scratch ok: $((6 * 7))"
`)+second)
	log, err := runWorkflowOn(t, p, wf, map[string]string{"microvm-scratch": pushImage(t, host, "scratch", img) + ";profile=scratch"})
	if err != nil {
		t.Fatalf("the workflow failed: %v", err)
	}
	for _, want := range []string{
		"modes: 0:0:711 0:0:700 1234:2345:750 0:0:1777 0:0:755 0:0:755 0:0:755 ",
		"scratch ok: 42",
		"fresh scratch: 42",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the job log does not show %q", want)
		}
	}
}

// A scratch path that is a file in the image, or under a symlink to /dev,
// fails Start, and the error names the path.
func TestScratchPathOnAFileFailsStart(t *testing.T) {
	t.Parallel()
	img, host := scratchImage(t)
	p := startPluginWith(t, buildPlugin(t), scratchProfiles)
	ref := pushImage(t, host, "scratch-file", img)
	for profile, msg := range map[string]string{
		"file": `scratch path /notadir: /notadir is not a directory in the image`,
		"dev":  `scratch path /devlink: /devlink crosses a mount: a scratch path can't be in /proc, /sys or /dev`,
	} {
		label := "microvm-" + profile
		wf := writeWorkflow(t, strings.Replace(step(profile, "echo unreachable"), "runs-on: microvm\n", "runs-on: "+label+"\n", 1))
		if _, err := runWorkflowOn(t, p, wf, map[string]string{label: ref + ";profile=" + profile}); err == nil {
			t.Fatalf("%s: the workflow succeeded", profile)
		}
		failed := regexp.MustCompile(`msg="start failed".*` + regexp.QuoteMeta(msg))
		if !failed.MatchString(p.log.String()) {
			t.Errorf("%s: no failed Start naming the path in the plugin log:\n%s", profile, p.log.String())
		}
	}
}
