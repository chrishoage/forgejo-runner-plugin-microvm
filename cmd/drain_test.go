// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	pluginv1alpha "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// drainBound is how soon after the last Remove, or a second SIGTERM, the
// plugin must have exited.
const drainBound = 5 * time.Second

// drainedJob is a started job on p's plugin, which the test removes itself,
// or the plugin's second signal does: no cleanup removes it.
type drainedJob struct {
	*vm
	health healthv1.HealthClient
	// pid is its job process's.
	pid int
}

// startDrainedJob creates and starts a job on p's plugin with the image
// e2e/image pins.
func startDrainedJob(t *testing.T, p *plugin) *drainedJob {
	t.Helper()
	conn, err := grpclib.NewClient("unix://"+p.sock, grpclib.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := pluginv1alpha.NewBackendPluginClient(conn)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	resp, err := c.Create(ctx, &pluginv1alpha.CreateRequest{Name: "drain", LabelArg: readImage(t)})
	if err != nil {
		t.Fatal(err)
	}
	j := &drainedJob{vm: &vm{c: c, id: resp.GetEnvironmentId()}, health: healthv1.NewHealthClient(conn)}
	if err := j.start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m := jobStarted.FindStringSubmatch(p.log.String())
	if m == nil {
		t.Fatalf("the plugin's log names no job process:\n%s", p.log.String())
	}
	if j.pid, err = strconv.Atoi(m[1]); err != nil {
		t.Fatal(err)
	}
	return j
}

// sigterm sends p SIGTERM and waits until its health says NOT_SERVING, so
// that a second SIGTERM is never merged with the first.
func (j *drainedJob) sigterm(t *testing.T, p *plugin) {
	t.Helper()
	if err := syscall.Kill(p.pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		resp, err := j.health.Check(ctx, &healthv1.HealthCheckRequest{})
		cancel()
		if err != nil {
			t.Fatalf("health during the drain: %v", err)
		}
		if resp.GetStatus() == healthv1.HealthCheckResponse_NOT_SERVING {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("health still %v 10 s after SIGTERM", resp.GetStatus())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// wantExited0 waits up to drainBound for p to exit, and fails t unless it
// exited 0.
func wantExited0(t *testing.T, p *plugin, after string) {
	t.Helper()
	select {
	case <-p.exited:
	case <-time.After(drainBound):
		t.Fatalf("the plugin still runs %v after %s:\n%s", drainBound, after, p.log.String())
	}
	if ws := p.exitStatus(t); ws.Signaled() {
		t.Fatalf("the plugin died of %v after %s:\n%s", ws.Signal(), after, p.log.String())
	} else if ws.ExitStatus() != 0 {
		t.Fatalf("the plugin exited %d after %s:\n%s", ws.ExitStatus(), after, p.log.String())
	}
}

// wantNoJobFiles fails t if any per-job file is left in p's directories:
// <state>/jobs/, <runtime>/ (but its lock, and the listen socket when
// listening, since a clean exit deletes it) and <cache>/jobs/.
func wantNoJobFiles(t *testing.T, p *plugin, listening bool) {
	t.Helper()
	base := filepath.Dir(p.state)
	runtime := filepath.Join(base, "runtime")
	for _, dir := range []string{filepath.Join(p.state, "jobs"), runtime, filepath.Join(base, "cache", "jobs")} {
		es, err := os.ReadDir(dir)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		for _, e := range es {
			if e.Name() != "lock" && (!listening || filepath.Join(dir, e.Name()) != p.sock) {
				t.Errorf("%s is left in %s", e.Name(), dir)
			}
		}
	}
}

// With a job started, SIGTERM drains the plugin: health says NOT_SERVING, a
// new Create fails with Unavailable, and an Exec on the job still succeeds;
// once the job is removed, the plugin exits 0 within 5 s and leaves no job
// files.
func TestDrain(t *testing.T) {
	t.Parallel()
	p := startPlugin(t, buildPlugin(t))
	j := startDrainedJob(t, p)

	j.sigterm(t, p)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	_, err := j.c.Create(ctx, &pluginv1alpha.CreateRequest{Name: "late", LabelArg: readImage(t)})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("Create during the drain: %v, want Unavailable", err)
	}
	if code, out := j.exec(t, "echo", "still served"); code != 0 || out != "still served\n" {
		t.Errorf("Exec during the drain: exit %d, %q", code, out)
	}
	select {
	case <-p.exited:
		t.Fatalf("the plugin exited with a job left:\n%s", p.log.String())
	default:
	}
	if _, err := j.c.Remove(ctx, &pluginv1alpha.RemoveRequest{EnvironmentId: j.id}); err != nil {
		t.Fatalf("Remove during the drain: %v", err)
	}
	wantExited0(t, p, "the last Remove")
	wantNoJobFiles(t, p, false)
}

// With a job started, a second SIGTERM ends the drain at once: within 5 s
// the job process is gone, the plugin has exited 0, and no job files are
// left.
func TestSecondSIGTERM(t *testing.T) {
	t.Parallel()
	p := startPlugin(t, buildPlugin(t))
	j := startDrainedJob(t, p)
	jobProc, _, ok := readProc(j.pid)
	if !ok {
		t.Fatalf("no job process %d", j.pid)
	}

	j.sigterm(t, p)
	if err := syscall.Kill(p.pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	sent := time.Now()
	wantExited0(t, p, "a second SIGTERM")
	// Reaped: not even a zombie is left, within the same bound.
	for jobProc.alive() {
		if time.Since(sent) > drainBound {
			t.Fatalf("job process %d still exists %v after a second SIGTERM", j.pid, drainBound)
		}
		time.Sleep(10 * time.Millisecond)
	}
	wantNoJobFiles(t, p, false)
}
