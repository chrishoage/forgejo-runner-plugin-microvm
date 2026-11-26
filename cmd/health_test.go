// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// healthProfile is the health check tests' profile: small's shape, 256 MiB,
// so a job and its one service are two small VMs.
const healthProfile = "health"

// serviceImage returns the test image, served from the in-process registry
// as repo, whose command is cmd, run through sh -c, and whose HEALTHCHECK is
// hc.
func serviceImage(t *testing.T, repo, cmd string, hc *v1.HealthConfig) string {
	t.Helper()
	base, host := baseImage(t)
	cfg, err := base.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Config
	c.Entrypoint, c.Cmd, c.Healthcheck = nil, []string{"sh", "-c", cmd}, hc
	img, err := mutate.Config(base, c)
	if err != nil {
		t.Fatal(err)
	}
	return pushImage(t, host, repo, img)
}

// healthJob is a workflow job on the health profile with the service name
// from image, whose step runs script.
func healthJob(name, image, script string) string {
	return fmt.Sprintf(`  health:
    runs-on: microvm-health
    services:
      %s:
        image: %s
    steps:
      - shell: sh -e {0}
        run: |
          %s
`, name, image, strings.ReplaceAll(strings.TrimSpace(script), "\n", "\n          "))
}

// Through forgejo-runner's own job executor, a service built on the test
// image with a HEALTHCHECK that passes only once a file appears makes Start
// wait until it does: the file appears 3 s after the service starts, with a
// listener on port 7000, and the job's step reaches it at once, with no
// retry. A service whose check always fails fails Start, naming it, with
// the check's output. One job at a time: at most two VMs.
func TestStartWaitsForAServicesHealthCheck(t *testing.T) {
	t.Parallel()
	p := startPluginWith(t, buildPlugin(t), fmt.Sprintf("profiles:\n  %s:\n    cpus: 1\n    memory_mib: 256\n    scratch_gib: 2\n", healthProfile))
	labels := map[string]string{"microvm-health": readImage(t) + ";profile=" + healthProfile}

	// It checks every 500 ms; with no start period, its failures count,
	// so retries leave it room.
	slow := serviceImage(t, "slow", `sleep 3
(while true; do echo up | nc -l -p 7000; done) &
sleep 0.2
echo "service: ready"
touch /tmp/ready
wait`, &v1.HealthConfig{Test: []string{"CMD-SHELL", "test -e /tmp/ready"}, Interval: 500 * time.Millisecond, Retries: 100})
	log, err := runWorkflowOn(t, p, writeWorkflow(t, healthJob("slow", slow, `out=$(nc -w 2 slow 7000 </dev/null)
echo "the service says $out"
[ "$out" = up ]`)), labels)
	if err != nil {
		t.Fatalf("the workflow with a healthy service failed: %v", err)
	}
	const at = `\[runner-microvm \d+\.\d\ds\] `
	healthy := regexp.MustCompile(at + `waiting for service slow to be healthy(?s:.*)` + at + `service slow healthy after (\d+\.\d\d)s(?s:.*)the service says up`)
	m := healthy.FindStringSubmatch(log)
	if m == nil {
		t.Fatalf("the job log does not show the wait for the service's health, then the step reaching it:\n%s", log)
	}
	// The file appears 3 s after the service starts, which its
	// StartService does just before the wait begins.
	if waited, _ := strconv.ParseFloat(m[1], 64); waited < 2 {
		t.Errorf("Start waited %ss for the service's health, want at least the 2 s left until the file appears", m[1])
	}

	sick := serviceImage(t, "sick", "exec sleep 3600", &v1.HealthConfig{
		Test: []string{"CMD-SHELL", "echo the service is not ok; exit 1"}, Interval: 200 * time.Millisecond, Retries: 2,
	})
	log, err = runWorkflowOn(t, p, writeWorkflow(t, healthJob("sick", sick, `echo "a step ran"`)), labels)
	if err == nil {
		t.Fatal("the workflow with an unhealthy service succeeded")
	}
	for _, want := range []string{"service sick is unhealthy", "the service is not ok"} {
		if !strings.Contains(log, want) && !strings.Contains(err.Error(), want) {
			t.Errorf("neither the job log nor the error (%v) shows %q", err, want)
		}
	}
	if strings.Contains(log, "a step ran") {
		t.Error("a step ran beside an unhealthy service")
	}
}
