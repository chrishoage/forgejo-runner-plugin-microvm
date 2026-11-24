// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"log/slog"
	"math"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"

	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
)

// fired is how the plugin's log says the backstop removed a job.
const fired = "the runner has not removed the job within its environment timeout"

// A job the runner never removes is removed at its environment timeout plus
// the backstop delay, and not before; the plugin logs that it fired, and a
// later Remove gets NotFound (PROTO-14).
func TestBackstopRemovesAForgottenJob(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cache := &fakeCache{}
		srv := newTestServer(t, cache, &fakeLauncher{})
		log := &logBuffer{}
		srv.log = slog.New(slog.NewTextHandler(log, nil))
		resp, err := srv.Create(t.Context(), &pluginv1alpha.CreateRequest{
			LabelArg: testImage, EnvironmentTimeout: durationpb.New(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		id := resp.GetEnvironmentId()

		time.Sleep(time.Hour + 30*time.Second - time.Nanosecond)
		synctest.Wait()
		if !inTable(srv, id) || strings.Contains(log.String(), fired) {
			t.Fatal("the backstop fired before the timeout plus 30 s")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if inTable(srv, id) {
			t.Fatal("the backstop did not remove the job at the timeout plus 30 s")
		}
		if !strings.Contains(log.String(), fired) || !strings.Contains(log.String(), "env="+id) {
			t.Errorf("the log does not say the backstop removed %s:\n%s", id, log)
		}
		if !slices.Equal(cache.released, []string{id}) {
			t.Errorf("released %v, want the job once, as Remove does", cache.released)
		}
		_, err = srv.Remove(t.Context(), &pluginv1alpha.RemoveRequest{EnvironmentId: id})
		wantCode(t, err, codes.NotFound)
	})
}

// No environment timeout, or a zero one, sets no backstop; and Remove stops
// a job's backstop.
func TestBackstopDoesNotFire(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cache := &fakeCache{}
		srv := newTestServer(t, cache, &fakeLauncher{})
		log := &logBuffer{}
		srv.log = slog.New(slog.NewTextHandler(log, nil))
		var kept []string
		for _, timeout := range []*durationpb.Duration{nil, durationpb.New(0)} {
			resp, err := srv.Create(t.Context(), &pluginv1alpha.CreateRequest{LabelArg: testImage, EnvironmentTimeout: timeout})
			if err != nil {
				t.Fatal(err)
			}
			kept = append(kept, resp.GetEnvironmentId())
		}
		resp, err := srv.Create(t.Context(), &pluginv1alpha.CreateRequest{
			LabelArg: testImage, EnvironmentTimeout: durationpb.New(time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
		removed := resp.GetEnvironmentId()
		if _, err := srv.Remove(t.Context(), &pluginv1alpha.RemoveRequest{EnvironmentId: removed}); err != nil {
			t.Fatal(err)
		}

		time.Sleep(1000 * time.Hour)
		synctest.Wait()
		for _, id := range kept {
			if !inTable(srv, id) {
				t.Errorf("a job with no environment timeout was removed")
			}
		}
		if strings.Contains(log.String(), fired) {
			t.Errorf("a backstop fired:\n%s", log)
		}
		if !slices.Equal(cache.released, []string{removed}) {
			t.Errorf("released %v, want only the job Remove removed, once", cache.released)
		}
	})
}

// An environment timeout too large to add the backstop delay to waits as
// long as a timer can, rather than overflowing and firing at once.
func TestBackstopOfAHugeTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		srv := newTestServer(t, &fakeCache{}, &fakeLauncher{})
		// AsDuration saturates this at the largest time.Duration.
		resp, err := srv.Create(t.Context(), &pluginv1alpha.CreateRequest{
			LabelArg: testImage, EnvironmentTimeout: &durationpb.Duration{Seconds: math.MaxInt64},
		})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(1000 * time.Hour)
		synctest.Wait()
		if !inTable(srv, resp.GetEnvironmentId()) {
			t.Fatal("the backstop of a huge environment timeout fired")
		}
	})
}

// The backstop removes a started job as Remove does: its job process ends
// and its files go.
func TestBackstopRemovesAStartedJob(t *testing.T) {
	t.Parallel()
	launcher := &fakeLauncher{}
	srv := newTestServer(t, &fakeCache{}, launcher)
	srv.cfg.BackstopDelay = 100 * time.Millisecond
	log := &logBuffer{}
	srv.log = slog.New(slog.NewTextHandler(log, nil))
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	const timeout = 2 * time.Second
	begin := time.Now()
	resp, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{LabelArg: testImage, EnvironmentTimeout: durationpb.New(timeout)})
	if err != nil {
		t.Fatal(err)
	}
	id := resp.GetEnvironmentId()
	if _, err := start(testContext(t), c, id); err != nil {
		t.Fatal(err)
	}
	// The job leaves the table before its teardown begins, so only the
	// removal's own log line says its files are gone; walking them sooner
	// races their deletion.
	removed := "msg=removed env=" + id + " "
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(log.String(), removed) {
		if time.Now().After(deadline) {
			t.Fatalf("the backstop has not removed the job 20 s on:\n%s", log)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if took := time.Since(begin); took < timeout+srv.cfg.BackstopDelay {
		t.Errorf("the backstop fired after %v, before the timeout plus the delay", took)
	}
	if !strings.Contains(log.String(), removed+"err=<nil>") {
		t.Errorf("the backstop's removal failed:\n%s", log)
	}
	if inTable(srv, id) {
		t.Error("the backstop left the job in the table")
	}
	if n := launcher.alive(); n != 0 {
		t.Errorf("the backstop left %d job processes running", n)
	}
	if left := jobFiles(t, srv, id); len(left) != 0 {
		t.Errorf("the backstop left the job's files %v", left)
	}
	_, err = c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id})
	wantCode(t, err, codes.NotFound)
}
