// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/agentclient"
	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/joblaunch"
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

// Config is what the server needs from startup.
type Config struct {
	Dirs Dirs
	// LibDir holds libkrun and libkrunfw; empty loads them by soname.
	LibDir string
	// JobBinary is the extracted job binary's path.
	JobBinary string
	// MkfsExt4 is the host's mkfs.ext4, which formats each scratch disk.
	MkfsExt4 string
	// Agent is the agent binary, written into each VM's root.
	Agent []byte
	// Cache and Launcher are the plugin's two seams.
	Cache    ImageCache
	Launcher JobLauncher
	// ReadyTimeout is how long Start waits for the agent to answer Ping.
	ReadyTimeout time.Duration
}

// Server is the runner's BackendPlugin service and the job table.
type Server struct {
	pluginv1alpha.UnimplementedBackendPluginServer

	log *slog.Logger
	cfg Config
	// runtime addresses the agent sockets under the runtime directory.
	runtime   *sockDir
	closeOnce sync.Once

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
	// while it is blocked on the runner's stream (await), so Remove never
	// waits long. Everything below is guarded by it.
	mu sync.Mutex
	// removed is set by Remove: a call that found the job just before
	// Remove took it out of the table sees it and fails with NotFound.
	removed bool
	// started is set when Start begins, and startErr when it failed: a
	// job is started once only, and never again after a failed Start.
	started  bool
	startErr error
	// proc is the job process, from Start's launch until teardown.
	proc *joblaunch.Process
	// agent is the job's container's agent, once it answered Ping.
	agent *agentclient.Client
}

// NewServer returns a server with an empty job table. cfg.Dirs.Runtime must
// exist. Close releases it.
func NewServer(log *slog.Logger, cfg Config) (*Server, error) {
	rt, err := openSockDir(cfg.Dirs.Runtime)
	if err != nil {
		return nil, err
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = agentclient.ReadyTimeout
	}
	return &Server{log: log, cfg: cfg, runtime: rt, jobs: map[string]*job{}}, nil
}

// Close tears down every job still in the table, and releases the server.
// A second Close does nothing more.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	jobs := s.jobs
	s.jobs = map[string]*job{}
	s.mu.Unlock()
	var errs []error
	for _, j := range jobs {
		errs = append(errs, s.remove(ctx, j))
	}
	s.closeOnce.Do(func() { errs = append(errs, s.runtime.Close()) })
	return errors.Join(errs...)
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

// lock returns the job id names, locked, or NotFound. The caller unlocks it.
func (s *Server) lock(id string) (*job, error) {
	s.mu.Lock()
	j, ok := s.jobs[id]
	s.mu.Unlock()
	if !ok {
		return nil, notFound(id)
	}
	j.mu.Lock()
	if j.removed {
		j.mu.Unlock()
		return nil, notFound(id)
	}
	return j, nil
}

// callContext returns ctx, also cancelled when Remove starts on j.
func callContext(ctx context.Context, j *job) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(j.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

// await runs op, a call on the runner's stream, and returns its error, or
// ctx's status as soon as ctx ends while op still blocks. A call holding the
// job then releases it at once when Remove cancels it, even while the runner
// neither reads nor writes. The call must return at once after a ctx error:
// op is still running then, and ends when gRPC cancels the stream, as the
// handler returns.
func await(ctx context.Context, op func() error) error {
	done := make(chan error, 1)
	go func() { done <- op() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

// Remove takes the job out of the table and tears it down. It cancels any
// call still unwinding on the job first, and waits for it (PROTO-13).
func (s *Server) Remove(ctx context.Context, req *pluginv1alpha.RemoveRequest) (*pluginv1alpha.RemoveResponse, error) {
	id := req.GetEnvironmentId()
	s.mu.Lock()
	j, ok := s.jobs[id]
	delete(s.jobs, id)
	s.mu.Unlock()
	if !ok {
		return nil, notFound(id)
	}
	if err := s.remove(ctx, j); err != nil {
		return nil, status.Errorf(codes.Internal, "remove %s: %v", id, err)
	}
	return &pluginv1alpha.RemoveResponse{}, nil
}

// remove tears down j, which is out of the table: it ends the job process,
// which ends the VM, and deletes the job's files.
func (s *Server) remove(ctx context.Context, j *job) error {
	j.cancel()
	j.mu.Lock()
	defer j.mu.Unlock()
	j.removed = true
	// The kill can't be left half done: it returns once the group is
	// reaped, moments after SIGKILL.
	err := s.stop(context.WithoutCancel(ctx), j)
	err = errors.Join(err,
		s.cfg.Cache.Release(context.WithoutCancel(ctx), j.id),
		removeTree(s.jobStateDir(j.id)),
		removeTree(filepath.Join(s.cfg.Dirs.Runtime, j.id)),
	)
	s.log.Info("removed", "job", j.id, "err", err)
	return err
}

// stop closes j's agent connection and kills its job process, if any, and
// waits until its process group is reaped. j.mu is held.
func (s *Server) stop(ctx context.Context, j *job) error {
	var err error
	if j.agent != nil {
		err = j.agent.Close()
		j.agent = nil
	}
	if j.proc != nil {
		err = errors.Join(err, s.cfg.Launcher.Kill(ctx, j.proc))
		j.proc = nil
	}
	return err
}

func (s *Server) jobStateDir(id string) string {
	return filepath.Join(s.cfg.Dirs.State, "jobs", id)
}
