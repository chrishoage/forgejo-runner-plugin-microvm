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

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/agentclient"
	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/imagecache"
)

// agentPath is where Start writes the agent in the VM's root.
//
// SHORTCUT(#6): provisional pick, and the same path is what the VM child
// starts (internal/job/internal/vm's AgentPath); the protocol test fails if
// they part.
const agentPath = ".runner-microvm-agent"

// The job's container's fixed shape and its agent's vsock port.
//
// SHORTCUT(#8): fixed CPU and memory, the built-in profile's, with no
// profiles.
const (
	vmCPUs      = 2
	vmMemoryMiB = 1024
	// SHORTCUT(#6): provisional pick: the agent's vsock port.
	agentPort = 1024
	// The VM's name; GST-12's hostname rule is #6's.
	vmName = "job"
)

// The most of a console or VM log a failed Start's error carries.
const tailBytes = 300

// Start pulls the job's image, starts its job process, which boots the VM,
// waits until the agent answers Ping, and ends with StartComplete carrying
// the image's environment (PROTO-8). A failed Start kills the job process
// and waits until its group is reaped.
//
// SHORTCUT(#10): no queue or memory budget: every Start boots at once.
func (s *Server) Start(req *pluginv1alpha.StartRequest, stream pluginv1alpha.BackendPlugin_StartServer) error {
	j, err := s.lock(req.GetEnvironmentId())
	if err != nil {
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
	s.log.Info("started", "job", j.id, "image", j.image, "took", time.Since(begin).Round(time.Millisecond))
	return await(ctx, func() error {
		return stream.Send(&pluginv1alpha.StartOutput{Output: &pluginv1alpha.StartOutput_StartComplete{
			StartComplete: &pluginv1alpha.StartComplete{ImageEnv: env},
		}})
	})
}

// startError is the status a failed Start returns: the call's own context's
// code once it ended, Unavailable otherwise.
func startError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	return status.Error(codes.Unavailable, err.Error())
}

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
	root := filepath.Join(vmState, "root")
	img, err := s.cfg.Cache.Acquire(ctx, imagecache.Request{Job: j.id, VM: 0, Ref: j.image, Root: root})
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", j.image, err)
	}
	if err := writeAgent(root, s.cfg.Agent); err != nil {
		return nil, fmt.Errorf("write the agent: %w", err)
	}
	vm := &jobv1.VM{
		Name:        vmName,
		Cpus:        vmCPUs,
		MemoryMib:   vmMemoryMiB,
		VmLog:       filepath.Join(vmState, "vm.log"),
		Console:     filepath.Join(vmState, "console"),
		AgentSocket: filepath.Join(vmRuntime, "agent.sock"),
		AgentPort:   agentPort,
		RootDir:     root,
	}
	proc, err := s.cfg.Launcher.Launch(ctx, &jobv1.Spec{
		Job:       j.id,
		JobBinary: s.cfg.JobBinary,
		LibDir:    s.cfg.LibDir,
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
	events := j.proc.Events
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
				events = nil
				continue
			}
			if x := ev.GetVmExited(); x != nil {
				return fmt.Errorf("VM %s exited during boot with code %d%s", vm.GetName(), x.GetExitCode(),
					lastOutput(x.GetConsoleTail(), x.GetLogTail()))
			}
			if f := ev.GetFailed(); f != nil {
				return fmt.Errorf("job process: %s", f.GetReason())
			}
		case <-j.proc.Exited:
			return fmt.Errorf("the job process exited during boot%s", lastOutput(tail(vm.GetConsole()), tail(vm.GetVmLog())))
		}
	}
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

// writeAgent writes the agent into the VM's root. The root holds the
// image, which is untrusted, so the write goes through an os.Root: a symlink
// the image put at the agent's path can't redirect it.
func writeAgent(root string, agent []byte) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	if err := r.RemoveAll(agentPath); err != nil {
		return err
	}
	f, err := r.OpenFile(agentPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	_, err = f.Write(agent)
	return errors.Join(err, f.Close())
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
