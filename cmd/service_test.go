// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
)

// withService gives VM i of spec the service svc, in its boot config and in
// the boot config file of its boot root.
func withService(t *testing.T, spec *jobv1.Spec, i int, svc *agentv1.Service) {
	t.Helper()
	v := spec.GetVms()[i]
	v.GetBoot().Service = svc
	cfg, err := proto.Marshal(v.GetBoot())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v.GetRootDir(), vmsAgentDir, "boot-config.binpb"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A VM whose boot config carries a service command runs it once
// StartService asks, here a small listener from the test image, on the
// built job binary: once the agent answers, StartService through that VM's
// agent socket starts it with the service's environment, the boot config
// holding none; an Exec through the same socket reaches the service on its
// port over loopback, and the service's output, its environment's value
// included, is in the VM's console; when the command exits, the VM stops
// and its VMExited is reported with the command's exit code, the job
// process exiting with it.
func TestVMRunsAService(t *testing.T) {
	t.Parallel()
	spec := vmsSpec(t, "service")
	// The service takes one connection on port 7000, then one on 7001,
	// and exits 3.
	withService(t, spec, 0, &agentv1.Service{
		Argv: []string{"sh", "-c", `echo "service $GREETING listening"; nc -l -p 7000; echo "service stopping"; nc -l -p 7001; exit 3`},
	})
	j := launchVMs(t, spec)

	ctx, cancel := context.WithTimeout(t.Context(), vmsEventBound)
	defer cancel()
	if err := j.agents[0].StartService(ctx, map[string]string{"GREETING": "hello-from-env"}); err != nil {
		t.Fatalf("StartService: %v", err)
	}
	// StartService returns once the command has started; nc may not be
	// listening yet.
	send := `for i in $(seq 100); do echo "$1" | nc -w 5 127.0.0.1 "$2" && exit 0; sleep 0.1; done; exit 1`
	if code, out, err := j.exec(ctx, 0, "sh", "-c", send, "sh", "a line for the service", "7000"); err != nil || code != 0 {
		t.Fatalf("Exec to the service: exit code %d, %v:\n%s", code, err, out)
	}
	// The service exits once this line arrives, and the VM stops with it,
	// so the Exec may end before its exit code comes back.
	if code, out, err := j.exec(ctx, 0, "sh", "-c", send, "sh", "a line to stop it", "7001"); err == nil && code != 0 {
		t.Fatalf("Exec to stop the service: exit code %d:\n%s", code, out)
	}

	x := j.next(t).GetVmExited()
	if x == nil || x.GetVm() != 0 {
		t.Fatalf("event = %v, want VM 0's VMExited", x)
	}
	if x.GetExitCode() != 3 {
		t.Errorf("VMExited's exit code = %d, want the service's 3", x.GetExitCode())
	}
	select {
	case <-j.proc.Exited:
	case <-time.After(vmsEventBound):
		t.Fatalf("the job process still runs %v after its VM stopped", vmsEventBound)
	}
	b, err := os.ReadFile(spec.GetVms()[0].GetConsole())
	if err != nil {
		t.Fatal(err)
	}
	console := string(b)
	for _, want := range []string{"service hello-from-env listening", "a line for the service", "service stopping", "a line to stop it", `msg="service exited"`} {
		if !strings.Contains(console, want) {
			t.Errorf("the console lacks %q:\n%s", want, console)
		}
	}
}
