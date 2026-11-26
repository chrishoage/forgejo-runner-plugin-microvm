// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	guestagent "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/agent"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/config"
	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/imagecache"
)

// svc is a service of a Create request.
func svc(name, image string) *pluginv1alpha.ServiceContainer {
	return &pluginv1alpha.ServiceContainer{Name: name, Image: image}
}

// createWithServices creates a job named name, on the test image's label,
// with services, on c, and returns its environment id.
func createWithServices(t *testing.T, c pluginv1alpha.BackendPluginClient, name string, services ...*pluginv1alpha.ServiceContainer) string {
	t.Helper()
	resp, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{Name: name, LabelArg: testImage, Services: services})
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetEnvironmentId()
}

// Create accepts a job's services, each named by the Docker backend's alias
// rule, and refuses, adding no job: two VMs with one name, the job's
// container's hostname included, or a VM's hostname that is another's name
// or hostname, naming both keys, and a service with no
// name or no image, with InvalidArgument; more than 252 services with
// FailedPrecondition, naming the count and the limit, as a job whose need,
// its profile's memory_mib for each VM, is over max_memory_mib.
func TestCreateTakesServices(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeCache{}, &fakeLauncher{})
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	resp, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{
		Name: "build", LabelArg: testImage,
		Services: []*pluginv1alpha.ServiceContainer{
			{Name: "My.DB", Image: "postgres:17", Env: map[string]string{"POSTGRES_PASSWORD": "s3cret"}},
			svc("cache", "redis:8"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	j, err := srv.lookup(resp.GetEnvironmentId())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range j.services {
		got = append(got, fmt.Sprintf("%s %s %s %v", s.key, s.name, s.image, s.env.Reveal()))
	}
	if want := []string{"My.DB my_db postgres:17 map[POSTGRES_PASSWORD:s3cret]", "cache cache redis:8 map[]"}; !slices.Equal(got, want) {
		t.Errorf("services %q, want %q", got, want)
	}

	many := make([]*pluginv1alpha.ServiceContainer, 253)
	for i := range many {
		many[i] = svc(fmt.Sprintf("s%d", i), "redis")
	}
	for _, tc := range []struct {
		desc     string
		services []*pluginv1alpha.ServiceContainer
		code     codes.Code
		want     string
	}{
		{
			"two keys, one name",
			[]*pluginv1alpha.ServiceContainer{svc("My.DB", "postgres"), svc("web", "nginx"), svc("my_db", "mysql")},
			codes.InvalidArgument, `services "My.DB" and "my_db" both have the name "my_db" on the job network`,
		},
		{
			"the job's container's hostname",
			[]*pluginv1alpha.ServiceContainer{svc("Build", "postgres")},
			codes.InvalidArgument, `service "Build" and the job's container (job "build") both have the name "build" on the job network`,
		},
		{
			"a hostname that is another service's name",
			[]*pluginv1alpha.ServiceContainer{svc("my-db", "postgres"), svc("my_db", "mysql")},
			codes.InvalidArgument, `service "my_db"'s hostname is "my-db", the name of service "my-db" on the job network`,
		},
		{
			"a name that is an earlier service's hostname",
			[]*pluginv1alpha.ServiceContainer{svc("my_db", "mysql"), svc("my-db", "postgres")},
			codes.InvalidArgument, `service "my_db"'s hostname is "my-db", the name of service "my-db" on the job network`,
		},
		{
			"two hostnames, one cut",
			[]*pluginv1alpha.ServiceContainer{svc(strings.Repeat("a", 63)+"_1", "postgres"), svc(strings.Repeat("a", 63)+"_2", "mysql")},
			codes.InvalidArgument, `services "` + strings.Repeat("a", 63) + `_1" and "` + strings.Repeat("a", 63) + `_2" both have the hostname "` + strings.Repeat("a", 63) + `" on the job network`,
		},
		{
			"two hostnames, one collapsed",
			[]*pluginv1alpha.ServiceContainer{svc("a__b", "postgres"), svc("a_b", "mysql")},
			codes.InvalidArgument, `services "a__b" and "a_b" both have the hostname "a-b" on the job network`,
		},
		{"no name", []*pluginv1alpha.ServiceContainer{svc("", "postgres")}, codes.InvalidArgument, `a service has no name (its image is "postgres")`},
		{"no image", []*pluginv1alpha.ServiceContainer{svc("db", "")}, codes.InvalidArgument, `service "db" has no image`},
		{
			"253 services", many, codes.FailedPrecondition,
			"the job has 253 services, more than the 252 a job network holds beside the job's container",
		},
	} {
		_, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{Name: "build", LabelArg: testImage, Services: tc.services})
		if status.Code(err) != tc.code || status.Convert(err).Message() != tc.want {
			t.Errorf("%s: Create = %v; want %v: %s", tc.desc, err, tc.code, tc.want)
		}
	}
	// A service's hostname may not be the job's container's name either.
	_, err = c.Create(testContext(t), &pluginv1alpha.CreateRequest{Name: "X-Y", LabelArg: testImage, Services: []*pluginv1alpha.ServiceContainer{svc("x_y", "postgres")}})
	if want := `service "x_y"'s hostname is "x-y", the name of the job's container (job "X-Y") on the job network`; status.Code(err) != codes.InvalidArgument ||
		status.Convert(err).Message() != want {
		t.Errorf("Create of a service whose hostname is the job's container's = %v; want InvalidArgument: %s", err, want)
	}
	if n := srv.jobCount(); n != 1 {
		t.Errorf("%d jobs in the table after refused Creates, want 1", n)
	}
	// 252 services are a job network's limit, not over it.
	if _, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{Name: "build", LabelArg: testImage, Services: many[:252]}); err != nil {
		t.Errorf("Create of 252 services: %v", err)
	}

	// A job of three VMs needs three times its profile's 1024 MiB.
	srv = newTestServer(t, &fakeCache{}, &fakeLauncher{})
	withBudget(srv, 3071)
	c = pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	_, err = c.Create(testContext(t), &pluginv1alpha.CreateRequest{LabelArg: testImage, Services: many[:2]})
	if want := "the job needs 3072 MiB of memory, more than max_memory_mib, 3071 MiB: it could never run"; status.Code(err) != codes.FailedPrecondition ||
		status.Convert(err).Message() != want {
		t.Errorf("Create of a job over the cap = %v; want FailedPrecondition: %s", err, want)
	}
	if _, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{LabelArg: testImage, Services: many[:1]}); err != nil {
		t.Errorf("Create of a job that fits: %v", err)
	}
}

// Create's checks come in order: the label's form, its profile, the image,
// then the services.
func TestCreateChecksServicesLast(t *testing.T) {
	t.Parallel()
	c := pluginv1alpha.NewBackendPluginClient(serve(t, newTestServer(t, &fakeCache{}, &fakeLauncher{})))
	bad := []*pluginv1alpha.ServiceContainer{svc("db", "")}
	for _, tc := range []struct{ desc, arg, want string }{
		{"the label's form", testImage + ";x=y", `label "` + testImage + `;x=y": "x=y": only profile=<name> may follow the image`},
		{"the profile", testImage + ";profile=nope", `label "` + testImage + `;profile=nope": unknown profile "nope"`},
		{"the image", ";profile=default", "no image: neither the label nor the job's container: names one"},
	} {
		_, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{LabelArg: tc.arg, Services: bad})
		if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != tc.want {
			t.Errorf("%s: Create = %v; want InvalidArgument: %s", tc.desc, err, tc.want)
		}
	}
}

// Start boots each service as a VM after the job's container, in the order
// the runner lists them, with the profile's shape but none of its mounts
// or disks, on the job network at the address its index gives, and its
// command, the image's Entrypoint and Cmd, in its boot config; every VM's
// hosts list names every VM, and its own hostname when that isn't its
// name. No file of the job holds a service's env: Start sends it in one
// StartService to each service's agent once it answers Ping, and none to
// the job's container's. Each image is acquired with the profile's
// force_pull, and each VM's progress lines name it. Exec still goes to the
// job's container only.
func TestStartBootsTheServices(t *testing.T) {
	t.Parallel()
	cache := &fakeCache{}
	// The service's agent fails every Exec, so an Exec that reached it shows.
	agents := []*recordingAgent{{}, {execFails: true}, {}}
	launcher := &fakeLauncher{vmAgents: map[int]agentv1.AgentServiceServer{0: agents[0], 1: agents[1], 2: agents[2]}}
	srv := newTestServer(t, cache, launcher)
	srv.SetProfiles(config.Profiles{"svc": {
		CPUs: 3, MemoryMiB: 768, ScratchGiB: 2, ScratchPaths: []string{"/var/lib/docker"}, ForcePull: true,
		Mounts: []config.Mount{{Source: "/srv/tools", Target: "/opt/tools"}},
		Disks:  []config.Disk{{Source: diskFile(t, "store.ext4"), Target: "/var/lib/store", ReadWrite: true}},
	}})
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	resp, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{
		Name: "Build", LabelArg: testImage + ";profile=svc",
		Services: []*pluginv1alpha.ServiceContainer{
			{Name: "My.DB", Image: "postgres:17", Env: map[string]string{"POSTGRES_PASSWORD": "s3cret"}},
			svc("cache", "redis:8"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := resp.GetEnvironmentId()
	r := startInBackground(testContext(t), c, id)
	if err := r.result(t); err != nil {
		t.Fatal(err)
	}
	lines := r.remaining()

	cache.mu.Lock()
	acquired := slices.Clone(cache.acquired)
	cache.mu.Unlock()
	slices.SortFunc(acquired, func(a, b imagecache.Request) int { return a.VM - b.VM })
	if want := []imagecache.Request{
		{Job: id, VM: 0, Ref: testImage, ForcePull: true}, {Job: id, VM: 1, Ref: "postgres:17", ForcePull: true}, {Job: id, VM: 2, Ref: "redis:8", ForcePull: true},
	}; !slices.Equal(acquired, want) {
		t.Errorf("acquired %+v, want %+v", acquired, want)
	}

	vms := specOf(t, launcher, id).GetVms()
	if len(vms) != 3 {
		t.Fatalf("the job spec has %d VMs, want 3", len(vms))
	}
	hosts := []*agentv1.Host{{Name: "build", Address: "198.18.0.2"}, {Name: "my_db", Address: "198.18.0.3"}, {Name: "cache", Address: "198.18.0.4"}}
	// A VM whose hostname isn't its name has its hostname in its own hosts
	// list only, at its own address.
	ownHosts := [][]*agentv1.Host{hosts, append(slices.Clone(hosts), &agentv1.Host{Name: "my-db", Address: "198.18.0.3"}), hosts}
	state := filepath.Join(srv.cfg.Dirs.State, "jobs", id)
	for i, tc := range []struct{ name, hostname, addr, mac string }{
		{"build", "build", "198.18.0.2", "02:00:c6:12:00:02"},
		{"my_db", "my-db", "198.18.0.3", "02:00:c6:12:00:03"},
		{"cache", "cache", "198.18.0.4", "02:00:c6:12:00:04"},
	} {
		vm, n := vms[i], fmt.Sprint(i)
		if vm.GetName() != tc.name || vm.GetCpus() != 3 || vm.GetMemoryMib() != 768 || vm.GetScratchSizeBytes() != 2<<30 ||
			vm.GetGuestAddress() != tc.addr || vm.GetGuestMac() != tc.mac ||
			vm.GetScratchDisk() != filepath.Join(state, n, "scratch.ext4") || vm.GetConsole() != filepath.Join(state, n, "console") ||
			vm.GetRootDir() != filepath.Join(state, n, "root") || vm.GetLayerDir() != filepath.Join(srv.cfg.Dirs.Cache, "jobs", id, n) ||
			vm.GetAgentSocket() != filepath.Join(srv.cfg.Dirs.Runtime, id, n, "agent.sock") {
			t.Errorf("VM %d = %v", i, vm)
		}
		boot := vm.GetBoot()
		if boot.GetHostname() != tc.hostname || !slices.EqualFunc(boot.GetHosts(), ownHosts[i], func(a, b *agentv1.Host) bool { return proto.Equal(a, b) }) ||
			boot.GetNetwork().GetAddress() != tc.addr+"/24" || !slices.Equal(boot.GetScratchPaths(), []string{"/var/lib/docker"}) {
			t.Errorf("VM %d's boot config = %v", i, boot)
		}
		// What the agent reads is what the plugin built.
		if b, err := guestagent.ReadBootConfig(filepath.Join(vm.GetRootDir(), guestagent.BootConfigPath), &slog.LevelVar{}); err != nil || !proto.Equal(b, boot) {
			t.Errorf("VM %d's boot config file = %v, %v; want %v", i, b, err, boot)
		}
		if i == 0 {
			if len(vm.GetMounts()) != 1 || len(vm.GetDisks()) != 1 || boot.GetService() != nil || len(boot.GetGuestDirs()) != 3 {
				t.Errorf("the job's container = %v", vm)
			}
			continue
		}
		if len(vm.GetMounts()) != 0 || len(vm.GetDisks()) != 0 || len(boot.GetMounts()) != 0 || len(boot.GetDisks()) != 0 || len(boot.GetGuestDirs()) != 0 {
			t.Errorf("service VM %d has storage or guest directories: %v", i, vm)
		}
		want := &agentv1.Service{Argv: []string{"entrypoint.sh", "serve", "--port", "6379"}}
		if !proto.Equal(boot.GetService(), want) {
			t.Errorf("service VM %d's service = %v, want %v", i, boot.GetService(), want)
		}
	}
	// No file of the job, nor its job spec, holds the env's value (LOG-2).
	for _, f := range jobFiles(t, srv, id) {
		if b, err := os.ReadFile(f); err == nil && bytes.Contains(b, []byte("s3cret")) {
			t.Errorf("%s holds the service's env value", f)
		}
	}
	if b, err := proto.Marshal(specOf(t, launcher, id)); err != nil || bytes.Contains(b, []byte("s3cret")) {
		t.Errorf("the job spec holds the service's env value (%v)", err)
	}
	// Each service's agent got one StartService, with its env, after its
	// Ping; the job's container's got none.
	for i, want := range []string{"Ping", "Ping StartService map[POSTGRES_PASSWORD:s3cret]", "Ping StartService map[]"} {
		if got := agents[i].record(); got != want {
			t.Errorf("VM %d's agent got %q, want %q", i, got, want)
		}
	}

	// One line per image and one per VM, each as it is ready: they come in
	// any order, and every line starts with the elapsed time, so each is
	// matched on its own, never by position.
	var images, ready []string
	for _, l := range lines {
		switch {
		case strings.Contains(l, "] image "):
			images = append(images, l)
		case strings.Contains(l, "] VM "):
			ready = append(ready, l)
		}
	}
	matchEach(t, "image", images,
		at+`image `+regexp.QuoteMeta(testImage)+` ready \(sha256:0, 2 layers\) in `,
		at+`image postgres:17 ready for service my_db \(sha256:0, 2 layers\) in `,
		at+`image redis:8 ready for service cache \(sha256:0, 2 layers\) in `,
	)
	vmLine := func(name string) string { return at + `VM ` + name + ` \(3 CPUs, 768 MiB\) ready in \d+\.\d\ds\n$` }
	matchEach(t, "VM", ready, vmLine("build"), vmLine("my_db"), vmLine("cache"))

	out, err := execAll(t, c, &pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"true"}, Workdir: "/"})
	if err != nil {
		t.Fatalf("Exec: %v (it went to a service's agent)", err)
	}
	if len(out) == 0 {
		t.Error("Exec sent nothing")
	}
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatal(err)
	}
}

// Start acquires every VM's image before the wait for admission: with
// max_memory_mib fitting one job of two VMs, a second such job has acquired
// both its images and waits for its whole need, launching nothing, until
// the first is removed.
func TestAJobOfServicesWaitsWhole(t *testing.T) {
	t.Parallel()
	cache, launcher := &fakeCache{}, &fakeLauncher{}
	srv := newTestServer(t, cache, launcher)
	log := withBudget(srv, 2048)
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	first := createWithServices(t, c, "first", svc("redis", "redis:8"))
	second := createWithServices(t, c, "second", svc("redis", "redis:8"))
	if _, err := start(testContext(t), c, first); err != nil {
		t.Fatal(err)
	}

	r := startInBackground(testContext(t), c, second)
	want := regexp.MustCompile(at + `waiting for memory: needs 2048 MiB, 0 of 2048 MiB free; 0 ahead\n$`)
	for l := r.next(t); !want.MatchString(l); l = r.next(t) {
		if !strings.Contains(l, "] image ") {
			t.Fatalf("progress line %q before the wait, want only the images' lines", l)
		}
	}
	cache.mu.Lock()
	var vms []int
	for _, req := range cache.acquired {
		if req.Job == second {
			vms = append(vms, req.VM)
		}
	}
	cache.mu.Unlock()
	if slices.Sort(vms); !slices.Equal(vms, []int{0, 1}) {
		t.Errorf("the waiting job acquired the images of VMs %v, want 0 and 1", vms)
	}
	if n := launcher.launched(); n != 1 {
		t.Fatalf("%d job processes launched while the second job waits, want 1", n)
	}
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: first}); err != nil {
		t.Fatal(err)
	}
	if err := r.result(t); err != nil {
		t.Fatalf("the waiting Start, once admitted: %v", err)
	}
	if !strings.Contains(log.String(), `msg="got memory" env=`+second+` need_mib=2048 `) {
		t.Errorf("the plugin's log lacks the second job's got memory:\n%s", log)
	}
	if len(specOf(t, launcher, second).GetVms()) != 2 {
		t.Error("the admitted job's spec lacks its service")
	}
}

// A service's image that fails to acquire, or that has neither an
// Entrypoint nor a Cmd, fails Start with FailedPrecondition, naming the
// service and its image, before any wait: with the memory budget full, the
// Start neither waits nor logs a wait, and launches nothing.
func TestStartFailsOnAServicesImage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		desc  string
		cache *fakeCache
		want  string
	}{
		{
			"a failed pull", &fakeCache{refErr: map[string]error{"postgres:17": errors.New("manifest unknown")}},
			"service my_db: image postgres:17: manifest unknown",
		},
		{
			"no command", &fakeCache{commandless: map[string]bool{"postgres:17": true}},
			"service my_db: image postgres:17: it has neither an Entrypoint nor a Cmd, so the service has no command to run",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			launcher := &fakeLauncher{}
			srv := newTestServer(t, tc.cache, launcher)
			log := withBudget(srv, 2048)
			c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
			full := createWithServices(t, c, "full", svc("cache", "redis:8"))
			if _, err := start(testContext(t), c, full); err != nil {
				t.Fatal(err)
			}
			id := createWithServices(t, c, "build", svc("My.DB", "postgres:17"))
			r := startInBackground(testContext(t), c, id)
			err := r.result(t)
			if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != tc.want {
				t.Errorf("Start = %v; want FailedPrecondition: %s", err, tc.want)
			}
			for _, l := range r.remaining() {
				if strings.Contains(l, "waiting") {
					t.Errorf("the failed Start sent %q", l)
				}
			}
			if strings.Contains(log.String(), "waiting for memory") {
				t.Errorf("the failed Start waited:\n%s", log)
			}
			if n := launcher.launched(); n != 1 {
				t.Errorf("%d job processes launched, want only the first job's", n)
			}
			if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
				t.Fatalf("Remove after a failed Start: %v", err)
			}
		})
	}
}

// The first image to fail cancels the others' Acquires, and Start returns
// only once each has returned, with the first failure.
func TestAFailedImageCancelsTheOthers(t *testing.T) {
	t.Parallel()
	cache := &blockingRefCache{fakeCache: &fakeCache{}, block: "redis:8", fail: "postgres:17", cancelled: make(chan struct{}), hold: make(chan struct{})}
	srv := newTestServer(t, cache.fakeCache, &fakeLauncher{})
	srv.cfg.Cache = cache
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	id := createWithServices(t, c, "build", svc("cache", "redis:8"), svc("db", "postgres:17"))
	r := startInBackground(testContext(t), c, id)
	within(t, cache.cancelled, "the blocked Acquire's cancel")
	select {
	case err := <-r.done:
		t.Fatalf("Start returned %v while an Acquire still ran", err)
	default:
	}
	close(cache.hold)
	err := r.result(t)
	if want := "service db: image postgres:17: manifest unknown"; status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != want {
		t.Errorf("Start = %v; want FailedPrecondition: %s", err, want)
	}
}

// blockingRefCache is fakeCache, but its Acquire of fail fails, and its
// Acquire of block waits for its context, closes cancelled, then waits for
// hold to close before it returns.
type blockingRefCache struct {
	*fakeCache
	block, fail     string
	cancelled, hold chan struct{}
}

func (c *blockingRefCache) Acquire(ctx context.Context, req imagecache.Request) (imagecache.Image, error) {
	switch req.Ref {
	case c.block:
		<-ctx.Done()
		close(c.cancelled)
		<-c.hold
		return imagecache.Image{}, ctx.Err()
	case c.fail:
		return imagecache.Image{}, errors.New("manifest unknown")
	}
	return c.fakeCache.Acquire(ctx, req)
}

// recordingAgent is a fake agent that records the Pings and StartServices
// it gets, in order, each StartService with its env. Its StartService sends
// on entered when that is set; with hang set, it then waits for its context
// to end, and sends on ended once it has; it fails with startErr when that
// is set. With execFails set, its Exec fails at once.
type recordingAgent struct {
	fakeAgent
	startErr       error
	hang           bool
	entered, ended chan struct{}
	execFails      bool

	mu    sync.Mutex
	calls []string
}

func (a *recordingAgent) add(call string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, call)
}

// record returns the calls a got, space-separated.
func (a *recordingAgent) record() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.calls, " ")
}

func (a *recordingAgent) Ping(ctx context.Context, req *agentv1.PingRequest) (*agentv1.PingResponse, error) {
	a.add("Ping")
	return a.fakeAgent.Ping(ctx, req)
}

func (a *recordingAgent) StartService(ctx context.Context, req *agentv1.StartServiceRequest) (*agentv1.StartServiceResponse, error) {
	a.add(fmt.Sprintf("StartService %v", req.GetEnv()))
	if a.entered != nil {
		a.entered <- struct{}{}
	}
	if a.hang {
		<-ctx.Done()
		a.ended <- struct{}{}
		return nil, ctx.Err()
	}
	if a.startErr != nil {
		return nil, a.startErr
	}
	return &agentv1.StartServiceResponse{}, nil
}

func (a *recordingAgent) Exec(req *agentv1.ExecRequest, stream agentv1.AgentService_ExecServer) error {
	if a.execFails {
		return status.Error(codes.Internal, "not the job's container's agent")
	}
	return a.fakeAgent.Exec(req, stream)
}

// silentAgent is a fake agent whose Ping never answers.
type silentAgent struct{ fakeAgent }

func (*silentAgent) Ping(ctx context.Context, _ *agentv1.PingRequest) (*agentv1.PingResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A service that exits during boot fails Start with FailedPrecondition,
// naming its VM, with its last output; the job process is killed and the
// job's memory released before Start returns.
func TestStartFailsWhenAServiceExitsDuringBoot(t *testing.T) {
	t.Parallel()
	launcher := &fakeLauncher{does: lastExitsDuringBoot}
	srv := newTestServer(t, &fakeCache{}, launcher)
	withBudget(srv, 4096)
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	id := createWithServices(t, c, "build", svc("cache", "redis:8"), svc("My.DB", "postgres:17"))
	_, err := start(testContext(t), c, id)
	if want := "VM my_db exited during boot (1)" + bootOutput; status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != want {
		t.Errorf("Start = %v; want FailedPrecondition: %q", err, want)
	}
	if launcher.alive() != 0 {
		t.Error("a failed Start left its job process running")
	}
	wantFree(t, srv, 4096)
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatalf("Remove after a failed Start: %v", err)
	}
}

// A service whose agent never answers fails Start at the ready timeout,
// naming it, with its VM's last output.
func TestStartFailsWhenAServiceNeverAnswers(t *testing.T) {
	t.Parallel()
	launcher := &fakeLauncher{vmAgents: map[int]agentv1.AgentServiceServer{1: &silentAgent{}}}
	srv := newTestServer(t, &fakeCache{}, launcher)
	srv.cfg.ReadyTimeout = 200 * time.Millisecond
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	id := createWithServices(t, c, "build", svc("cache", "redis:8"))
	_, err := start(testContext(t), c, id)
	if want := "VM cache: the agent did not answer within 200ms"; status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != want {
		t.Errorf("Start = %v; want FailedPrecondition: %q", err, want)
	}
	if launcher.alive() != 0 {
		t.Error("a failed Start left its job process running")
	}
}

// A service whose VM exits after Start doesn't end the job: the plugin
// logs it, and Exec on the job's container still runs. Only the job's
// container's death fails later calls.
func TestAServiceThatStopsLeavesTheJobRunning(t *testing.T) {
	t.Parallel()
	launcher := &fakeLauncher{}
	srv := newTestServer(t, &fakeCache{}, launcher)
	buf := &logBuffer{}
	srv.log = slog.New(slog.NewTextHandler(buf, nil))
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	id := createWithServices(t, c, "build", svc("cache", "redis:8"))
	if _, err := start(testContext(t), c, id); err != nil {
		t.Fatal(err)
	}
	launcher.report(t, id, &jobv1.Event{Event: &jobv1.Event_VmExited{VmExited: &jobv1.VMExited{Vm: 1, ExitCode: 137}}})
	waitFor(t, "the service's exit in the plugin's log", func() bool {
		return strings.Contains(buf.String(), `msg="a service's VM exited; the job runs on" env=`+id+` vm=cache exit_code=137`)
	})
	if _, err := execAll(t, c, &pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"true"}, Workdir: "/"}); err != nil {
		t.Errorf("Exec after the service stopped: %v", err)
	}
	launcher.report(t, id, &jobv1.Event{Event: &jobv1.Event_VmExited{VmExited: &jobv1.VMExited{Vm: 0, ExitCode: 1}}})
	waitFor(t, "the job's container's exit to fail Exec", func() bool {
		_, err := execAll(t, c, &pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"true"}, Workdir: "/"})
		return status.Code(err) == codes.FailedPrecondition
	})
}

// A service whose command can't start fails Start with FailedPrecondition,
// naming the service and quoting the agent's message, with its VM's last
// output, which the plugin's log leaves out; StartService is sent once, the
// service's VM is never reported ready, and the job process is killed and
// the job's memory released before Start returns.
func TestStartFailsWhenAServiceCannotStart(t *testing.T) {
	t.Parallel()
	agent := &recordingAgent{startErr: status.Error(codes.FailedPrecondition, "service entrypoint.sh: no such file or directory")}
	launcher := &fakeLauncher{vmAgents: map[int]agentv1.AgentServiceServer{1: agent}, bootOutput: true}
	srv := newTestServer(t, &fakeCache{}, launcher)
	log := withBudget(srv, 2048)
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	id := createWithServices(t, c, "build", svc("cache", "redis:8"))
	r := startInBackground(testContext(t), c, id)
	err := r.result(t)
	const summary = "VM cache: its service did not start: service entrypoint.sh: no such file or directory"
	if want := summary + bootOutput; status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != want {
		t.Errorf("Start = %v; want FailedPrecondition: %q", err, want)
	}
	wantLine(t, log.String(), "start failed", regexp.QuoteMeta(`err="`+summary+`"`))
	if strings.Contains(log.String(), "console line 25") || strings.Contains(log.String(), "log line 10") {
		t.Errorf("the plugin's log has the service's VM's output:\n%s", log)
	}
	wantStartFailedClean(t, srv, c, id, launcher, r, agent, "StartService map[]")
}

// A service's StartService that outlasts the ready timeout fails Start
// naming the service; its call is cancelled, and the job process is killed
// and the job's memory released before Start returns.
func TestStartFailsWhenAServiceHangsStarting(t *testing.T) {
	t.Parallel()
	agent := &recordingAgent{hang: true, ended: make(chan struct{}, 1)}
	launcher := &fakeLauncher{vmAgents: map[int]agentv1.AgentServiceServer{1: agent}, bootOutput: true}
	srv := newTestServer(t, &fakeCache{}, launcher)
	withBudget(srv, 2048)
	srv.cfg.ReadyTimeout = 200 * time.Millisecond
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	id := createWithServices(t, c, "build", svc("cache", "redis:8"))
	r := startInBackground(testContext(t), c, id)
	err := r.result(t)
	if want := "VM cache: the agent did not start its service within 200ms" + bootOutput; status.Code(err) != codes.FailedPrecondition ||
		status.Convert(err).Message() != want {
		t.Errorf("Start = %v; want FailedPrecondition: %q", err, want)
	}
	within(t, agent.ended, "the hung StartService's end")
	wantStartFailedClean(t, srv, c, id, launcher, r, agent, "StartService map[]")
}

// gRPC ends a call at its deadline on its own, through the server's
// grpc-timeout or the stream's reset, so StartService can fail with
// DeadlineExceeded before the ready timeout's context reports its end. That
// is the ready timeout, never a lost connection: Start fails at once, not
// after the lost agent grace.
func TestStartFailsWhenAServiceStartOutlivesItsDeadline(t *testing.T) {
	t.Parallel()
	agent := &recordingAgent{startErr: status.Error(codes.DeadlineExceeded, "context deadline exceeded")}
	launcher := &fakeLauncher{vmAgents: map[int]agentv1.AgentServiceServer{1: agent}, bootOutput: true}
	srv := newTestServer(t, &fakeCache{}, launcher)
	withBudget(srv, 2048)
	// Longer than result waits, short enough that a Start that waits it
	// out lets the test end.
	srv.cfg.LostAgentGrace = time.Minute
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	id := createWithServices(t, c, "build", svc("cache", "redis:8"))
	r := startInBackground(testContext(t), c, id)
	err := r.result(t)
	if want := "VM cache: the agent did not start its service within 5s" + bootOutput; status.Code(err) != codes.FailedPrecondition ||
		status.Convert(err).Message() != want {
		t.Errorf("Start = %v; want FailedPrecondition: %q", err, want)
	}
	wantStartFailedClean(t, srv, c, id, launcher, r, agent, "StartService map[]")
}

// A service's VM that exits around its StartService, while the call is in
// flight or once it failed on the broken connection, fails Start once, with
// the VM's exit and its last output. A failed connection with no exit fails
// Start once the lost agent grace has passed, naming the service.
func TestStartFailsWhenAServiceExitsStarting(t *testing.T) {
	t.Parallel()
	exit := &jobv1.Event{Event: &jobv1.Event_VmExited{VmExited: &jobv1.VMExited{
		Vm: 1, ExitCode: 1, ConsoleTail: bootConsole, LogTail: bootLog[len(bootLog)-4096:],
	}}}
	broken := status.Error(codes.Unavailable, "connection reset")
	for _, tc := range []struct {
		desc  string
		agent *recordingAgent
		exits bool
		want  string
	}{
		{"during the call", &recordingAgent{hang: true}, true, "VM cache exited during boot (1)"},
		{"after the call failed", &recordingAgent{startErr: broken}, true, "VM cache exited during boot (1)"},
		{
			"never", &recordingAgent{startErr: broken}, false,
			"VM cache: the agent's connection failed as it started its service: rpc error: code = Unavailable desc = connection reset",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			tc.agent.entered, tc.agent.ended = make(chan struct{}, 1), make(chan struct{}, 1)
			launcher := &fakeLauncher{vmAgents: map[int]agentv1.AgentServiceServer{1: tc.agent}, bootOutput: true}
			srv := newTestServer(t, &fakeCache{}, launcher)
			withBudget(srv, 2048)
			srv.cfg.LostAgentGrace = 100 * time.Millisecond
			if tc.exits {
				// The exit must win however long it takes.
				srv.cfg.LostAgentGrace = time.Hour
			}
			c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
			id := createWithServices(t, c, "build", svc("cache", "redis:8"))
			r := startInBackground(testContext(t), c, id)
			within(t, tc.agent.entered, "the StartService")
			if tc.exits {
				launcher.exitVM(t, id, 1, exit)
			}
			err := r.result(t)
			if want := tc.want + bootOutput; status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != want {
				t.Errorf("Start = %v; want FailedPrecondition: %q", err, want)
			}
			wantStartFailedClean(t, srv, c, id, launcher, r, tc.agent, "StartService map[]")
		})
	}
}

// wantStartFailedClean checks what a Start r of the job id, on c, that
// failed on the service of agent, the job's only one, left: agent got Ping then one StartService,
// start; the service's VM was never reported ready; the job process is
// killed and the job's 2048 MiB free; and Remove leaves no file of the job.
func wantStartFailedClean(t *testing.T, srv *Server, c pluginv1alpha.BackendPluginClient, id string, launcher *fakeLauncher, r *runningStart, agent *recordingAgent, start string) {
	t.Helper()
	if got := agent.record(); got != "Ping "+start {
		t.Errorf("the service's agent got %q, want Ping then %s", got, start)
	}
	for _, l := range r.remaining() {
		if strings.Contains(l, "VM cache ") {
			t.Errorf("the failed Start sent %q", l)
		}
	}
	if launcher.alive() != 0 {
		t.Error("a failed Start left its job process running")
	}
	wantFree(t, srv, 2048)
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatalf("Remove after a failed Start: %v", err)
	}
	if left := jobFiles(t, srv, id); len(left) != 0 {
		t.Errorf("Remove left %q", left)
	}
}

// matchEach fails t unless lines and patterns pair up one to one, whatever
// the order of lines.
func matchEach(t *testing.T, kind string, lines []string, patterns ...string) {
	t.Helper()
	if len(lines) != len(patterns) {
		t.Errorf("%s lines %q; want %d", kind, lines, len(patterns))
	}
	for _, p := range patterns {
		re := regexp.MustCompile(p)
		if n := len(slices.DeleteFunc(slices.Clone(lines), func(l string) bool { return !re.MatchString(l) })); n != 1 {
			t.Errorf("%s lines %q; want one matching %s, got %d", kind, lines, p, n)
		}
	}
}
