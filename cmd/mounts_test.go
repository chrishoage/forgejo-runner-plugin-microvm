// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	pluginv1alpha "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"golang.org/x/sys/unix"
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
	t.Parallel()
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
	t.Parallel()
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

// rwProfile is the profile rw: read-write mounts of data at /data and of
// open at /open, and a read-only one of tools at /tools.
func rwProfile(data, open, tools string) string {
	return fmt.Sprintf(`profiles:
  rw:
    mounts:
      - {source: %s, target: /data, read_write: true}
      - {source: %s, target: /open, read_write: true}
      - {source: %s, target: /tools}
`, data, open, tools)
}

// otherUID is a guest uid that isn't the test's: 1001, unless the test runs
// as 1001, as GitHub's runners do, and then 1002.
func otherUID() int {
	if os.Getuid() == 1001 {
		return 1002
	}
	return 1001
}

// writableDir makes the host directory d writable by every user, so only
// libkrun's rules decide which guest users may create files in it.
func writableDir(t *testing.T, d string) {
	t.Helper()
	if err := os.Chmod(d, 0o777); err != nil {
		t.Fatal(err)
	}
}

// as runs script with sh in v as user, and returns its exit code and
// output.
func as(t *testing.T, v *vm, user, script string) (int32, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	code, out, err := v.runRequest(ctx, &pluginv1alpha.ExecRequest{Command: []string{"sh", "-c", script}, User: &user, Workdir: "/"})
	if err != nil {
		t.Fatal(err)
	}
	return code, out
}

// wantOwner fails t unless the host file p is owned by uid and gid.
func wantOwner(t *testing.T, p string, uid, gid int) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s: no stat_t", p)
	}
	if int(st.Uid) != uid || int(st.Gid) != gid {
		t.Errorf("%s is owned by %d:%d, want %d:%d", p, st.Uid, st.Gid, uid, gid)
	}
}

// wantNode fails t unless the host file p has the mode mode, its type,
// setuid and setgid bits included, and, for a device, the device number
// rdev.
func wantNode(t *testing.T, p string, mode fs.FileMode, rdev uint64) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode() != mode {
		t.Errorf("%s has mode %v, want %v", p, fi.Mode(), mode)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Rdev != rdev {
		t.Errorf("%s is device %v, want %d", p, fi.Sys(), rdev)
	}
}

// suidMode is the mode of a file the guest's root copies /bin/sh to and
// gives mode 6755.
const suidMode = 0o755 | fs.ModeSetuid | fs.ModeSetgid

// With the plugin unprivileged, a read-write mount is a virtiofs share
// mounted rw,nosuid,nodev that the guest's root writes, and a file it makes
// is owned on the host by the plugin's user. A guest user with that uid can
// create files too. One with another uid can't: in the host directory of
// mode 0755 the guest's kernel refuses it (Permission denied), and in the
// one of mode 0777 libkrun does (Operation not permitted). Root's chown of
// a file there fails. Root's setuid and setgid bits on a file reach the
// host, on the plugin user's file, and its mknod of a block or a character
// device fails with "Operation not permitted", leaving nothing on the host.
// A read-only mount in the same profile still can't be written (STO-2,
// STO-3).
func TestReadWriteMounts(t *testing.T) {
	t.Parallel()
	img, host := baseImage(t)
	src := hostDirs(t, "data", "open", "tools")
	writableDir(t, src[1])
	p := startPluginWith(t, buildPlugin(t), rwProfile(src[0], src[1], src[2]))
	v := bootImage(t, p, "rw-mounts", pushImage(t, host, "rw-mounts", img)+";profile=rw")
	uid, gid, other := os.Getuid(), os.Getgid(), strconv.Itoa(otherUID())
	if code, out := as(t, v, "0", `
opts=$(awk '$2 == "/data" { o = $4 } END { print o }' /proc/mounts)
case ",$opts," in *,rw,*nosuid,*nodev,*) ;; *) echo "options: $opts"; exit 1;; esac
test "$(cat /data/hello)" = data
echo root > /data/by-root`); code != 0 {
		t.Fatalf("root's write exited %d:\n%s", code, out)
	}
	wantOwner(t, filepath.Join(src[0], "by-root"), uid, gid)
	own := strconv.Itoa(uid) + ":" + strconv.Itoa(gid)
	if code, out := as(t, v, own, "touch /data/by-own"); code != 0 {
		t.Errorf("as the plugin's uid, %s, touch exited %d:\n%s", own, code, out)
	}
	wantOwner(t, filepath.Join(src[0], "by-own"), uid, gid)
	for dir, msg := range map[string]string{"data": "Permission denied", "open": "Operation not permitted"} {
		code, out := as(t, v, other, "touch /"+dir+"/by-other")
		if code == 0 || !strings.Contains(out, msg) {
			t.Errorf("as uid %s, touch in /%s exited %d, want a failure saying %s:\n%s", other, dir, code, msg, out)
		}
	}
	for _, d := range src[:2] {
		if _, err := os.Lstat(filepath.Join(d, "by-other")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("uid %s's file is in %s: %v", other, d, err)
		}
	}
	if code, out := as(t, v, "0", "chown "+other+" /data/by-root"); code == 0 {
		t.Errorf("root's chown %s succeeded:\n%s", other, out)
	}
	wantOwner(t, filepath.Join(src[0], "by-root"), uid, gid)
	if code, out := as(t, v, "0", "cp /bin/sh /data/suid && chmod 6755 /data/suid"); code != 0 {
		t.Errorf("root's setuid copy exited %d:\n%s", code, out)
	}
	wantNode(t, filepath.Join(src[0], "suid"), suidMode, 0)
	wantOwner(t, filepath.Join(src[0], "suid"), uid, gid)
	for _, node := range []string{"blk b 8 0", "chr c 1 3"} {
		name := strings.Fields(node)[0]
		code, out := as(t, v, "0", "mknod /data/"+node)
		if code == 0 || !strings.Contains(out, "Operation not permitted") {
			t.Errorf("root's mknod %s exited %d, want a failure saying Operation not permitted:\n%s", node, code, out)
		}
		if _, err := os.Lstat(filepath.Join(src[0], name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("root's %s is in %s: %v", name, src[0], err)
		}
	}
	if code, out := as(t, v, "0", "touch /tools/x"); code == 0 {
		t.Errorf("root wrote the read-only mount:\n%s", out)
	}
	if entries, err := os.ReadDir(src[2]); err != nil || len(entries) != 1 {
		t.Errorf("the guest changed %s: %v, %v", src[2], entries, err)
	}
}

// With the plugin as root, a file the guest makes on a read-write mount
// gets the guest's uid and gid on the host, and chown works (STO-3, VM-9).
// So root's setuid and setgid copy of /bin/sh is a setuid-root file on the
// host, and its mknod of /dev/sda's block device and /dev/null's character
// device, mode 0666, makes them on the host, root's.
// The plugin runs under `sudo -n`; without it the test skips, saying so,
// unless CI is set.
func TestReadWriteMountsAsRoot(t *testing.T) {
	t.Parallel()
	needRoot(t)
	img, host := baseImage(t)
	src := hostDirs(t, "data", "open", "tools")
	writableDir(t, src[1])
	p := startRootPluginWith(t, buildPlugin(t), rwProfile(src[0], src[1], src[2]))
	v := bootImage(t, p, "rw-mounts-root", pushImage(t, host, "rw-mounts-root", img)+";profile=rw")
	other := otherUID()
	ids := strconv.Itoa(other) + ":" + strconv.Itoa(other)
	if code, out := as(t, v, "0", "echo root > /open/by-root && touch /open/given && chown "+ids+" /open/given"); code != 0 {
		t.Fatalf("root's write and chown exited %d:\n%s", code, out)
	}
	wantOwner(t, filepath.Join(src[1], "by-root"), 0, 0)
	wantOwner(t, filepath.Join(src[1], "given"), other, other)
	if code, out := as(t, v, ids, "touch /open/by-other"); code != 0 {
		t.Fatalf("as %s, touch exited %d:\n%s", ids, code, out)
	}
	wantOwner(t, filepath.Join(src[1], "by-other"), other, other)
	if code, out := as(t, v, "0", "cd /open && cp /bin/sh suid && chmod 6755 suid && mknod blk b 8 0 && mknod chr c 1 3 && chmod 666 blk chr"); code != 0 {
		t.Fatalf("root's setuid copy and mknod exited %d:\n%s", code, out)
	}
	for name, want := range map[string]struct {
		mode fs.FileMode
		rdev uint64
	}{
		"suid": {suidMode, 0},
		"blk":  {0o666 | fs.ModeDevice, unix.Mkdev(8, 0)},
		"chr":  {0o666 | fs.ModeDevice | fs.ModeCharDevice, unix.Mkdev(1, 3)},
	} {
		wantNode(t, filepath.Join(src[1], name), want.mode, want.rdev)
		wantOwner(t, filepath.Join(src[1], name), 0, 0)
	}
}
