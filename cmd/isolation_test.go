// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolatedProfile is the isolation test's profile: small's shape, 256 MiB,
// with the test's handshake directory, read-write, at /handshake in each
// job's container.
const isolatedProfile = "isolated"

// isolatedJobs is a workflow job run twice at once, as a matrix of first
// and second, each with a redis service from e2e/redis-image on the
// isolated profile: four VMs. Each sets a key of its own in its service and
// checks that it reads it back, then signals that it is ready with a file
// in /handshake, which both jobs' containers share, and waits, at most
// 120 s, for the other job's file. Only then does it ask its service, by
// name and at the service's address, which is the same in both jobs,
// whether each key exists, and fail unless only its own does: by then the
// other job's key is set, in the other job's service.
const isolatedJobs = `  isolated:
    strategy:
      matrix:
        own: [first, second]
    runs-on: microvm-isolated
    services:
      redis:
        image: ${{ vars.REDIS_IMAGE }}
    steps:
      - shell: sh -e {0}
        run: |
          own=${{ matrix.own }}
          if [ "$own" = first ]; then other=second; else other=first; fi
          # The first line of redis's reply to one command, sent to host.
          ask() { printf '%s\r\nQUIT\r\n' "$2" | nc -w 5 "$1" 6379 | tr -d '\r' | head -n 1; }
          for i in $(seq 50); do [ "$(ask redis PING)" = +PONG ] && break; sleep 0.2; done
          [ "$(ask redis "SET key-$own $own")" = +OK ]
          [ "$(ask redis "EXISTS key-$own")" = :1 ]
          touch "/handshake/ready-$own"
          deadline=$(($(date +%s) + 120))
          until [ -e "/handshake/ready-$other" ]; do
            if [ "$(date +%s)" -ge "$deadline" ]; then echo "job $own: job $other never got ready"; exit 1; fi
            sleep 0.2
          done
          echo "job $own: job $other is ready"
          for host in redis 198.18.0.3; do
            mine=$(ask "$host" "EXISTS key-$own")
            theirs=$(ask "$host" "EXISTS key-$other")
            echo "job $own at $host: own=$mine other=$theirs"
            [ "$mine" = :1 ] && [ "$theirs" = :0 ]
          done
`

// TestServicesAreTheirJobsOwn is the isolation protocol test: two jobs run
// at once, each with a redis service, through forgejo-runner's own job
// executor. Each job's step sees its own key in its own service, by the
// service's name and at its address, and never the other job's, though
// both services have the same name and address, each on its own job
// network. Each job reads only once the other has signalled that its key
// is set, through a host directory both jobs mount, so the reads prove
// the two jobs overlapped.
func TestServicesAreTheirJobsOwn(t *testing.T) {
	t.Parallel()
	handshake := t.TempDir()
	p := startPluginWith(t, buildPlugin(t), fmt.Sprintf(`profiles:
  %s:
    cpus: 1
    memory_mib: 256
    scratch_gib: 2
    mounts:
      - {source: %s, target: /handshake, read_write: true}
`, isolatedProfile, handshake))
	labels := map[string]string{"microvm-isolated": readImage(t) + ";profile=" + isolatedProfile}
	log, err := runWorkflowOn(t, p, writeWorkflow(t, isolatedJobs), labels)
	if err != nil {
		t.Fatalf("the workflow failed: %v", err)
	}
	for _, own := range []string{"first", "second"} {
		other := map[string]string{"first": "second", "second": "first"}[own]
		if want := fmt.Sprintf("job %s: job %s is ready", own, other); !strings.Contains(log, want) {
			t.Errorf("the job log does not show %q", want)
		}
		for _, host := range []string{"redis", "198.18.0.3"} {
			if want := fmt.Sprintf("job %s at %s: own=:1 other=:0", own, host); !strings.Contains(log, want) {
				t.Errorf("the job log does not show %q", want)
			}
		}
		if _, err := os.Stat(filepath.Join(handshake, "ready-"+own)); err != nil {
			t.Errorf("job %s's ready file: %v", own, err)
		}
	}
}
