// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// serveScript is a service's /serve: it answers a connection with the
// file whose guest path the connection's first line names.
const serveScript = `#!/bin/sh
read -r path
cat "$path"
`

// storageService is what a storage service runs: it writes its report on
// the profile's storage to /tmp/report, which says "has <target>" for each
// read-write target present, "wrote <target>" for each read-only one it
// could write, also after remounting it read-write, and "storage ok" when
// neither happened, then serves its root on port 8000 through /serve.
const storageService = `report=/tmp/report
: > $report
for t in /srv/cache /srv/store; do
  if [ -e $t ]; then echo "has $t" >> $report; fi
done
for f in /srv/seed/hello /srv/tools/marker; do
  t=${f%/*}
  if touch $t/x 2>/dev/null; then echo "wrote $t" >> $report; fi
  if echo x 2>/dev/null >> $f; then echo "wrote $f" >> $report; fi
  mount -o remount,rw $t 2>/dev/null
  if touch $t/y 2>/dev/null; then echo "wrote $t after remounting it" >> $report; fi
  if echo x 2>/dev/null >> $f; then echo "wrote $f after remounting $t" >> $report; fi
done
[ -s $report ] || echo "storage ok" > $report
exec nc -lk -p 8000 -e /serve
`

// storageServiceImage returns the test image with /serve, running
// storageService, healthy once port 8000 listens, served from the
// in-process registry as repo; extra are more entries of its layer, each
// holding serveScript.
func storageServiceImage(t *testing.T, repo string, extra ...*tar.Header) string {
	t.Helper()
	base, host := baseImage(t)
	img, err := mutate.AppendLayers(base, tarLayer(t, serveScript,
		append([]*tar.Header{{Name: "serve", Typeflag: tar.TypeReg, Mode: 0o755}}, extra...)...))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Config
	c.Entrypoint, c.Cmd = nil, []string{"sh", "-c", storageService}
	// The step reaches the service with no retry, so Start waits until it
	// listens.
	c.Healthcheck = &v1.HealthConfig{Test: []string{"CMD-SHELL", "nc -z 127.0.0.1 8000"}, Interval: 200 * time.Millisecond, Retries: 100}
	if img, err = mutate.Config(img, c); err != nil {
		t.Fatal(err)
	}
	return pushImage(t, host, repo, img)
}

// storageJob is a workflow job on label with the service store from image,
// whose step runs script.
func storageJob(label, image, script string) string {
	return fmt.Sprintf(`  storage:
    runs-on: %s
    services:
      store:
        image: %s
    steps:
      - shell: sh -e {0}
        run: |
          %s
`, label, image, strings.ReplaceAll(strings.TrimSpace(script), "\n", "\n          "))
}

// A job's service gets its profile's read-only mounts and disks and none of
// its read-write ones, while the job's container gets all of them: on a
// profile with a read-only mount and disk and a read-write mount, then on
// one with a read-only mount and disk and a read-write disk (3 entries at
// most per profile, CFG-10), a step fetches both read-only markers from the
// service by its name, and the service reports that it has neither
// read-write target and can't write the read-only ones, also after
// remounting them read-write. The step reads the same markers and writes
// its read-write target. A service whose image has a file at a read-only
// mount's target fails Start, naming its VM and the target, with no step
// run. The host's read-only files are unchanged. One job at a time: at
// most two VMs.
func TestServicesTakeTheProfilesReadOnlyStorage(t *testing.T) {
	t.Parallel()
	dirs := hostDirs(t, "seed", "cache")
	seed, cache := dirs[0], dirs[1]
	tools, store := ext4Disk(t, "tools"), ext4Disk(t, "store")
	toolsBefore := fileBytes(t, tools)
	const shape = "    cpus: 1\n    memory_mib: 256\n    scratch_gib: 2\n"
	p := startPluginWith(t, buildPlugin(t), fmt.Sprintf(`profiles:
  rwmount:
%s    mounts:
      - {source: %s, target: /srv/seed}
      - {source: %s, target: /srv/cache, read_write: true}
    disks:
      - {source: %s, target: /srv/tools}
  rwdisk:
%s    mounts:
      - {source: %s, target: /srv/seed}
    disks:
      - {source: %s, target: /srv/store, read_write: true}
      - {source: %s, target: /srv/tools}
`, shape, seed, cache, tools, shape, seed, store, tools))
	img := storageServiceImage(t, "storage-service")

	for _, tc := range []struct{ profile, rw string }{{"rwmount", "/srv/cache"}, {"rwdisk", "/srv/store"}} {
		label := "microvm-" + tc.profile
		log, err := runWorkflowOn(t, p, writeWorkflow(t, storageJob(label, img, fmt.Sprintf(`
ask() { echo "$1" | nc -w 5 store 8000; }
echo "the service reads seed=$(ask /srv/seed/hello) tools=$(ask /srv/tools/marker)"
echo "the service reports: $(ask /tmp/report | tr '\n' ' ')"
[ "$(ask /srv/seed/hello)" = seed ]
[ "$(ask /srv/tools/marker)" = tools ]
[ "$(ask /tmp/report)" = "storage ok" ]
[ "$(cat /srv/seed/hello)" = seed ]
[ "$(cat /srv/tools/marker)" = tools ]
echo job > %[1]s/from-job
sync
[ "$(cat %[1]s/from-job)" = job ]
echo "storage ok: $((6 * 7))"
`, tc.rw))), map[string]string{label: readImage(t) + ";profile=" + tc.profile})
		if err != nil {
			t.Fatalf("%s: the workflow failed: %v", tc.profile, err)
		}
		if !strings.Contains(log, "storage ok: 42") {
			t.Errorf("%s: the job log does not show the step's success", tc.profile)
		}
	}
	if b, err := os.ReadFile(filepath.Join(cache, "from-job")); err != nil || string(b) != "job\n" {
		t.Errorf("the job's container's write to its read-write mount: %q, %v", b, err)
	}
	if entries, err := os.ReadDir(seed); err != nil || len(entries) != 1 {
		t.Errorf("the guest changed %s: %v, %v", seed, entries, err)
	}
	if !bytes.Equal(fileBytes(t, tools), toolsBefore) {
		t.Errorf("the guest changed %s", tools)
	}

	// The service's image has a file at /srv/seed; the job's container's
	// doesn't.
	blocked := storageServiceImage(t, "storage-blocked", &tar.Header{Name: "srv/seed", Typeflag: tar.TypeReg, Mode: 0o644})
	log, err := runWorkflowOn(t, p, writeWorkflow(t, storageJob("microvm-rwmount", blocked, `echo "a step ran"`)),
		map[string]string{"microvm-rwmount": readImage(t) + ";profile=rwmount"})
	if err == nil {
		t.Fatal("the workflow with a service whose image has a file at a mount's target succeeded")
	}
	for _, want := range []string{"VM store exited during boot", "mount /srv/seed: /srv/seed is not a directory in the image"} {
		if !strings.Contains(log, want) && !strings.Contains(err.Error(), want) {
			t.Errorf("neither the job log nor the error (%v) shows %q", err, want)
		}
	}
	if strings.Contains(log, "a step ran") {
		t.Error("a step ran beside a service that failed to boot")
	}
}
