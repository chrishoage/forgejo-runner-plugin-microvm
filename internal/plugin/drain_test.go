// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/config"
	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
)

// drainFixture is a server with fakes for its seams, served on a listen
// socket by serveWithReloads as run serves it, with channels standing in for
// the signals serve passes on.
type drainFixture struct {
	*reloadFixture
	c      pluginv1alpha.BackendPluginClient
	health healthv1.HealthClient
	// term carries SIGTERM and SIGINT, hup SIGHUP.
	term, hup chan os.Signal
	// served closes once serveWithReloads has returned, with err its
	// result.
	served chan struct{}
	err    error
}

func newDrainFixture(t *testing.T) *drainFixture {
	t.Helper()
	f := &drainFixture{
		reloadFixture: newReloadFixture(t, "profiles:\n  big: {cpus: 3}\n"),
		term:          make(chan os.Signal, 2),
		hup:           make(chan os.Signal, 1),
		served:        make(chan struct{}),
	}
	// The server's own lines, such as each removal's, in the same log.
	f.srv.log = f.log
	dir := t.TempDir()
	lis, closeLis, err := listen(t.Context(), filepath.Join(dir, "plugin.sock"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := openSockDir(dir)
	if err != nil {
		closeLis()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(f.served)
		f.err = serveWithReloads(ctx, f.log, lis, f.srv, f.hup, f.term, f.cfg, (*config.Config).Reload)
	}()
	t.Cleanup(func() {
		cancel()
		<-f.served
		closeLis()
		_ = d.Close()
	})
	conn, err := grpc.NewClient("unix://"+d.path("plugin.sock"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	f.c = pluginv1alpha.NewBackendPluginClient(conn)
	f.health = healthv1.NewHealthClient(conn)
	return f
}

// started creates and starts a job, and returns its environment id.
func (f *drainFixture) started(t *testing.T) string {
	t.Helper()
	id := create(t, f.c)
	if _, err := start(testContext(t), f.c, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// remove removes the job id, which must succeed.
func (f *drainFixture) remove(t *testing.T, id string) {
	t.Helper()
	if _, err := f.c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatalf("Remove %s: %v", id, err)
	}
}

// signal sends sig as serve passes SIGTERM and SIGINT on.
func (f *drainFixture) signal(sig os.Signal) { f.term <- sig }

// healthOf returns the health service's status for service.
func (f *drainFixture) healthOf(t *testing.T, service string) healthv1.HealthCheckResponse_ServingStatus {
	t.Helper()
	resp, err := f.health.Check(testContext(t), &healthv1.HealthCheckRequest{Service: service})
	if err != nil {
		t.Fatalf("health of %q: %v", service, err)
	}
	return resp.GetStatus()
}

// draining sends sig and waits until health says NOT_SERVING.
func (f *drainFixture) draining(t *testing.T, sig os.Signal) {
	t.Helper()
	f.signal(sig)
	waitFor(t, "the drain to begin", func() bool { return f.healthOf(t, "") == healthv1.HealthCheckResponse_NOT_SERVING })
}

// wantServed waits for serveWithReloads to return, and fails t unless it
// returned nil.
func (f *drainFixture) wantServed(t *testing.T) {
	t.Helper()
	select {
	case <-f.served:
		if f.err != nil {
			t.Fatalf("serveWithReloads: %v", f.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveWithReloads did not return")
	}
}

// wantServing fails t if serveWithReloads has returned.
func (f *drainFixture) wantServing(t *testing.T, when string) {
	t.Helper()
	select {
	case <-f.served:
		t.Fatalf("serveWithReloads returned %s: %v", when, f.err)
	default:
	}
}

// backendService is the BackendPlugin service's name, which the health
// service reports on beside the overall "".
var backendService = pluginv1alpha.BackendPlugin_ServiceDesc.ServiceName

// The health service says SERVING for "" and for the BackendPlugin service
// until the drain begins, then NOT_SERVING for both.
func TestHealthDuringADrain(t *testing.T) {
	t.Parallel()
	f := newDrainFixture(t)
	id := f.started(t)
	for _, svc := range []string{"", backendService} {
		if got := f.healthOf(t, svc); got != healthv1.HealthCheckResponse_SERVING {
			t.Errorf("before the drain, health of %q is %v, want SERVING", svc, got)
		}
	}

	f.draining(t, syscall.SIGTERM)
	for _, svc := range []string{"", backendService} {
		if got := f.healthOf(t, svc); got != healthv1.HealthCheckResponse_NOT_SERVING {
			t.Errorf("during the drain, health of %q is %v, want NOT_SERVING", svc, got)
		}
	}
	f.remove(t, id)
	f.wantServed(t)
}

// During a drain a new Create fails with Unavailable, before any check of
// its request; every job created before it is served: Start, Exec and
// Remove succeed.
func TestADrainRefusesCreatesAndServesItsJobs(t *testing.T) {
	t.Parallel()
	for _, sig := range []os.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			t.Parallel()
			f := newDrainFixture(t)
			running := f.started(t)
			created := create(t, f.c)

			f.draining(t, sig)
			for _, req := range []*pluginv1alpha.CreateRequest{{LabelArg: testImage}, {LabelArg: testImage + ";bogus"}} {
				_, err := f.c.Create(testContext(t), req)
				if status.Code(err) != codes.Unavailable || status.Convert(err).Message() != "runner-microvm is shutting down" {
					t.Errorf("Create %q during the drain: %v, want Unavailable: runner-microvm is shutting down", req.GetLabelArg(), err)
				}
			}
			outs, err := execAll(t, f.c, &pluginv1alpha.ExecRequest{EnvironmentId: running, Command: []string{"true"}})
			if err != nil {
				t.Errorf("Exec during the drain: %v", err)
			}
			if len(outs) == 0 || outs[len(outs)-1].GetExecComplete() == nil {
				t.Errorf("Exec during the drain did not complete: %v", outs)
			}
			if _, err := start(testContext(t), f.c, created); err != nil {
				t.Errorf("Start during the drain of a job created before it: %v", err)
			}
			f.remove(t, running)
			f.remove(t, created)
			f.wantServed(t)
			if n := f.launcher.alive(); n != 0 {
				t.Errorf("%d job processes left", n)
			}
		})
	}
}

// A drain ends once the last job is removed, and not before: a drain with
// jobs left keeps serving.
func TestADrainEndsWithTheLastJob(t *testing.T) {
	t.Parallel()
	f := newDrainFixture(t)
	first, last := f.started(t), f.started(t)
	f.draining(t, syscall.SIGTERM)

	f.remove(t, first)
	f.wantServing(t, "with a job left")
	select {
	case <-f.srv.Drained():
		t.Fatal("the drain ended with a job left")
	default:
	}
	if got := f.healthOf(t, ""); got != healthv1.HealthCheckResponse_NOT_SERVING {
		t.Errorf("health %v with a job left, want NOT_SERVING", got)
	}
	f.remove(t, last)
	f.wantServed(t)
	if log := f.buf.String(); !strings.Contains(log, `msg="drained: no job is left; stopping"`) {
		t.Errorf("the log does not say the drain ended:\n%s", log)
	}
}

// A drain with no job ends at once.
func TestADrainWithNoJobEndsAtOnce(t *testing.T) {
	t.Parallel()
	f := newDrainFixture(t)
	f.signal(syscall.SIGTERM)
	f.wantServed(t)
}

// A job the backstop removes during a drain ends it as a Remove does.
func TestTheBackstopEndsADrain(t *testing.T) {
	t.Parallel()
	f := newDrainFixture(t)
	f.srv.cfg.BackstopDelay = time.Millisecond
	resp, err := f.c.Create(testContext(t), &pluginv1alpha.CreateRequest{LabelArg: testImage, EnvironmentTimeout: durationpb.New(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	f.draining(t, syscall.SIGTERM)
	if !inTable(f.srv, resp.GetEnvironmentId()) {
		t.Fatal("the backstop fired before the drain began")
	}
	f.wantServed(t)
	if inTable(f.srv, resp.GetEnvironmentId()) {
		t.Error("the job is still in the table")
	}
}

// A second signal ends the drain at once: every job is removed as Remove
// removes it, its job process killed and its files deleted, and serving
// returns nil.
func TestASecondSignalRemovesEveryJob(t *testing.T) {
	t.Parallel()
	f := newDrainFixture(t)
	ids := []string{f.started(t), f.started(t), create(t, f.c)}
	f.draining(t, syscall.SIGTERM)
	f.wantServing(t, "after one signal, with jobs left")

	f.signal(syscall.SIGINT)
	f.wantServed(t)
	if n := f.launcher.alive(); n != 0 {
		t.Errorf("%d job processes left", n)
	}
	log := f.buf.String()
	if !strings.Contains(log, `msg="a second signal: removing every job"`) {
		t.Errorf("the log does not name the second signal:\n%s", log)
	}
	for _, id := range ids {
		if files := jobFiles(t, f.srv, id); len(files) != 0 {
			t.Errorf("job %s left %v", id, files)
		}
		if !strings.Contains(log, fmt.Sprintf(`msg=removed env=%s err=<nil>`, id)) {
			t.Errorf("job %s was not removed as Remove removes it:\n%s", id, log)
		}
	}
}

// A second signal stops serving, which doesn't wait for a Remove still
// tearing its job down; Close then waits for it, so the plugin exits, and
// releases its locks, only once the job's files are gone.
func TestCloseDuringADrainWaitsForARemoveInFlight(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cache := &fakeCache{releasing: make(chan string), unrelease: make(chan struct{})}
		srv := newTestServer(t, cache, &fakeLauncher{})
		// A job never started has no layer link directory, but the cache
		// directory is there, for jobFiles.
		if err := os.MkdirAll(srv.cfg.Dirs.Cache, 0o700); err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Create(t.Context(), &pluginv1alpha.CreateRequest{LabelArg: testImage})
		if err != nil {
			t.Fatal(err)
		}
		id := resp.GetEnvironmentId()
		srv.Drain()
		removed := make(chan error)
		go func() {
			_, err := srv.Remove(t.Context(), &pluginv1alpha.RemoveRequest{EnvironmentId: id})
			removed <- err
		}()
		<-cache.releasing

		// The second signal: serving stops, then Close.
		closed := make(chan error, 1)
		go func() { closed <- srv.Close(context.Background()) }()
		synctest.Wait()
		select {
		case err := <-closed:
			t.Errorf("Close returned while a Remove was still removing its job: %v", err)
			closed <- err // for the checks below
		default:
		}

		close(cache.unrelease)
		if err := <-removed; err != nil {
			t.Errorf("Remove: %v", err)
		}
		if err := <-closed; err != nil {
			t.Errorf("Close: %v", err)
		}
		if files := jobFiles(t, srv, id); len(files) != 0 {
			t.Errorf("job %s left %v", id, files)
		}
	})
}

// SIGHUP still reloads during a drain.
func TestAReloadDuringADrain(t *testing.T) {
	t.Parallel()
	f := newDrainFixture(t)
	id := f.started(t)
	f.draining(t, syscall.SIGTERM)

	writeFile(t, f.path, "profiles:\n  big: {cpus: 5}\n")
	f.hup <- syscall.SIGHUP
	waitFor(t, "the reload", func() bool { p, _ := profilesOf(f.srv).Lookup("big"); return p.CPUs == 5 })
	f.remove(t, id)
	f.wantServed(t)
}

// Creates racing a drain either succeed, with a job the drain then waits
// for, or fail with Unavailable; none is lost between the two.
func TestCreatesRacingADrain(t *testing.T) {
	t.Parallel()
	f := newDrainFixture(t)
	keep := create(t, f.c)
	var (
		mu  sync.Mutex
		ids []string
		wg  sync.WaitGroup
	)
	for range 8 {
		wg.Go(func() {
			for range 20 {
				resp, err := f.srv.Create(testContext(t), &pluginv1alpha.CreateRequest{LabelArg: testImage})
				if status.Code(err) == codes.Unavailable {
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				ids = append(ids, resp.GetEnvironmentId())
				mu.Unlock()
			}
		})
	}
	f.signal(syscall.SIGTERM)
	wg.Wait()
	for _, id := range append(ids, keep) {
		f.wantServing(t, "with jobs left")
		f.remove(t, id)
	}
	f.wantServed(t)
}
