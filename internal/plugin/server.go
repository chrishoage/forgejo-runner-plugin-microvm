// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/agentclient"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/config"
	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/joblaunch"
)

// Name is the plugin's name in Capabilities.
const Name = "runner-microvm"

// The job's paths and OS, as Create returns them (PROTO-7). They are fixed:
// nothing asks for them to change.
const (
	rootPath      = "/"
	actPath       = "/var/run/act"
	toolCachePath = "/opt/hostedtoolcache"
	tempPath      = "/tmp"
	platformOS    = "Linux"
)

// defaultPath is the plugin's default PATH, which Create returns as the
// runner's default_path_variable: the runner uses it for a job whose image
// sets no PATH. Start puts it in the boot config as the agent's default
// PATH, so the runner and the agent use one value.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// runnerArch returns the runner's name (RUNNER_ARCH) for goarch, the host's
// architecture, or an error naming it when libkrun doesn't support it. Images
// resolve for the host's platform, so the job's arch is the host's.
func runnerArch(goarch string) (string, error) {
	switch goarch {
	case "amd64":
		return "X64", nil
	case "arm64":
		return "ARM64", nil
	}
	return "", fmt.Errorf("runner-microvm runs only on amd64 and arm64, the architectures libkrun supports, not on %q", goarch)
}

// Config is what the server needs from startup.
type Config struct {
	Dirs Dirs
	// LibDir holds libkrun and libkrunfw; empty loads them by soname.
	LibDir string
	// JobBinary is the extracted job binary's path.
	JobBinary string
	// MkfsExt4 is the host's mkfs.ext4, which formats each scratch disk.
	MkfsExt4 string
	// Agent is the agent's path in <state>/bin/, which each VM's boot
	// root hard-links.
	Agent string
	// Profiles are the config file's profiles, which Create picks from.
	Profiles config.Profiles
	// Cache and Launcher are the plugin's two seams.
	Cache    ImageCache
	Launcher JobLauncher
	// ReadyTimeout is how long Start waits for the agent to answer Ping.
	ReadyTimeout time.Duration
	// LostAgentGrace is how long a call whose agent failed, and is gone,
	// waits for the job process to report the VM exited (agentError).
	LostAgentGrace time.Duration
	// BackstopDelay is how long after its environment timeout the plugin
	// removes a job the runner has not (PROTO-14).
	BackstopDelay time.Duration
	// Arch is the runner's name for the host's architecture (runnerArch),
	// which Create returns.
	Arch string
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
	// profiles are the profiles Create picks from; SetProfiles replaces
	// them.
	profiles config.Profiles
	// closed is set by Close: SetProfiles then does nothing.
	closed bool

	// backstops counts the backstop's removals in progress, which Close
	// waits for.
	backstops sync.WaitGroup

	// warnedIgnored is set by the first Create that carries a request field
	// the plugin ignores, which logs the one warning naming them.
	warnedIgnored atomic.Bool
}

// job is one job in the table, from Create to Remove.
type job struct {
	id    string
	image string
	// hostname is the job's container's name on the job network: the
	// runner's name for the job made DNS-safe (GST-12).
	hostname string
	// profileName names the profile the job's label picked, and profile is
	// the job's copy of it, taken at Create: a later change of the config
	// never changes it (CFG-7).
	profileName string
	profile     config.Profile
	// backstop removes the job at its environment timeout plus the
	// backstop delay, if the runner has not (PROTO-14); nil when the
	// runner sent no timeout. It is set before the job enters the table.
	backstop *time.Timer

	// ctx is cancelled when Remove starts, so a call still unwinding on the
	// job ends at once (PROTO-13).
	ctx    context.Context
	cancel context.CancelFunc

	// ready is set once Start has booted the job's container, before Start
	// releases mu. Exec, CopyIn and CopyOut read it before they wait for mu,
	// so a call made while Start is still in progress fails at once with
	// FailedPrecondition (PROTO-11) instead of waiting out the pull and the
	// boot, and running once they succeed.
	ready atomic.Bool

	// mu is held by every call on the job for its whole length, so Remove
	// waits until a cancelled call has unwound before it tears the job down.
	// A call that holds it releases it promptly once ctx is cancelled, also
	// while it is blocked on the runner's stream (await), so Remove never
	// waits long. A call waits for it only as long as its own context runs
	// (lockJob). Everything below is guarded by it.
	mu jobLock
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
	// gone is set when Start succeeds, and cancelled, with the
	// FailedPrecondition status every call on the job then fails with as
	// its cause, once the job's container's VM has died: the job process
	// reported it exited, or failed, or exited itself. watched closes once
	// the goroutine that watches for it has returned. They are set under
	// mu, and gone is cancelled without it.
	gone    context.Context
	watched chan struct{}
}

// NewServer returns a server with an empty job table. cfg.Dirs.Runtime must
// exist. Close releases it.
func NewServer(log *slog.Logger, cfg Config) (*Server, error) {
	if cfg.Arch == "" {
		return nil, errors.New("no arch for Create to return")
	}
	rt, err := openSockDir(cfg.Dirs.Runtime)
	if err != nil {
		return nil, err
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = agentclient.ReadyTimeout
	}
	if cfg.LostAgentGrace == 0 {
		cfg.LostAgentGrace = lostAgentGrace
	}
	if cfg.BackstopDelay == 0 {
		cfg.BackstopDelay = backstopDelay
	}
	return &Server{log: log, cfg: cfg, runtime: rt, jobs: map[string]*job{}, profiles: cfg.Profiles}, nil
}

// SetProfiles replaces the profiles later Creates pick from. A job already
// created keeps its own copy of its profile (CFG-7). ps must not change
// afterwards. Once s is closed it does nothing, and reports false: a reload
// still reading as the plugin stops finishes after Close.
func (s *Server) SetProfiles(ps config.Profiles) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.profiles = ps
	return true
}

// Close tears down every job still in the table, and releases the server.
// A second Close does nothing more.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	jobs := s.jobs
	s.jobs = map[string]*job{}
	s.closed = true
	s.mu.Unlock()
	var errs []error
	for _, j := range jobs {
		errs = append(errs, s.remove(ctx, j))
	}
	// A backstop that took its job out of the table before Close did is
	// still removing it.
	s.backstops.Wait()
	s.closeOnce.Do(func() { errs = append(errs, s.runtime.Close()) })
	return errors.Join(errs...)
}

// Capabilities names the plugin.
func (s *Server) Capabilities(context.Context, *pluginv1alpha.CapabilitiesRequest) (*pluginv1alpha.CapabilitiesResponse, error) {
	return &pluginv1alpha.CapabilitiesResponse{Name: Name}, nil
}

// Create picks the job's image and profile from the label argument and
// returns the job's environment id, paths and platform. It boots nothing
// (PROTO-3). The job keeps a copy of its profile for its whole life.
func (s *Server) Create(ctx context.Context, req *pluginv1alpha.CreateRequest) (*pluginv1alpha.CreateResponse, error) {
	s.warnIgnored(req)
	if len(req.GetServices()) > 0 {
		// The request is well formed; the plugin can't run services until
		// the protocol carries per-service VM options (PROTO-6).
		return nil, status.Error(codes.Unimplemented,
			"runner-microvm does not run services: the plugin protocol passes no options to configure a service's VM")
	}
	l, err := parseLabel(req.GetLabelArg())
	if err != nil {
		return nil, err
	}
	prof, err := s.lookupProfile(l.profile, req.GetLabelArg())
	if err != nil {
		return nil, err
	}
	image := req.GetImage()
	if image == "" {
		image = l.image
	}
	if image == "" {
		return nil, status.Error(codes.InvalidArgument, "no image: neither the label nor the job's container: names one")
	}
	// A missing or zero timeout sets no backstop.
	timeout := req.GetEnvironmentTimeout().AsDuration()
	// The backstop outlives Create, and its removal is never cancelled.
	j, err := s.add(context.WithoutCancel(ctx), image, dnsName(req.GetName()), l.profile, prof, timeout)
	if err != nil {
		return nil, err
	}
	s.log.Info("created", "job", j.id, "name", req.GetName(), "hostname", j.hostname, "image", image, "profile", j.profileName,
		"environment_timeout", timeout,
		"cpus", j.profile.CPUs, "memory_mib", j.profile.MemoryMiB, "scratch_gib", j.profile.ScratchGiB,
		"scratch_paths", j.profile.ScratchPaths, "force_pull", j.profile.ForcePull)
	return &pluginv1alpha.CreateResponse{
		EnvironmentId:       j.id,
		RootPath:            rootPath,
		ActPath:             actPath,
		ToolCachePath:       toolCachePath,
		TempPath:            tempPath,
		DefaultPathVariable: proto.String(defaultPath),
		Os:                  platformOS,
		Arch:                s.cfg.Arch,
	}, nil
}

// warnIgnored logs one warning, at the first Create that carries any of
// them, naming the request fields req carries that the plugin ignores:
// backend_options (the runner's per-label options, which the config file
// covers), cap_add and cap_drop (set only by forgejo-runner exec; steps run
// as root with every capability, in a VM of their own).
func (s *Server) warnIgnored(req *pluginv1alpha.CreateRequest) {
	var fields []string
	if len(req.GetBackendOptions()) > 0 {
		fields = append(fields, "backend_options")
	}
	if len(req.GetCapAdd()) > 0 {
		fields = append(fields, "cap_add")
	}
	if len(req.GetCapDrop()) > 0 {
		fields = append(fields, "cap_drop")
	}
	if len(fields) == 0 || !s.warnedIgnored.CompareAndSwap(false, true) {
		return
	}
	s.log.Warn("runner-microvm ignores these Create request fields; this warning is logged once",
		"name", req.GetName(), "fields", strings.Join(fields, ","))
}

// lookupProfile returns a copy of the profile name names, or fails with
// InvalidArgument naming arg, the label argument, when there is none
// (CFG-6).
func (s *Server) lookupProfile(name, arg string) (config.Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prof, ok := s.profiles.Lookup(name)
	if !ok {
		return config.Profile{}, labelError(arg, "unknown profile %q", name)
	}
	// The job's own copy shares nothing with the profiles a reload swaps.
	prof.ScratchPaths = slices.Clone(prof.ScratchPaths)
	return prof, nil
}

// add puts a new job in the table, under a fresh environment id, with its
// image, its hostname and its copy of the profile name names, and, for a
// timeout above 0, its backstop, which removes it with ctx.
func (s *Server) add(ctx context.Context, image, hostname, name string, prof config.Profile, timeout time.Duration) (*job, error) {
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
		jobCtx, cancel := context.WithCancel(context.Background())
		j := &job{id: id, image: image, hostname: hostname, profileName: name, profile: prof, ctx: jobCtx, cancel: cancel, mu: newJobLock()}
		if timeout > 0 {
			// It runs only once add has released s.mu.
			j.backstop = time.AfterFunc(saturatingAdd(timeout, s.cfg.BackstopDelay), func() { s.removeForgotten(ctx, j, timeout) })
		}
		s.jobs[id] = j
		return j, nil
	}
}

// saturatingAdd returns a+b, both at least 0, or the largest Duration where
// the sum overflows: a huge environment timeout must not wrap to a
// negative one, whose backstop fires at once.
func saturatingAdd(a, b time.Duration) time.Duration {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
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

// jobLock is a job's lock: a mutex whose wait a context can end, so a call
// queued behind another on the same job never outlives its own context.
type jobLock struct{ held chan struct{} }

func newJobLock() jobLock { return jobLock{held: make(chan struct{}, 1)} }

// Lock waits for l. Only Remove uses it, which cancels every call on the
// job first, so the wait is short.
func (l jobLock) Lock() { l.held <- struct{}{} }

// LockContext waits for l until ctx ends, and returns ctx's error then,
// without l.
func (l jobLock) LockContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case l.held <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l jobLock) Unlock() {
	select {
	case <-l.held:
	default:
		panic("unlock of an unlocked job lock")
	}
}

// lookup returns the job id names, or NotFound.
func (s *Server) lookup(id string) (*job, error) {
	s.mu.Lock()
	j, ok := s.jobs[id]
	s.mu.Unlock()
	if !ok {
		return nil, notFound(id)
	}
	return j, nil
}

// lockJob locks j, waiting at most as long as ctx, the call's context,
// runs: it fails with ctx's status then, and with NotFound once Remove has
// had j. The caller unlocks it.
func (s *Server) lockJob(ctx context.Context, j *job) error {
	if err := j.mu.LockContext(ctx); err != nil {
		return status.FromContextError(err).Err()
	}
	if j.removed {
		j.mu.Unlock()
		return notFound(j.id)
	}
	return nil
}

// callContext returns ctx, also cancelled when Remove starts on j, and,
// once j is started, when its VM dies, with the status it died with as the
// cause (callError).
func callContext(ctx context.Context, j *job) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	stopRemoved := context.AfterFunc(j.ctx, func() { cancel(context.Canceled) })
	stopGone := func() bool { return true }
	if gone := j.gone; gone != nil {
		stopGone = context.AfterFunc(gone, func() { cancel(context.Cause(gone)) })
	}
	return ctx, func() { stopRemoved(); stopGone(); cancel(context.Canceled) }
}

// callError is the status a call on j returns for err, its outcome, once
// ctx, its call context, has ended: Canceled when Remove ended it, the
// status the VM died with when its death did, else ctx's own code. While
// ctx runs, and for a call that succeeded, it is err.
func callError(ctx context.Context, j *job, err error) error {
	if err == nil || ctx.Err() == nil {
		return err
	}
	if j.ctx.Err() != nil {
		return status.FromContextError(context.Canceled).Err()
	}
	if cause := context.Cause(ctx); status.Code(cause) == codes.FailedPrecondition {
		return cause
	}
	return status.FromContextError(ctx.Err()).Err()
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

// backstopDelay is how long after a job's environment timeout the plugin
// removes it if the runner has not: long enough to leave the runner its own
// Remove first (PROTO-14).
const backstopDelay = 30 * time.Second

// removeForgotten is the backstop: it removes j with ctx, which the runner
// has not removed by its environment timeout plus the backstop delay,
// exactly as Remove does, and logs that the backstop fired (PROTO-14). A
// Remove after it gets NotFound, as for any unknown id.
func (s *Server) removeForgotten(ctx context.Context, j *job, timeout time.Duration) {
	s.mu.Lock()
	if s.jobs[j.id] != j {
		// Remove, or Close, has it.
		s.mu.Unlock()
		return
	}
	delete(s.jobs, j.id)
	s.backstops.Add(1)
	s.mu.Unlock()
	defer s.backstops.Done()
	s.log.Warn("the runner has not removed the job within its environment timeout: removing it",
		"job", j.id, "environment_timeout", timeout, "backstop_delay", s.cfg.BackstopDelay)
	_ = s.remove(ctx, j)
}

// remove tears down j, which is out of the table: it stops its backstop,
// ends the job process, which ends the VM, and deletes the job's files.
func (s *Server) remove(ctx context.Context, j *job) error {
	if j.backstop != nil {
		j.backstop.Stop()
	}
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
// waits until its process group is reaped and its watcher has returned.
// j.mu is held.
func (s *Server) stop(ctx context.Context, j *job) error {
	var err error
	if j.agent != nil {
		err = j.agent.Close()
		j.agent = nil
	}
	if j.proc != nil {
		killErr := s.cfg.Launcher.Kill(ctx, j.proc)
		j.proc = nil
		if killErr == nil && j.watched != nil {
			// The job process is gone, so its events end, and the
			// watcher with them.
			<-j.watched
			j.watched = nil
		}
		err = errors.Join(err, killErr)
	}
	return err
}

func (s *Server) jobStateDir(id string) string {
	return filepath.Join(s.cfg.Dirs.State, "jobs", id)
}
