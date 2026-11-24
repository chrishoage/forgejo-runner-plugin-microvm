// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/containerd/continuity/tarconv"
	erofs "github.com/erofs/go-erofs"
)

// ext4Disk makes a 16 MiB ext4 file with the host's mkfs.ext4, holding a
// file `marker` whose content is marker, and returns its path.
func ext4Disk(t *testing.T, marker string) string {
	t.Helper()
	mkfs, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		t.Fatalf("mkfs.ext4, which the dev shell provides: %v", err)
	}
	content := t.TempDir()
	if err := os.WriteFile(filepath.Join(content, "marker"), []byte(marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), marker+".ext4")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, mkfs, "-q", "-F", "-d", content, p, "16M").CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v\n%s", err, out)
	}
	return p
}

// erofsDisk makes an EROFS file with go-erofs, holding a file `marker`
// whose content is marker, and returns its path.
func erofsDisk(t *testing.T, marker string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := marker + "\n"
	if err := tw.WriteHeader(&tar.Header{Name: "marker", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), marker+".erofs")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	w := erofs.Create(f)
	if err := errors.Join(tarconv.Apply(w, &buf), w.Close(), f.Close()); err != nil {
		t.Fatal(err)
	}
	return p
}

// fileBytes returns the host file p's content.
func fileBytes(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A profile's disks are read-only block devices at their targets, each
// mounted as the filesystem its superblock names: a step reads each disk's
// own marker at its own target, two ext4 disks and an EROFS one, so a swap
// would show. As root it can't write either kind, also after remounting it
// read-write, and the host's files are unchanged. A target the image lacks
// is created, and one under a scratch path sits on its fresh directory
// (STO-1, STO-2, STO-4, STO-6).
func TestReadOnlyDisks(t *testing.T) {
	img, host := baseImage(t)
	disks := []string{ext4Disk(t, "first"), erofsDisk(t, "tools"), ext4Disk(t, "second")}
	before := make([][]byte, len(disks))
	for i, d := range disks {
		before[i] = fileBytes(t, d)
	}
	p := startPluginWith(t, buildPlugin(t), fmt.Sprintf(`profiles:
  disks:
    scratch_paths: [/var/lib/docker]
    disks:
      - {source: %s, target: /new/first}
      - {source: %s, target: /opt/tools}
      - {source: %s, target: /var/lib/docker/second}
`, disks[0], disks[1], disks[2]))
	wf := writeWorkflow(t, onLabel(step("disks", `
fstype() { awk -v m="$1" '$2 == m { t = $3 } END { print t }' /proc/mounts; }
opts() { awk -v m="$1" '$2 == m { o = $4 } END { print o }' /proc/mounts; }
test "$(id -u)" = 0
test "$(cat /new/first/marker)" = first
test "$(cat /opt/tools/marker)" = tools
test "$(cat /var/lib/docker/second/marker)" = second
test "$(fstype /new/first)" = ext4
test "$(fstype /opt/tools)" = erofs
test "$(fstype /var/lib/docker/second)" = ext4
test "$(fstype /var/lib/docker)" = ext4
for d in /new/first /opt/tools /var/lib/docker/second; do
  case ",$(opts $d)," in *,ro,*) ;; *) echo "$d options: $(opts $d)"; exit 1;; esac
  case ",$(opts $d)," in *,nosuid,*|*,nodev,*) echo "$d options: $(opts $d)"; exit 1;; esac
  if touch $d/x; then echo "wrote $d"; exit 1; fi
  if echo x >> $d/marker; then echo "wrote $d/marker"; exit 1; fi
  mount -o remount,rw $d && echo "remounted $d: $(opts $d)"
  if touch $d/x; then echo "wrote $d after remounting it"; exit 1; fi
  if echo x >> $d/marker; then echo "wrote $d/marker after remounting it"; exit 1; fi
done
sync
echo "disks ok: $((6 * 7))"
`), "microvm-disks"))
	log, err := runWorkflowOn(t, p, wf, map[string]string{"microvm-disks": pushImage(t, host, "disks", img) + ";profile=disks"})
	if err != nil {
		t.Fatalf("the workflow failed: %v", err)
	}
	if !strings.Contains(log, "disks ok: 42") {
		t.Error("the job log does not show the step's success")
	}
	for i, d := range disks {
		if !bytes.Equal(fileBytes(t, d), before[i]) {
			t.Errorf("the guest changed %s", d)
		}
	}
}

// A disk that is neither ext4 nor EROFS fails Start, saying so; so does a
// read-only ext4 disk whose journal needs recovery, saying how to replay
// it, and a disk whose source is deleted after the config was read, naming
// the source (STO-4, PROTO-10).
func TestDiskFailsStart(t *testing.T) {
	img, host := baseImage(t)
	neither := filepath.Join(t.TempDir(), "zeros.img")
	if err := os.WriteFile(neither, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	recovering := ext4Disk(t, "recovering")
	setNeedsRecovery(t, recovering)
	gone := ext4Disk(t, "gone")
	p := startPluginWith(t, buildPlugin(t), fmt.Sprintf(`profiles:
  neither:
    disks: [{source: %s, target: /data}]
  recovering:
    disks: [{source: %s, target: /data}]
  gone:
    disks: [{source: %s, target: /data}]
`, neither, recovering, gone))
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	ref := pushImage(t, host, "disk-fails", img)
	for _, tc := range []struct{ profile, msg string }{
		{"neither", "disk /data: not ext4 or EROFS"},
		{"recovering", "disk /data: its ext4 journal needs recovery, which a read-only mount can't do: mount the disk read-write once to replay it"},
		{"gone", fmt.Sprintf("disk %s: does not exist", gone)},
	} {
		label := "microvm-" + tc.profile
		wf := writeWorkflow(t, onLabel(step(tc.profile, "echo unreachable"), label))
		if _, err := runWorkflowOn(t, p, wf, map[string]string{label: ref + ";profile=" + tc.profile}); err == nil {
			t.Fatalf("%s: the workflow succeeded", tc.profile)
		}
		failed := regexp.MustCompile(`msg="start failed".*` + regexp.QuoteMeta(tc.msg))
		if !failed.MatchString(p.log.String()) {
			t.Errorf("%s: no failed Start saying %q in the plugin log:\n%s", tc.profile, tc.msg, p.log.String())
		}
	}
}

// setNeedsRecovery sets the needs_recovery bit in the ext4 superblock of the
// file p, s_feature_incompat's 0x4 at byte 1120, as a crash with a dirty
// journal leaves it.
func setNeedsRecovery(t *testing.T, p string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, 1120); err != nil {
		t.Fatal(err)
	}
	b[0] |= 0x4
	if _, err := f.WriteAt(b, 1120); err != nil {
		t.Fatal(err)
	}
}
