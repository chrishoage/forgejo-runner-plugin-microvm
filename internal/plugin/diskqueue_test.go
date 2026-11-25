// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/admission"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/config"
	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
)

// diskFile makes an empty file named name in a directory of t's, a disk's
// source as admission reads it, and returns its path. The fake job process
// never opens it.
func diskFile(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// diskServer returns a server on the fakes with no memory budget, a log t
// can read, and profiles, which it serves to a client.
func diskServer(t *testing.T, launcher *fakeLauncher, profiles config.Profiles) (*Server, pluginv1alpha.BackendPluginClient, *logBuffer) {
	t.Helper()
	srv := newTestServer(t, &fakeCache{}, launcher)
	log := withBudget(srv, 0)
	srv.SetProfiles(profiles)
	return srv, pluginv1alpha.NewBackendPluginClient(serve(t, srv)), log
}

// diskProfile is a profile with disks.
func diskProfile(disks ...config.Disk) config.Profile {
	return config.Profile{CPUs: 1, MemoryMiB: 512, ScratchGiB: 1, Disks: disks}
}

// createProfile creates a job on c with profile, and returns its id.
func createProfile(t *testing.T, c pluginv1alpha.BackendPluginClient, profile string) string {
	t.Helper()
	resp, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{LabelArg: testImage + ";profile=" + profile})
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetEnvironmentId()
}

// A Start whose read-write disk another job holds waits after its image
// is ready, before its job process starts, sending a waiting line naming
// the disk and its holder, and once the holder is removed, a got line,
// then boots; the plugin's log has both. A Start that takes its disk at
// once sends neither line, and logs no wait (QUE-3, QUE-8).
func TestAStartWaitsForADisk(t *testing.T) {
	t.Parallel()
	disk := diskFile(t, "persist.ext4")
	launcher := &fakeLauncher{}
	_, c, log := diskServer(t, launcher, config.Profiles{"rw": diskProfile(config.Disk{Source: disk, Target: "/data", ReadWrite: true})})
	first, second := createProfile(t, c, "rw"), createProfile(t, c, "rw")

	r := startInBackground(testContext(t), c, first)
	if err := r.result(t); err != nil {
		t.Fatal(err)
	}
	for _, l := range r.remaining() {
		if strings.Contains(l, "disk") {
			t.Errorf("a Start that took its disk at once sent %q", l)
		}
	}
	if strings.Contains(log.String(), "waiting for disk") {
		t.Errorf("a Start that took its disk at once logged a wait:\n%s", log)
	}

	r = startInBackground(testContext(t), c, second)
	r.waiting(t, regexp.MustCompile(at+`waiting for disk `+regexp.QuoteMeta(disk)+` \(held by `+first+`; 0 ahead\)\n$`))
	if n := launcher.launched(); n != 1 {
		t.Fatalf("%d job processes launched while the second job waits, want 1", n)
	}
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: first}); err != nil {
		t.Fatal(err)
	}
	if l := r.next(t); !regexp.MustCompile(at + `got disk ` + regexp.QuoteMeta(disk) + ` after \d+\.\d\ds\n$`).MatchString(l) {
		t.Errorf("progress line after the wait %q, want the got line", l)
	}
	if err := r.result(t); err != nil {
		t.Fatalf("the waiting Start, once admitted: %v", err)
	}
	logged := log.String()
	for _, want := range []string{
		`msg="waiting for disk" env=` + second + ` disk=` + disk + ` mode=rw holders=` + first + ` ahead=0`,
		`msg="got disk" env=` + second + ` disk=` + disk + ` mode=rw took=`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("the plugin's log lacks %q:\n%s", want, logged)
		}
	}
}

// Two paths naming one file, here a symlink, share its disk queue: a job
// on the link waits for one on the file, and its waiting line names the
// disk as its own profile does.
func TestTwoPathsToOneDiskShareAQueue(t *testing.T) {
	t.Parallel()
	disk := diskFile(t, "persist.ext4")
	link := filepath.Join(t.TempDir(), "link.ext4")
	if err := os.Symlink(disk, link); err != nil {
		t.Fatal(err)
	}
	_, c, _ := diskServer(t, &fakeLauncher{}, config.Profiles{
		"file": diskProfile(config.Disk{Source: disk, Target: "/data", ReadWrite: true}),
		"link": diskProfile(config.Disk{Source: link, Target: "/data"}),
	})
	first, second := createProfile(t, c, "file"), createProfile(t, c, "link")
	if _, err := start(testContext(t), c, first); err != nil {
		t.Fatal(err)
	}
	r := startInBackground(testContext(t), c, second)
	r.waiting(t, regexp.MustCompile(`waiting for disk `+regexp.QuoteMeta(link)+` \(held by `+first+`; 0 ahead\)`))
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: first}); err != nil {
		t.Fatal(err)
	}
	if err := r.result(t); err != nil {
		t.Fatalf("the waiting Start, once admitted: %v", err)
	}
}

// A disk whose source is missing at Start fails it with FailedPrecondition
// naming the source, before any wait or job process.
func TestAMissingDiskFailsStart(t *testing.T) {
	t.Parallel()
	disk := diskFile(t, "gone.ext4")
	launcher := &fakeLauncher{}
	_, c, log := diskServer(t, launcher, config.Profiles{"gone": diskProfile(config.Disk{Source: disk, Target: "/data"})})
	id := createProfile(t, c, "gone")
	if err := os.Remove(disk); err != nil {
		t.Fatal(err)
	}
	_, err := start(testContext(t), c, id)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "disk "+disk+": does not exist") {
		t.Errorf("Start = %v, want FailedPrecondition naming the missing source", err)
	}
	if n := launcher.launched(); n != 0 {
		t.Errorf("%d job processes launched for a missing disk", n)
	}
	if strings.Contains(log.String(), "waiting for disk") {
		t.Errorf("a missing disk was waited for:\n%s", log)
	}
}

// A Start whose disk's stat hangs, as on a dead NFS or FUSE mount, ends
// with its context: Remove ends it with Canceled and tears the job down,
// so a drain still ends with the last job, while the stat stays stuck.
func TestAHungDiskStatEndsWithRemove(t *testing.T) {
	t.Parallel()
	disk := diskFile(t, "persist.ext4")
	srv, c, _ := diskServer(t, &fakeLauncher{}, config.Profiles{"rw": diskProfile(config.Disk{Source: disk, Target: "/data", ReadWrite: true})})
	stating, unhang := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(unhang) })
	srv.statDisk = func(path string) (fs.FileInfo, error) {
		close(stating)
		<-unhang
		return os.Stat(path)
	}
	id := createProfile(t, c, "rw")
	r := startInBackground(testContext(t), c, id)
	<-stating

	srv.Drain()
	removed := make(chan error, 1)
	go func() {
		_, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id})
		removed <- err
	}()
	select {
	case <-srv.Drained():
	case <-time.After(10 * time.Second):
		t.Fatal("the drain did not end after the Remove of a job whose Start hangs in stat")
	}
	if err := <-removed; err != nil {
		t.Errorf("Remove: %v", err)
	}
	if err := r.result(t); status.Code(err) != codes.Canceled {
		t.Errorf("the Start hung in stat = %v, want Canceled", err)
	}
}

// A source's stat stuck on a dead mount is the only one of it in flight: a
// later Start on that source fails at once naming the disk, without a
// stat of its own, so stuck stats can't pile up; once the stuck one
// returns, a Start on it works again.
func TestAStuckDiskStatIsNotRepeated(t *testing.T) {
	t.Parallel()
	disk := diskFile(t, "persist.ext4")
	srv, c, _ := diskServer(t, &fakeLauncher{}, config.Profiles{"ro": diskProfile(config.Disk{Source: disk, Target: "/data"})})
	stating, unhang := make(chan struct{}, 1), make(chan struct{})
	var unhung sync.Once
	t.Cleanup(func() { unhung.Do(func() { close(unhang) }) })
	var stats atomic.Int32
	statDone := make(chan struct{}, 1)
	srv.statDisk = func(path string) (fs.FileInfo, error) {
		if stats.Add(1) == 1 {
			stating <- struct{}{}
			<-unhang
			defer func() { statDone <- struct{}{} }()
		}
		return os.Stat(path)
	}
	stuck := createProfile(t, c, "ro")
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	r := startInBackground(ctx, c, stuck)
	<-stating
	cancel()
	if err := r.result(t); status.Code(err) != codes.Canceled {
		t.Fatalf("the Start hung in stat = %v, want Canceled", err)
	}

	_, err := start(testContext(t), c, createProfile(t, c, "ro"))
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "disk "+disk+": ") {
		t.Errorf("Start while its disk's stat is stuck = %v, want FailedPrecondition naming the disk", err)
	}
	if n := stats.Load(); n != 1 {
		t.Errorf("%d stats of the stuck source, want 1", n)
	}

	unhung.Do(func() { close(unhang) })
	<-statDone
	waitFor(t, "the stuck stat's entry to go", func() bool { return srv.disksStatting() == 0 })
	if _, err := start(testContext(t), c, createProfile(t, c, "ro")); err != nil {
		t.Errorf("Start after the stuck stat returned: %v", err)
	}
}

// A Start cancelled while it waits for a disk fails with its context's
// code, logs the wait as stopped, and holds no place: the job behind it
// gets the disk once the holder is removed, and a disk it took before the
// wait is free.
func TestACancelledDiskWaitFreesItsPlace(t *testing.T) {
	t.Parallel()
	a, b := diskFile(t, "a.ext4"), diskFile(t, "b.ext4")
	srv, c, log := diskServer(t, &fakeLauncher{}, config.Profiles{
		"b":    diskProfile(config.Disk{Source: b, Target: "/b", ReadWrite: true}),
		"both": diskProfile(config.Disk{Source: a, Target: "/a", ReadWrite: true}, config.Disk{Source: b, Target: "/b", ReadWrite: true}),
	})
	holder, cancelled, next := createProfile(t, c, "b"), createProfile(t, c, "both"), createProfile(t, c, "b")
	if _, err := start(testContext(t), c, holder); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	r := startInBackground(ctx, c, cancelled)
	r.waiting(t, regexp.MustCompile(`waiting for disk `+regexp.QuoteMeta(b)+` \(held by `+holder+`; 0 ahead\)`))
	n := startInBackground(testContext(t), c, next)
	n.waiting(t, regexp.MustCompile(`waiting for disk `+regexp.QuoteMeta(b)+` \(held by `+holder+`; 1 ahead\)`))
	cancel()
	if err := r.result(t); status.Code(err) != codes.Canceled {
		t.Errorf("the cancelled waiting Start = %v, want Canceled", err)
	}
	waitFor(t, "the stopped wait's line", func() bool {
		return strings.Contains(log.String(), `msg="stopped waiting for disk" env=`+cancelled+` disk=`+b+` mode=rw took=`)
	})
	if !diskFree(t, srv, a) {
		t.Errorf("%s, which the cancelled Start took before it waited, is still held", a)
	}
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: holder}); err != nil {
		t.Fatal(err)
	}
	if err := n.result(t); err != nil {
		t.Fatalf("the Start behind the cancelled one: %v", err)
	}
}

// A failed Start frees its disks once its job process is reaped, before it
// returns; one whose kill fails keeps them until its Remove reaps it.
func TestDisksAreHeldUntilTheReap(t *testing.T) {
	t.Parallel()
	disk := diskFile(t, "persist.ext4")
	launcher := &fakeLauncher{does: networkFails}
	srv, c, _ := diskServer(t, launcher, config.Profiles{"rw": diskProfile(config.Disk{Source: disk, Target: "/data", ReadWrite: true})})
	if _, err := start(testContext(t), c, createProfile(t, c, "rw")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Start = %v, want FailedPrecondition", err)
	}
	if !diskFree(t, srv, disk) {
		t.Error("a failed Start whose job process was reaped still holds its disk")
	}

	failed := createProfile(t, c, "rw")
	launcher.setKillFails(1)
	if _, err := start(testContext(t), c, failed); status.Code(err) == codes.OK {
		t.Fatal("Start whose job process failed succeeded")
	}
	if diskFree(t, srv, disk) {
		t.Error("a failed Start whose kill failed freed its disk")
	}
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: failed}); err != nil {
		t.Fatal(err)
	}
	if !diskFree(t, srv, disk) {
		t.Error("Remove, which reaped the failed Start's job process, did not free its disk")
	}
}

// diskFree reports whether a job could write the disk at path at once.
func diskFree(t *testing.T, srv *Server, path string) bool {
	t.Helper()
	disks, err := admissionDisks(t.Context(), func(_ context.Context, p string) (fs.FileInfo, error) { return os.Stat(p) }, []config.Disk{{Source: path, ReadWrite: true}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := srv.admission.Admit(ctx, "probe", disks, 0, admission.Observer{}); err != nil {
		return false
	}
	srv.admission.Release("probe")
	return true
}
