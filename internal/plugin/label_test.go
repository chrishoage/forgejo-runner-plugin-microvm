// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/config"
	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/imagecache"
)

// The profiles the tests below define: big sets every key, and cpusOnly
// only cpus.
var (
	bigProfile      = config.Profile{CPUs: 3, MemoryMiB: 1536, ScratchGiB: 4, ScratchPaths: []string{"/var/lib/docker", "/tmp"}, ForcePull: true}
	cpusOnlyProfile = config.Profile{CPUs: 8, MemoryMiB: 1024, ScratchGiB: 10}
	definedDefault  = config.Profile{CPUs: 1, MemoryMiB: 512, ScratchGiB: 2}
)

// jobOf returns the job id names in srv's table.
func jobOf(t *testing.T, srv *Server, id string) *job {
	t.Helper()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	j, ok := srv.jobs[id]
	if !ok {
		t.Fatalf("no job %s", id)
	}
	return j
}

// Each well-formed label form picks its image and profile: no profile is
// default, and default always exists, with the built-in values when the
// config defines none (CFG-4).
func TestCreatePicksTheLabelsProfile(t *testing.T) {
	t.Parallel()
	builtin := config.BuiltinProfile()
	withDefault := config.Profiles{"big": bigProfile, "default": definedDefault}
	without := config.Profiles{"big": bigProfile, "cpus-only": cpusOnlyProfile}
	for _, tc := range []struct {
		name     string
		profiles config.Profiles
		arg      string
		image    string
		profile  string
		want     config.Profile
	}{
		{"no profile, no default", without, testImage, testImage, "default", builtin},
		{"no profile, a default", withDefault, testImage, testImage, "default", definedDefault},
		{"no profiles at all", nil, testImage, testImage, "default", builtin},
		{"a profile", without, testImage + ";profile=big", testImage, "big", bigProfile},
		{"default named, none defined", without, testImage + ";profile=default", testImage, "default", builtin},
		{"default named and defined", withDefault, testImage + ";profile=default", testImage, "default", definedDefault},
		// Only cpus set: the built-in memory and scratch size, not
		// default's (CFG-5).
		{
			"cpus only",
			config.Profiles{"cpus-only": cpusOnlyProfile, "default": definedDefault},
			testImage + ";profile=cpus-only", testImage, "cpus-only", cpusOnlyProfile,
		},
		{"leading //", without, "//" + testImage + ";profile=big", testImage, "big", bigProfile},
		{"only one // stripped", without, "////" + testImage, "//" + testImage, "default", builtin},
		{
			"a digest reference", without, "alpine@sha256:" + strings.Repeat("0", 64) + ";profile=big",
			"alpine@sha256:" + strings.Repeat("0", 64), "big", bigProfile,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newTestServer(t, &fakeCache{}, &fakeLauncher{})
			srv.SetProfiles(tc.profiles)
			c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
			resp, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{LabelArg: tc.arg})
			if err != nil {
				t.Fatal(err)
			}
			j := jobOf(t, srv, resp.GetEnvironmentId())
			if j.image != tc.image || j.profileName != tc.profile || !reflect.DeepEqual(j.profile, tc.want) {
				t.Errorf("job has image %q, profile %q %+v; want %q, %q %+v",
					j.image, j.profileName, j.profile, tc.image, tc.profile, tc.want)
			}
		})
	}
}

// A malformed label argument, an unknown profile, and an empty image part
// with no container: image each fail Create with InvalidArgument, naming
// the label argument where it is at fault.
func TestCreateRefusesTheLabel(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeCache{}, &fakeLauncher{})
	srv.SetProfiles(config.Profiles{"big": bigProfile})
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	for _, tc := range []struct {
		arg, container, want string
	}{
		{testImage + ";", "", `label "` + testImage + `;": nothing follows the ";"`},
		{";", "", `label ";": nothing follows the ";"`},
		{testImage + ";profile=", "", `label "` + testImage + `;profile=": profile= names no profile`},
		{testImage + ";size=big", "", `label "` + testImage + `;size=big": "size=big": only profile=<name> may follow the image`},
		{testImage + ";profile", "", `"profile": only profile=<name> may follow the image`},
		{testImage + ";Profile=big", "", `"Profile=big": only profile=<name> may follow the image`},
		{testImage + "; profile=big", "", `" profile=big": only profile=<name> may follow the image`},
		{testImage + ";;profile=big", "", `"": only profile=<name> may follow the image`},
		{testImage + ";profile=big;", "", `label "` + testImage + `;profile=big;": ";" follows the profile`},
		{testImage + ";profile=big;profile=big", "", `";profile=big" follows the profile`},
		{testImage + ";profile=big;cpus=2", "", `";cpus=2" follows the profile`},
		// A malformed label fails even with a container: image.
		{";size=big", testImage, `label ";size=big": "size=big": only profile=<name> may follow the image`},
		// CFG-6, with the message decided in #67; matching is exact.
		{testImage + ";profile=small", "", `label "` + testImage + `;profile=small": unknown profile "small"`},
		{testImage + ";profile=Big", "", `label "` + testImage + `;profile=Big": unknown profile "Big"`},
		{";profile=small", testImage, `label ";profile=small": unknown profile "small"`},
		// An empty image part is well formed, but the job must then bring
		// its own image (PROTO-5).
		{";profile=big", "", "no image: neither the label nor the job's container: names one"},
		{"", "", "no image: neither the label nor the job's container: names one"},
		{"//", "", "no image: neither the label nor the job's container: names one"},
	} {
		_, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{LabelArg: tc.arg, Image: tc.container})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), tc.want) {
			t.Errorf("Create(label %q, container %q) = %v; want InvalidArgument with %q", tc.arg, tc.container, err, tc.want)
		}
	}
	if n := len(srv.jobs); n != 0 {
		t.Errorf("%d jobs in the table after refused Creates", n)
	}
}

// An empty image part with a container: image is a well-formed label: it
// reaches the choice of image, which today still refuses a container:
// image (SHORTCUT(#6) in jobImage, which #50 replaces).
func TestCreateEmptyImagePartWithAContainerImage(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeCache{}, &fakeLauncher{})
	srv.SetProfiles(config.Profiles{"big": bigProfile})
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	_, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{LabelArg: ";profile=big", Image: testImage})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "container: image yet") {
		t.Errorf("Create = %v; want the container: image refusal, not a label or image error", err)
	}
}

// Start puts the job's profile's CPU count, memory and scratch size in the
// job spec (VM-4, VM-5), and acquires its image with the profile's
// force_pull (IMG-6); a job created before the profiles change keeps its
// copy, even one not yet started (CFG-7).
func TestStartUsesTheJobsProfile(t *testing.T) {
	t.Parallel()
	cache, launcher := &fakeCache{}, &fakeLauncher{}
	srv := newTestServer(t, cache, launcher)
	srv.SetProfiles(config.Profiles{"big": bigProfile})
	c := pluginv1alpha.NewBackendPluginClient(serve(t, srv))
	createWith := func(arg string) string {
		t.Helper()
		resp, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{LabelArg: arg})
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetEnvironmentId()
	}
	big := createWith(testImage + ";profile=big")
	plain := createWith(testImage)
	// The config changes: big grows, a default appears.
	grown := config.Profile{CPUs: 16, MemoryMiB: 8192, ScratchGiB: 100, ScratchPaths: []string{"/srv"}}
	srv.SetProfiles(config.Profiles{"big": grown, "default": definedDefault})
	bigAfter := createWith(testImage + ";profile=big")
	plainAfter := createWith(testImage)
	for _, tc := range []struct {
		id   string
		want config.Profile
	}{
		{big, bigProfile},
		{plain, config.BuiltinProfile()},
		{bigAfter, grown},
		{plainAfter, definedDefault},
	} {
		if _, err := start(testContext(t), c, tc.id); err != nil {
			t.Fatal(err)
		}
		vm := specOf(t, launcher, tc.id).GetVms()[0]
		if vm.GetCpus() != tc.want.CPUs || vm.GetMemoryMib() != tc.want.MemoryMiB ||
			vm.GetScratchSizeBytes() != tc.want.ScratchGiB<<30 ||
			!slices.Equal(vm.GetBoot().GetScratchPaths(), tc.want.ScratchPaths) {
			t.Errorf("job %s: VM has %d CPUs, %d MiB, %d scratch bytes, scratch paths %q; want %+v",
				tc.id, vm.GetCpus(), vm.GetMemoryMib(), vm.GetScratchSizeBytes(), vm.GetBoot().GetScratchPaths(), tc.want)
		}
		if req := acquiredBy(t, cache, tc.id); req.ForcePull != tc.want.ForcePull {
			t.Errorf("job %s: Acquire(%+v); want ForcePull %v", tc.id, req, tc.want.ForcePull)
		}
	}
}

// acquiredBy returns the one request cache's Acquire had for job id.
func acquiredBy(t *testing.T, cache *fakeCache, id string) imagecache.Request {
	t.Helper()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	var reqs []imagecache.Request
	for _, req := range cache.acquired {
		if req.Job == id {
			reqs = append(reqs, req)
		}
	}
	if len(reqs) != 1 {
		t.Fatalf("job %s acquired %+v, want one request", id, reqs)
	}
	return reqs[0]
}

// specOf returns the job spec launcher launched for job id.
func specOf(t *testing.T, launcher *fakeLauncher, id string) *jobv1.Spec {
	t.Helper()
	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	for _, s := range launcher.specs {
		if s.GetJob() == id {
			return s
		}
	}
	t.Fatalf("no job spec for %s", id)
	return nil
}
