// SPDX-License-Identifier: GPL-3.0-or-later

// Package cmd_test enforces the boundaries between the three programs: what
// each binary may, must and must not depend on, transitively
// (docs/architecture.md, Boundary enforcement).
package cmd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const module = "github.com/chrishoage/forgejo-runner-plugin-microvm"

// Third-party modules the boundaries name.
const (
	purego         = "github.com/ebitengine/purego"
	gvisor         = "gvisor.dev/gvisor"
	gvisorTapVsock = "github.com/containers/gvisor-tap-vsock"
	grpc           = "google.golang.org/grpc"
	protobuf       = "google.golang.org/protobuf"
	// forgejo-runner's module: only the protocol test may use it.
	forgejoRunner = "code.forgejo.org/forgejo/runner"
)

// The binaries as they ship (`just payload`, `just build`): every one with
// CGO_ENABLED=0, the plugin also with -tags payload. The graph is checked for
// the shipped build, not the test process's own (cgo on under -race, no
// tags), because a file behind //go:build !cgo or payload is only in one of
// them.
var (
	pluginBuild = build{tags: "payload"}
	jobBuild    = build{}
	agentBuild  = build{}
)

// build is how one binary is built.
type build struct {
	tags string // comma-separated, as -tags takes them
}

// shippedEnv returns the environment `go list` runs in: the test's own, with
// everything that selects files pinned to the shipped builds. Each pinned
// value, like each build's tags, is a constant in this file, so go test's
// cache, which can't see what `go list` read from the environment, keys on
// them through the test binary.
func shippedEnv() []string {
	return append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH=amd64",
		// GOFLAGS could add -tags; the shipped builds set none.
		"GOFLAGS=",
	)
}

// The plugin binary never loads libkrun, runs a job network, or contains job
// process or agent code. No binary contains forgejo-runner's module: the
// protocol test alone uses it.
func TestPluginDependencies(t *testing.T) {
	t.Parallel()
	deps := listDeps(t, module+"/cmd/runner-microvm", pluginBuild)
	for _, p := range forbidden(deps, []string{
		purego, gvisor, gvisorTapVsock, module + "/internal/job", module + "/internal/agent", forgejoRunner,
	}) {
		t.Errorf("the plugin binary depends on %s", p)
	}
}

// The job binary does depend on purego and gvisor-tap-vsock, so a renamed
// import path can't make the plugin's check pass silently.
func TestJobDependencies(t *testing.T) {
	t.Parallel()
	deps := listDeps(t, module+"/cmd/runner-microvm-job", jobBuild)
	for _, m := range missing(deps, []string{purego, gvisorTapVsock}) {
		t.Errorf("the job binary does not depend on %s", m)
	}
	for _, p := range forbidden(deps, []string{forgejoRunner}) {
		t.Errorf("the job binary depends on %s", p)
	}
}

// The agent binary depends only on the standard library, grpc, protobuf and
// what they bring in, and its own allow-listed packages.
func TestAgentDependencies(t *testing.T) {
	t.Parallel()
	deps := listDeps(t, module+"/cmd/runner-microvm-agent", agentBuild)
	allow := allowList{
		module: module,
		own: []string{
			module + "/cmd/runner-microvm-agent",
			module + "/internal/agent",
			module + "/internal/logging",
			module + "/internal/secret",
			module + "/internal/gen/runnermicrovm/agent/v1",
		},
		via: []string{grpc, protobuf},
	}
	for _, v := range allow.outside(deps) {
		t.Errorf("the agent binary is outside its allow-list: %s", v)
	}
}

func TestMatches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path, prefix string
		want         bool
	}{
		{"example.com/m/internal/agent", "example.com/m/internal/agent", true},
		{"example.com/m/internal/agent/boot", "example.com/m/internal/agent", true},
		{"example.com/m/internal/agentclient", "example.com/m/internal/agent", false},
		{"example.com/m/internal", "example.com/m/internal/agent", false},
	}
	for _, tt := range tests {
		if got := matches(tt.path, tt.prefix); got != tt.want {
			t.Errorf("matches(%q, %q) = %v, want %v", tt.path, tt.prefix, got, tt.want)
		}
	}
}

func TestForbidden(t *testing.T) {
	t.Parallel()
	deps := graph(
		pkg{path: "example.com/m/cmd/a", imports: []string{"example.com/m/internal/agentclient", "fmt"}},
		pkg{path: "example.com/m/internal/agentclient", imports: []string{"example.com/bad/sub"}},
		pkg{path: "example.com/bad/sub"},
		pkg{path: "fmt", standard: true},
	)
	got := forbidden(deps, []string{"example.com/m/internal/agent", "example.com/bad"})
	if want := []string{"example.com/bad/sub"}; !slices.Equal(got, want) {
		t.Errorf("forbidden = %v, want %v", got, want)
	}
	if got := forbidden(deps, []string{"example.com/other"}); len(got) != 0 {
		t.Errorf("forbidden with no match = %v, want none", got)
	}
}

func TestMissing(t *testing.T) {
	t.Parallel()
	deps := graph(
		pkg{path: "example.com/m/cmd/a", imports: []string{"example.com/lib/sub"}},
		pkg{path: "example.com/lib/sub"},
	)
	got := missing(deps, []string{"example.com/lib", "example.com/renamed"})
	if want := []string{"example.com/renamed"}; !slices.Equal(got, want) {
		t.Errorf("missing = %v, want %v", got, want)
	}
}

func TestOutsideAllowList(t *testing.T) {
	t.Parallel()
	const mod = "example.com/m"
	allow := allowList{
		module: mod,
		own:    []string{mod + "/cmd/agent", mod + "/internal/agent"},
		via:    []string{"example.com/grpc"},
	}
	tests := []struct {
		name string
		deps map[string]pkg
		want []string
	}{
		{
			name: "allowed",
			deps: graph(
				pkg{path: mod + "/cmd/agent", imports: []string{mod + "/internal/agent", "os"}},
				pkg{path: mod + "/internal/agent", imports: []string{"example.com/grpc/server"}},
				pkg{path: "example.com/grpc/server", imports: []string{"example.com/x/net"}},
				// Brought in by grpc, so allowed.
				pkg{path: "example.com/x/net"},
				pkg{path: "os", standard: true},
			),
		},
		{
			name: "own package not on the list",
			deps: graph(
				pkg{path: mod + "/cmd/agent", imports: []string{mod + "/internal/job"}},
				pkg{path: mod + "/internal/job"},
			),
			want: []string{mod + "/internal/job"},
		},
		{
			name: "third party imported by our own code",
			deps: graph(
				pkg{path: mod + "/cmd/agent", imports: []string{mod + "/internal/agent"}},
				pkg{path: mod + "/internal/agent", imports: []string{"example.com/x/net"}},
				pkg{path: "example.com/x/net"},
			),
			want: []string{mod + "/internal/agent imports example.com/x/net"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := allow.outside(tt.deps); !slices.Equal(got, tt.want) {
				t.Errorf("outside = %v, want %v", got, tt.want)
			}
		})
	}
}

// graph indexes packages by import path.
func graph(pkgs ...pkg) map[string]pkg {
	g := make(map[string]pkg, len(pkgs))
	for _, p := range pkgs {
		g[p.path] = p
	}
	return g
}

// pkg is one package in a binary's dependency graph.
type pkg struct {
	path     string
	standard bool
	imports  []string
}

// listDeps returns every package main depends on, itself included, as
// `go list -deps` reports it for build b.
func listDeps(t *testing.T, main string, b build) map[string]pkg {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	// -e, because with -tags payload but no payload built (`just test`
	// builds none), internal/payload's embed patterns match no file. That
	// error leaves its imports intact; any other error still fails below.
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-deps", "-tags="+b.tags,
		"-json=ImportPath,Standard,Imports,Dir,GoFiles,Module,Error", main)
	cmd.Env = shippedEnv()
	cmd.WaitDelay = 10 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", main, err, stderr.String())
	}
	deps := map[string]pkg{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p struct {
			ImportPath string
			Standard   bool
			Imports    []string
			Dir        string
			GoFiles    []string
			Module     *struct {
				Main  bool
				GoMod string
			}
			Error *struct{ Err string }
		}
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("go list -deps %s: decoding its output: %v", main, err)
		}
		if p.Error != nil && !isMissingEmbed(p.Error.Err) {
			t.Fatalf("go list -deps %s: %s: %s", main, p.ImportPath, p.Error.Err)
		}
		deps[p.ImportPath] = pkg{path: p.ImportPath, standard: p.Standard, imports: p.Imports}
		if p.Module != nil && p.Module.Main {
			// go test caches a passing result by the files the test opens,
			// and can't see what `go list` read. Opening the inputs here
			// makes a change to any of them rerun the test.
			inputs := []string{p.Dir, p.Module.GoMod, filepath.Join(filepath.Dir(p.Module.GoMod), "go.sum")}
			for _, f := range p.GoFiles {
				inputs = append(inputs, filepath.Join(p.Dir, f))
			}
			touch(t, inputs)
		}
	}
	if _, ok := deps[main]; !ok {
		t.Fatalf("go list -deps %s: %s is not in its own output", main, main)
	}
	return deps
}

// isMissingEmbed reports whether err is only a //go:embed pattern that
// matches no file, such as the payload before `just payload` builds it.
func isMissingEmbed(err string) bool {
	return strings.Contains(err, ": pattern ") && strings.HasSuffix(err, ": no matching files found")
}

// touch opens and closes each path, so go test's cache records it as an
// input: a file by its content, a directory by its listing.
func touch(t *testing.T, paths []string) {
	t.Helper()
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("recording %s as a test input: %v", path, err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("recording %s as a test input: %v", path, err)
		}
	}
}

// matches reports whether path is prefix or a package below it.
func matches(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func matchesAny(path string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(prefix string) bool { return matches(path, prefix) })
}

// forbidden returns the packages in deps under any of prefixes, sorted.
func forbidden(deps map[string]pkg, prefixes []string) []string {
	var found []string
	for path := range deps {
		if matchesAny(path, prefixes) {
			found = append(found, path)
		}
	}
	slices.Sort(found)
	return found
}

// missing returns the prefixes with no package in deps, in order.
func missing(deps map[string]pkg, prefixes []string) []string {
	var absent []string
	for _, prefix := range prefixes {
		found := false
		for path := range deps {
			if matches(path, prefix) {
				found = true
				break
			}
		}
		if !found {
			absent = append(absent, prefix)
		}
	}
	return absent
}

// allowList is what a binary may depend on: the standard library, the
// module's own packages under own, the packages under via, and whatever those
// bring in.
type allowList struct {
	module string
	own    []string
	via    []string
}

// outside returns what in deps breaks the allow-list, sorted: each of the
// module's packages not under own, and each import by an allowed own package
// of anything but the standard library, own and via. Together these mean any
// other third-party package can only have come in through via.
func (a allowList) outside(deps map[string]pkg) []string {
	var found []string
	for path, p := range deps {
		if !matches(path, a.module) {
			continue
		}
		if !matchesAny(path, a.own) {
			found = append(found, path)
			continue
		}
		for _, imp := range p.imports {
			if deps[imp].standard || matchesAny(imp, a.own) || matchesAny(imp, a.via) {
				continue
			}
			if matches(imp, a.module) {
				continue // reported as a package of its own
			}
			found = append(found, path+" imports "+imp)
		}
	}
	slices.Sort(found)
	return found
}
