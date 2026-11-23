// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"google.golang.org/protobuf/proto"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/agentclient"
	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/imagecache"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/joblaunch"
)

// The boot root's files, by their names in it, and where the agent mounts
// the layer share.
//
// SHORTCUT(#6): provisional picks. The agent's path is the one the VM child
// starts (internal/job/internal/vm's AgentPath), the boot config's name the
// one the agent reads (internal/agent's BootConfigPath), and the layer
// directory is under the agent's staging directory; the protocol test fails
// if they part.
const (
	agentPath      = ".runner-microvm-agent"
	bootConfigPath = ".runner-microvm-boot"
	guestLayerDir  = "/dev/.runner-microvm/layers"
	// The scratch disk is the VM's first block device.
	scratchDevice = "/dev/vda"
)

// SHORTCUT(#6): provisional pick: the job's container's agent's vsock
// port.
const agentPort = 1024

// The most of a console or VM log a failed Start's error carries.
const tailBytes = 300

// Start pulls the job's image, starts its job process, which boots the VM,
// waits until the agent answers Ping, and ends with StartComplete carrying
// the image's environment (PROTO-8). A failed Start kills the job process
// and waits until its group is reaped.
//
// SHORTCUT(#10): no queue or memory budget: every Start boots at once.
func (s *Server) Start(req *pluginv1alpha.StartRequest, stream pluginv1alpha.BackendPlugin_StartServer) error {
	j, err := s.lookup(req.GetEnvironmentId())
	if err != nil {
		return err
	}
	if err := s.lockJob(stream.Context(), j); err != nil {
		return err
	}
	defer j.mu.Unlock()
	// A failed Start left the VM's files half made; the runner removes
	// the job, it never starts it again.
	if j.startErr != nil {
		return status.Errorf(codes.FailedPrecondition, "environment %s failed to start earlier, and must be removed: %v", j.id, j.startErr)
	}
	if j.started {
		return status.Errorf(codes.FailedPrecondition, "environment %s is already started", j.id)
	}
	j.started = true
	ctx, cancel := callContext(stream.Context(), j)
	defer cancel()
	begin := time.Now()
	env, err := s.boot(ctx, j)
	if err != nil {
		// The job process and its VM are gone before Start returns.
		err = errors.Join(err, s.stop(context.WithoutCancel(ctx), j))
		j.startErr = err
		s.log.Error("start failed", "job", j.id, "err", err)
		return startError(ctx, err)
	}
	j.ready.Store(true)
	s.log.Info("started", "job", j.id, "image", j.image, "profile", j.profileName, "took", time.Since(begin).Round(time.Millisecond))
	gone, markGone := context.WithCancelCause(context.Background())
	j.gone, j.watched = gone, make(chan struct{})
	go s.watch(j, j.proc, markGone, j.watched)
	return await(ctx, func() error {
		return stream.Send(&pluginv1alpha.StartOutput{Output: &pluginv1alpha.StartOutput_StartComplete{
			StartComplete: &pluginv1alpha.StartComplete{ImageEnv: env},
		}})
	})
}

// startError is the status a failed Start returns: the call's own context's
// code once it ended, FailedPrecondition when the job network failed to
// start, Unavailable otherwise.
func startError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	if errors.As(err, new(*networkError)) {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return status.Error(codes.Unavailable, err.Error())
}

// networkFailed is how a job process's Failed reason begins when its job
// network failed to start.
const networkFailed = "network: "

// networkError is a job network that failed to start.
type networkError struct{ reason string }

func (e *networkError) Error() string { return "job process: " + e.reason }

// boot does Start's work on j, which is locked, and returns the image's
// environment. On error, j.proc and j.agent may be set: the caller stops
// them.
func (s *Server) boot(ctx context.Context, j *job) (map[string]string, error) {
	vmState := filepath.Join(s.jobStateDir(j.id), "0")
	vmRuntime := filepath.Join(s.cfg.Dirs.Runtime, j.id, "0")
	for _, d := range []string{vmState, vmRuntime} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	addr, mac, err := vmAddress(0)
	if err != nil {
		return nil, err
	}
	img, err := s.cfg.Cache.Acquire(ctx, imagecache.Request{Job: j.id, VM: 0, Ref: j.image, ForcePull: j.profile.ForcePull})
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", j.image, err)
	}
	boot := &agentv1.BootConfig{
		LayerDir:      guestLayerDir,
		Layers:        img.Layers,
		ScratchDevice: scratchDevice,
		ScratchPaths:  j.profile.ScratchPaths,
		Network:       bootNetwork(addr),
		Hostname:      j.hostname,
		Image:         &agentv1.Image{Env: img.Env},
		Hosts:         []*agentv1.Host{{Name: j.hostname, Address: addr.String()}},
	}
	root := filepath.Join(vmState, "root")
	if err := makeBootRoot(root, s.cfg.Agent, boot); err != nil {
		return nil, fmt.Errorf("boot root %s: %w", root, err)
	}
	vm := &jobv1.VM{
		Name:             j.hostname,
		Cpus:             j.profile.CPUs,
		MemoryMib:        j.profile.MemoryMiB,
		ScratchDisk:      filepath.Join(vmState, "scratch.ext4"),
		ScratchSizeBytes: j.profile.ScratchBytes(),
		VmLog:            filepath.Join(vmState, "vm.log"),
		Console:          filepath.Join(vmState, "console"),
		AgentSocket:      filepath.Join(vmRuntime, "agent.sock"),
		AgentPort:        agentPort,
		RootDir:          root,
		LayerDir:         img.LinkDir,
		GuestAddress:     addr.String(),
		GuestMac:         mac.String(),
		Boot:             boot,
	}
	proc, err := s.cfg.Launcher.Launch(ctx, &jobv1.Spec{
		Job:       j.id,
		JobBinary: s.cfg.JobBinary,
		LibDir:    s.cfg.LibDir,
		MkfsExt4:  s.cfg.MkfsExt4,
		Network:   jobNetwork(),
		Vms:       []*jobv1.VM{vm},
	}, filepath.Join(s.jobStateDir(j.id), "job.log"))
	if err != nil {
		return nil, fmt.Errorf("job process: %w", err)
	}
	j.proc = proc
	// The agent socket's path can outgrow connect's 107 bytes, so it is
	// dialled through the runtime directory.
	agent, err := agentclient.New(s.runtime.path(j.id + "/0/agent.sock"))
	if err != nil {
		return nil, err
	}
	j.agent = agent
	if err := s.waitReady(ctx, j, vm); err != nil {
		return nil, err
	}
	return envMap(img.Env), nil
}

// waitReady waits until j's agent answers Ping, failing as soon as the VM
// exits or the job process fails, or after the ready timeout (VM-8).
func (s *Server) waitReady(ctx context.Context, j *job, vm *jobv1.VM) error {
	readyCtx, cancel := context.WithTimeout(ctx, s.cfg.ReadyTimeout)
	defer cancel()
	ready := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ready <- j.agent.WaitReady(readyCtx)
	}()
	// The goroutine always ends, once readyCtx does at the latest.
	defer func() { cancel(); <-done }()
	// Once the job process has exited, its events still to come say why:
	// they end when the launcher reaches the end of its stdout, which no
	// process is left to hold open, so they are read to their end before
	// the exit is reported.
	events, exited := j.proc.Events, j.proc.Exited
	exitedErr := func() error {
		return fmt.Errorf("the job process exited during boot%s", lastOutput(tail(vm.GetConsole()), tail(vm.GetVmLog())))
	}
	for {
		select {
		case err := <-ready:
			if err == nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("VM %s: the agent did not answer within %v%s", vm.GetName(), s.cfg.ReadyTimeout,
				lastOutput(tail(vm.GetConsole()), tail(vm.GetVmLog())))
		case ev, ok := <-events:
			if !ok {
				if exited == nil {
					return exitedErr()
				}
				events = nil
				continue
			}
			if x := ev.GetVmExited(); x != nil {
				return fmt.Errorf("VM %s exited during boot with code %d%s", vm.GetName(), x.GetExitCode(),
					lastOutput(x.GetConsoleTail(), x.GetLogTail()))
			}
			if f := ev.GetFailed(); f != nil {
				if strings.HasPrefix(f.GetReason(), networkFailed) {
					return &networkError{reason: f.GetReason()}
				}
				return fmt.Errorf("job process: %s", f.GetReason())
			}
		case <-exited:
			if events == nil {
				return exitedErr()
			}
			exited = nil
		}
	}
}

// lostAgentGrace is how long a call whose agent failed, and is gone, waits
// for the job process to report its VM exited. A failing agent call is how
// the plugin first sees a VM die, moments before the job process has reaped the
// VM child and says so; the agent is the VM's workload, so a VM whose agent
// is gone exits.
const lostAgentGrace = 10 * time.Second

// watch reads the events of p, the job process of j once its Start
// succeeded, and marks j gone with markGone at the first that ends its job's
// container's VM: that VM exited, or the job process failed, or the job
// process exited with neither said. It returns, closing watched, once p has
// exited and its events have ended.
func (s *Server) watch(j *job, p *joblaunch.Process, markGone context.CancelCauseFunc, watched chan<- struct{}) {
	defer close(watched)
	gone := func(err error) {
		if j.gone.Err() != nil {
			return
		}
		markGone(err)
		// Remove kills the job process; only a VM that died on its own
		// is news.
		if j.ctx.Err() == nil {
			s.log.Warn("the job's VM died; its calls fail until the runner removes it", "job", j.id, "err", err)
		}
	}
	events, exited := p.Events, p.Exited
	for events != nil || exited != nil {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if x := ev.GetVmExited(); x != nil && x.GetVm() == 0 {
				gone(vmExitedError(j.hostname, x))
			}
			if f := ev.GetFailed(); f != nil {
				gone(status.Errorf(codes.FailedPrecondition, "VM %s: its job process failed: %s", j.hostname, f.GetReason()))
			}
		case <-exited:
			exited = nil
		}
	}
	// Events end only once the job process is gone.
	gone(status.Errorf(codes.FailedPrecondition, "VM %s: its job process exited", j.hostname))
}

// vmExitedError is the status of every call on a job whose VM, named name,
// exited after Start: FailedPrecondition, naming the VM, its exit code and
// its console tail. It is not Unavailable, which the runner reads as the
// plugin being gone.
func vmExitedError(name string, x *jobv1.VMExited) error {
	msg := fmt.Sprintf("VM %s exited (%d)", name, x.GetExitCode())
	if c := strings.TrimSpace(x.GetConsoleTail()); c != "" {
		msg += ": " + c
	}
	return status.Error(codes.FailedPrecondition, msg)
}

// lastOutput formats a VM's last console and log output for an error.
func lastOutput(console, log string) string {
	var b strings.Builder
	if c := strings.TrimSpace(console); c != "" {
		fmt.Fprintf(&b, "; console: %s", c)
	}
	if l := strings.TrimSpace(log); l != "" {
		fmt.Fprintf(&b, "; VM log: %s", l)
	}
	return b.String()
}

// tail returns the end of the file at path, at most tailBytes; a file that
// can't be read has an empty tail.
func tail(path string) string {
	f, err := os.Open(path) //nolint:gosec // G304: a log file of the plugin's own
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err == nil && fi.Size() > tailBytes {
		if _, err := f.Seek(-tailBytes, io.SeekEnd); err != nil {
			return ""
		}
	}
	b, _ := io.ReadAll(io.LimitReader(f, tailBytes))
	return string(b)
}

// envMap turns an image's KEY=VALUE environment into StartComplete's map.
func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// makeBootRoot makes the VM's boot root at root: empty proc, sys and dev
// mount points, so images without them boot (GST-2), a hard link to the
// agent, not a copy, and the agent's boot config as a file. libkrun shares it
// read-only as the VM's root, and it is the bottom layer of the guest's root,
// so the agent and its config stay visible in the job's root.
func makeBootRoot(root, agent string, boot *agentv1.BootConfig) error {
	b, err := proto.Marshal(boot)
	if err != nil {
		return err
	}
	if err := os.Mkdir(root, 0o755); err != nil { //nolint:gosec // G301: the guest's /, which every guest user must traverse
		return err
	}
	for _, d := range []string{"proc", "sys", "dev"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil { //nolint:gosec // G301: guest mount points
			return err
		}
	}
	if err := os.Link(agent, filepath.Join(root, agentPath)); err != nil {
		return err
	}
	// The boot config holds the image's environment, which may carry
	// values.
	return os.WriteFile(filepath.Join(root, bootConfigPath), b, 0o600)
}

// removeTree deletes dir and everything in it, also directories the image or
// the guest made read-only. A missing dir is not an error.
func removeTree(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	// Give every directory back its owner's write bit, then try again.
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o700) //nolint:gosec // G302: owner-only, only to delete it
		}
		return nil
	})
	return os.RemoveAll(dir)
}
