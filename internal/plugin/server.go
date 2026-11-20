// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
)

// Name is the plugin's name in Capabilities.
const Name = "runner-microvm"

// The job's paths and platform, as Create returns them (PROTO-7).
const (
	rootPath      = "/"
	actPath       = "/var/run/act"
	toolCachePath = "/opt/hostedtoolcache"
	tempPath      = "/tmp"
	platformOS    = "Linux"
	platformArch  = "X64"
)

// Server is the runner's BackendPlugin service and the job table.
type Server struct {
	pluginv1alpha.UnimplementedBackendPluginServer

	log *slog.Logger

	mu   sync.Mutex
	jobs map[string]*job
}

// job is one job in the table, from Create to Remove.
type job struct {
	id    string
	image string

	// ctx is cancelled when Remove starts, so a call still unwinding on the
	// job ends at once (PROTO-13).
	ctx    context.Context
	cancel context.CancelFunc

	// mu is held by every call on the job for its whole length, so Remove
	// waits until a cancelled call has unwound before it tears the job down.
	// A call that holds it releases it promptly once ctx is cancelled, also
	// while it is blocked on the runner's stream, so Remove never waits
	// long.
	mu sync.Mutex
	// removed is set, under mu, by Remove: a call that found the job just
	// before Remove took it out of the table sees it and fails with NotFound.
	removed bool
}

// NewServer returns a server with an empty job table.
func NewServer(log *slog.Logger) *Server {
	return &Server{log: log, jobs: map[string]*job{}}
}

// Capabilities names the plugin.
func (s *Server) Capabilities(context.Context, *pluginv1alpha.CapabilitiesRequest) (*pluginv1alpha.CapabilitiesResponse, error) {
	return &pluginv1alpha.CapabilitiesResponse{Name: Name}, nil
}

// Create picks the job's image from the label argument and returns the job's
// environment id, paths and platform. It boots nothing (PROTO-3).
func (s *Server) Create(_ context.Context, req *pluginv1alpha.CreateRequest) (*pluginv1alpha.CreateResponse, error) {
	image, err := createImage(req)
	if err != nil {
		return nil, err
	}
	j, err := s.add(image)
	if err != nil {
		return nil, err
	}
	s.log.Info("created", "job", j.id, "name", req.GetName(), "image", image)
	return &pluginv1alpha.CreateResponse{
		EnvironmentId: j.id,
		RootPath:      rootPath,
		ActPath:       actPath,
		ToolCachePath: toolCachePath,
		TempPath:      tempPath,
		Os:            platformOS,
		Arch:          platformArch,
	}, nil
}

// createImage returns the image a Create asks for, or InvalidArgument.
func createImage(req *pluginv1alpha.CreateRequest) (string, error) {
	if len(req.GetServices()) > 0 {
		// SHORTCUT(#6): services are refused (PROTO-6) until #6 decides
		// what the plugin does with them.
		return "", status.Error(codes.InvalidArgument,
			"runner-microvm does not run services: the plugin protocol passes no options to configure a service's VM")
	}
	if req.GetImage() != "" {
		// SHORTCUT(#6): a job's container: image is refused, not used
		// in place of the label's (PROTO-4).
		return "", status.Error(codes.InvalidArgument, "runner-microvm does not take a job's container: image yet")
	}
	image := req.GetLabelArg()
	if image == "" {
		return "", status.Error(codes.InvalidArgument, "no image: the label names none")
	}
	if strings.Contains(image, ";") {
		// SHORTCUT(#8): no profiles, so nothing may follow the image.
		return "", status.Errorf(codes.InvalidArgument, "label argument %q: nothing may follow the image yet", image)
	}
	return image, nil
}

// add puts a new job in the table, under a fresh environment id.
func (s *Server) add(image string) (*job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		id, err := newID()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "new environment id: %v", err)
		}
		if _, taken := s.jobs[id]; taken {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		j := &job{id: id, image: image, ctx: ctx, cancel: cancel}
		s.jobs[id] = j
		return j, nil
	}
}

// newID returns an environment id: a short prefix and 8 random hex digits.
// The plugin chooses it, so it is safe as a path component.
func newID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "job-" + hex.EncodeToString(b), nil
}

func notFound(id string) error {
	return status.Errorf(codes.NotFound, "no environment %q", id)
}

// Remove takes the job out of the table and tears it down. It cancels any
// call still unwinding on the job first, and waits for it (PROTO-13).
func (s *Server) Remove(_ context.Context, req *pluginv1alpha.RemoveRequest) (*pluginv1alpha.RemoveResponse, error) {
	id := req.GetEnvironmentId()
	s.mu.Lock()
	j, ok := s.jobs[id]
	delete(s.jobs, id)
	s.mu.Unlock()
	if !ok {
		return nil, notFound(id)
	}
	j.cancel()
	j.mu.Lock()
	defer j.mu.Unlock()
	j.removed = true
	s.log.Info("removed", "job", id)
	return &pluginv1alpha.RemoveResponse{}, nil
}
