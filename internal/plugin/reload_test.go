// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/config"
	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
)

// logBuffer is a log several goroutines may write, and the test read.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// reloadFixture is a config file loaded as serve loads it, and a server
// with fakes for its seams, given the file's profiles.
type reloadFixture struct {
	path     string
	cfg      *config.Config
	srv      *Server
	launcher *fakeLauncher
	buf      *logBuffer
	log      *slog.Logger
}

func newReloadFixture(t *testing.T, content string) *reloadFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, content)
	cfg, err := config.Load(func(k string) string {
		if k == config.PathEnv {
			return path
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	launcher := &fakeLauncher{}
	srv := newTestServer(t, &fakeCache{}, launcher)
	srv.SetProfiles(cfg.Profiles)
	buf := &logBuffer{}
	return &reloadFixture{path, cfg, srv, launcher, buf, slog.New(slog.NewTextHandler(buf, nil))}
}

// writeFile replaces path with content in one step, as an editor's save
// through a rename does, so a reload never reads half a file.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// createOn creates a job on srv with label arg, and returns its id.
func createOn(t *testing.T, srv *Server, arg string) string {
	t.Helper()
	resp, err := srv.Create(testContext(t), &pluginv1alpha.CreateRequest{LabelArg: arg})
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetEnvironmentId()
}

// cpusOfSpec returns the CPU count of the VM in the launcher's n-th spec.
func cpusOfSpec(t *testing.T, l *fakeLauncher, n int) uint32 {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.specs) <= n {
		t.Fatalf("%d job processes launched, want at least %d", len(l.specs), n+1)
	}
	return l.specs[n].GetVms()[0].GetCpus()
}

const bigLabel = testImage + ";profile=big"

// After a reload that changes a profile, the next Create gets the new
// values; a job created before keeps its copy, whether it was started
// before the reload or after (CFG-7).
func TestReloadGivesLaterJobsTheNewProfile(t *testing.T) {
	t.Parallel()
	f := newReloadFixture(t, "profiles:\n  big: {cpus: 3, memory_mib: 1536, scratch_gib: 4}\n")
	c := pluginv1alpha.NewBackendPluginClient(serve(t, f.srv))
	running := createOn(t, f.srv, bigLabel)
	if _, err := start(testContext(t), c, running); err != nil {
		t.Fatal(err)
	}
	created := createOn(t, f.srv, bigLabel)

	writeFile(t, f.path, "profiles:\n  big: {cpus: 5, memory_mib: 2048, scratch_gib: 4}\n  small: {cpus: 1}\n")
	next := reloadConfig(f.log, f.srv, f.cfg, (*config.Config).Reload)
	if next == f.cfg || next.Profiles["big"].CPUs != 5 {
		t.Fatalf("the reload did not take: %+v", next)
	}
	later := createOn(t, f.srv, bigLabel)

	for _, tc := range []struct {
		id   string
		want config.Profile
	}{
		{running, config.Profile{CPUs: 3, MemoryMiB: 1536, ScratchGiB: 4}},
		{created, config.Profile{CPUs: 3, MemoryMiB: 1536, ScratchGiB: 4}},
		{later, config.Profile{CPUs: 5, MemoryMiB: 2048, ScratchGiB: 4}},
	} {
		if j := jobOf(t, f.srv, tc.id); !reflect.DeepEqual(j.profile, tc.want) {
			t.Errorf("job %s has %+v, want %+v", tc.id, j.profile, tc.want)
		}
	}
	for _, id := range []string{created, later} {
		if _, err := start(testContext(t), c, id); err != nil {
			t.Fatal(err)
		}
	}
	for n, want := range []uint32{3, 3, 5} {
		if got := cpusOfSpec(t, f.launcher, n); got != want {
			t.Errorf("job process %d got %d CPUs, want %d", n, got, want)
		}
	}
	log := f.buf.String()
	for _, want := range []string{
		`msg="reloaded the config file" path=` + f.path + ` profiles="big default small"`,
		`msg=profile name=big defined=true cpus=5 memory_mib=2048 scratch_gib=4`,
		`msg=profile name=default defined=false cpus=2`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log does not contain %q:\n%s", want, log)
		}
	}
}

// A reload of a file with a YAML error, an unknown key, an invalid value,
// or one that RUNNER_MICROVM_CONFIG names but is gone keeps the old
// profiles, and the log names the error with its line, key and profile
// (CFG-8).
func TestFailedReloadKeepsTheProfiles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		content string // empty: the file is deleted
		want    string
	}{
		{"YAML error", "profiles:\n  big: {cpus: 5\n", `: yaml: line 1: did not find expected`},
		{"unknown key", "profiles:\n  big:\n    cpus: 5\n    gpus: 1\n", `: line 4: profile \"big\": gpus: unknown key`},
		{
			"invalid value", "profiles:\n  big:\n    cpus: 5\n  small:\n    memory_mib: -1\n",
			`: line 5: profile \"small\": memory_mib: must be from 1 to 4294967295, not -1`,
		},
		{"missing named file", "", `: open @PATH@: no such file or directory`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newReloadFixture(t, "profiles:\n  big: {cpus: 3}\n")
			if tc.content == "" {
				if err := os.Remove(f.path); err != nil {
					t.Fatal(err)
				}
			} else {
				writeFile(t, f.path, tc.content)
			}
			if next := reloadConfig(f.log, f.srv, f.cfg, (*config.Config).Reload); next != f.cfg {
				t.Errorf("a failed reload returned %+v, want the running config", next)
			}
			if j := jobOf(t, f.srv, createOn(t, f.srv, bigLabel)); j.profile.CPUs != 3 {
				t.Errorf("a job after the failed reload has %+v, want the old big", j.profile)
			}
			want := `level=ERROR msg="reload failed: kept the running config" err="config ` + f.path +
				strings.ReplaceAll(tc.want, "@PATH@", f.path)
			if log := f.buf.String(); !strings.Contains(log, want) {
				t.Errorf("log does not contain %q:\n%s", want, log)
			}
		})
	}
}

// A reload that changes a plugin-level key logs the key as needing a
// restart, with both values, and the running value stays; the profiles
// still reload.
func TestReloadLogsPluginLevelKeysAsNeedingARestart(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	dirs := func(state string) string {
		return fmt.Sprintf("state_dir: %s/%s\ncache_dir: %s/cache\nruntime_dir: %s/run\nlisten: unix://%s/run/rm.sock\n",
			base, state, base, base, base)
	}
	f := newReloadFixture(t, dirs("state")+"max_memory_mib: 4096\nprofiles:\n  big: {cpus: 3}\n")
	writeFile(t, f.path, dirs("state2")+"max_memory_mib: 2048\ncache_max_gib: 0\nlib_dir: /opt/libkrun\nlibkrun_log_level: debug\n"+
		"profiles:\n  big: {cpus: 4}\n")

	next := reloadConfig(f.log, f.srv, f.cfg, (*config.Config).Reload)
	if next.StateDir != base+"/state" || next.MaxMemoryMiB != 4096 || next.CacheMaxGiB != 20 || next.LibDir != "" ||
		next.LibkrunLogLevel != "warn" {
		t.Errorf("state_dir %s, max_memory_mib %d, cache_max_gib %d, lib_dir %q, libkrun_log_level %s: want the running values",
			next.StateDir, next.MaxMemoryMiB, next.CacheMaxGiB, next.LibDir, next.LibkrunLogLevel)
	}
	if j := jobOf(t, f.srv, createOn(t, f.srv, bigLabel)); j.profile.CPUs != 4 {
		t.Errorf("the profiles did not reload: %+v", j.profile)
	}
	log := f.buf.String()
	for _, want := range []string{
		`level=WARN msg="a changed setting takes effect only at a restart: kept the running value" key=state_dir running=` +
			base + `/state file=` + base + `/state2`,
		`key=max_memory_mib running=4096 file=2048`,
		`key=cache_max_gib running=20 file=0`,
		`key=lib_dir running="" file=/opt/libkrun`,
		`key=libkrun_log_level running=warn file=debug`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log does not contain %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "key=cache_dir") {
		t.Errorf("an unchanged key is logged:\n%s", log)
	}
}

// Creates running concurrently with reloads each get one whole profile of
// one set: never a mix of the old set's values and the new one's.
func TestCreatesDuringReloads(t *testing.T) {
	t.Parallel()
	sets := []string{
		"profiles:\n  default: {cpus: 1, memory_mib: 1001, scratch_gib: 1}\n  big: {cpus: 3, memory_mib: 1003, scratch_gib: 3}\n",
		"profiles:\n  default: {cpus: 2, memory_mib: 1002, scratch_gib: 2}\n  big: {cpus: 4, memory_mib: 1004, scratch_gib: 4}\n",
	}
	f := newReloadFixture(t, sets[0])
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Go(func() {
		cfg := f.cfg
		for i := range 50 {
			if ctx.Err() != nil {
				return
			}
			// Not writeFile: t.Fatal can't stop the test from here.
			tmp := f.path + ".tmp"
			if err := os.WriteFile(tmp, []byte(sets[(i+1)%2]), 0o600); err != nil {
				t.Error(err)
				return
			}
			if err := os.Rename(tmp, f.path); err != nil {
				t.Error(err)
				return
			}
			if cfg = reloadConfig(f.log, f.srv, cfg, (*config.Config).Reload); cfg.Profiles["big"].CPUs != uint32(3+(i+1)%2) {
				t.Errorf("reload %d did not take", i)
			}
		}
	})
	ids := make(chan string, 400)
	for _, arg := range []string{testImage, bigLabel} {
		wg.Go(func() {
			for range 200 {
				resp, err := f.srv.Create(ctx, &pluginv1alpha.CreateRequest{LabelArg: arg})
				if err != nil {
					t.Error(err)
					return
				}
				ids <- resp.GetEnvironmentId()
			}
		})
	}
	wg.Wait()
	close(ids)
	for id := range ids {
		p := jobOf(t, f.srv, id).profile
		if p.MemoryMiB != 1000+p.CPUs || p.ScratchGiB != uint64(p.CPUs) {
			t.Errorf("job %s has %+v: a mix of two sets", id, p)
		}
	}
	if log := f.buf.String(); strings.Contains(log, "level=ERROR") {
		t.Errorf("a reload failed:\n%s", log)
	}
}

// reloadOnSignal reloads on each signal it is sent, and returns when its
// context ends.
func TestReloadOnSignal(t *testing.T) {
	t.Parallel()
	f := newReloadFixture(t, "profiles:\n  big: {cpus: 3}\n")
	hup := make(chan os.Signal, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); reloadOnSignal(ctx, f.log, hup, f.srv, f.cfg, (*config.Config).Reload) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("reloadOnSignal did not return when its context ended")
		}
	}()

	for _, cpus := range []uint32{4, 6} {
		writeFile(t, f.path, fmt.Sprintf("profiles:\n  big: {cpus: %d}\n", cpus))
		hup <- syscall.SIGHUP
		deadline := time.Now().Add(10 * time.Second)
		for {
			if p, _ := profilesOf(f.srv).Lookup("big"); p.CPUs == cpus {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no reload to %d CPUs:\n%s", cpus, f.buf.String())
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// A reload still reading the config file as the plugin stops, as one on a
// stalled network mount does, doesn't hold the plugin up: serving ends and
// the server is closed at once. The reload, unblocked later, changes
// nothing on the closed server.
func TestStopDoesNotWaitForABlockedReload(t *testing.T) {
	t.Parallel()
	f := newReloadFixture(t, "profiles:\n  big: {cpus: 3}\n")
	writeFile(t, f.path, "profiles:\n  big: {cpus: 4}\n")
	lis, closeLis, err := listen(t.Context(), filepath.Join(t.TempDir(), "plugin.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeLis()
	reading, unblock := make(chan struct{}), make(chan struct{})
	read := func(running *config.Config) (*config.Config, []config.Change, error) {
		close(reading)
		<-unblock
		return running.Reload()
	}
	hup := make(chan os.Signal, 1)
	hup <- syscall.SIGHUP
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- serveWithReloads(ctx, f.log, lis, f.srv, hup, nil, f.cfg, read) }()
	select {
	case <-reading:
	case <-time.After(10 * time.Second):
		t.Fatal("no reload started")
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serveWithReloads: %v", err)
		}
	case <-time.After(10 * time.Second):
		close(unblock)
		t.Fatal("serveWithReloads waited for the blocked reload")
	}
	close(unblock)
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(f.buf.String(), "reload ended after the plugin stopped") {
		if time.Now().After(deadline) {
			t.Fatalf("the reload did not end:\n%s", f.buf.String())
		}
		time.Sleep(time.Millisecond)
	}
	if p, _ := profilesOf(f.srv).Lookup("big"); p.CPUs != 3 {
		t.Errorf("the reload changed the closed server's profiles: big has %d CPUs, want 3", p.CPUs)
	}
}

// profilesOf returns the profiles srv's Creates pick from.
func profilesOf(srv *Server) config.Profiles {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.profiles
}
