// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"google.golang.org/protobuf/proto"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/admission"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/agentclient"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/config"
	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	jobv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/job/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/imagecache"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/joblaunch"
)

// The boot root's directory of the agent's own files, and the files in it,
// by their paths in the boot root: one hidden directory keeps the job's root
// tidy, and names both after the program. The agent's path is the one the
// VM child starts (internal/job/internal/vm's AgentPath), and the boot
// config's the one the agent reads (internal/agent's BootConfigPath, which
// the tests check this against).
const (
	agentDir       = ".runner-microvm"
	agentPath      = agentDir + "/agent"
	bootConfigPath = agentDir + "/boot-config.binpb"
)

// The layer share: the virtiofs tag the VM's layer link directory is shared
// under, and where the agent mounts it. Only the plugin sets them: the job
// spec carries the tag for libkrun, and the boot config both for the agent,
// so the share and the mount can't name two tags. The mount point is under
// the agent's staging directory: the devtmpfs is the one writable place
// before the job's root exists.
const (
	layerTag      = "layers"
	guestLayerDir = "/dev/.runner-microvm/layers"
)

// The scratch disk is the VM's first block device.
const scratchDevice = "/dev/vda"

// mountTag is the virtiofs tag of the mount at index i of a profile's
// mounts. It is never derived from the mount's path: libkrun copies a tag
// into a 36-byte buffer with no length check, and panics when the VM starts
// on a longer one.
func mountTag(i int) string { return "mount" + strconv.Itoa(i) }

// diskBlockID is the block id of the disk at index i of a profile's disks,
// which the guest sees as the disk's virtio-blk serial: up to 20 bytes, so
// never derived from the disk's path.
func diskBlockID(i int) string { return "disk" + strconv.Itoa(i) }

// disksAttr formats a profile's disks for a log line, as source:target:ro
// (or rw), comma-separated.
func disksAttr(ds []config.Disk) string {
	ms := make([]config.Mount, len(ds))
	for i, d := range ds {
		ms[i] = config.Mount(d)
	}
	return mountsAttr(ms)
}

// mountsAttr formats a profile's mounts for a log line, as
// source:target:ro (or rw), comma-separated.
func mountsAttr(ms []config.Mount) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		mode := "ro"
		if m.ReadWrite {
			mode = "rw"
		}
		parts[i] = m.Source + ":" + m.Target + ":" + mode
	}
	return strings.Join(parts, ",")
}

// guestDirs are the directories steps expect, Create's act path, tool cache
// and temp directory (PROTO-7), with the mode the agent gives each one the
// image lacks: any user a step runs as must be able to write them, and the
// temp directory keeps the sticky bit.
var guestDirs = []struct {
	path string
	mode uint32
}{
	{actPath, 0o777},
	{toolCachePath, 0o777},
	{tempPath, 0o1777},
}

// agentPort is the vsock port every VM's agent listens on. Only the plugin
// sets it: the job spec carries it for libkrun's port mapping, and the boot
// config for the agent, which reads it before it listens.
const agentPort = 1024

// How much of a VM's console and of its VM log an error carries: the last
// tailLines lines of the last tailBytes of each. 20 lines hold a kernel
// panic's message and the lines before it, and the job log shows several
// lines fine (PROTO-10, VM-8).
const (
	tailLines = 20
	tailBytes = 4 << 10
)

// Start pulls the job's image, waits for the job's admission to its disks
// and memory, starts its job process, which boots the VM, waits until the
// agent answers Ping, and ends with StartComplete carrying the image's
// environment (PROTO-8). Before that it sends a progress line for the image,
// each wait if there is one, the VM and the total (PROTO-9). A failed Start
// kills the job process, waits until its group is reaped, and then releases
// the job's admission, its disks and memory together; when the kill fails,
// the job keeps its admission until its Remove reaps the group.
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
	p := &progress{ctx: ctx, stream: stream, begin: time.Now(), log: s.log.With("env", j.id)}
	img, err := s.boot(ctx, j, p)
	if err == nil {
		err = s.complete(ctx, j, p, envMap(img.Env))
	}
	if err != nil {
		// The job process and its VM are gone before Start returns.
		reaped, stopErr := s.stop(context.WithoutCancel(ctx), j)
		err = errors.Join(err, stopErr)
		if reaped {
			// The next job need not wait for this one's Remove. A
			// kill that failed is tried again at Remove, which then
			// releases.
			s.admission.Release(j.id)
		}
		j.startErr = err
		s.log.Error("start failed", "env", j.id, "err", err)
		return startError(ctx, stream.Context(), err)
	}
	j.ready.Store(true)
	s.log.Info("started", "env", j.id, "took", time.Since(p.begin).Round(time.Millisecond), "image", j.image, "digest", img.Digest,
		"layers", len(img.Layers), "layers_pulled", img.Pulled, "profile", j.profileName)
	return nil
}

// complete ends a Start whose VM is ready: it starts watching j's job
// process, then sends the last progress line and StartComplete with env.
// Its error, once ctx ended before StartComplete was sent, fails Start like
// any other: the runner never takes the job as started. Once StartComplete's
// send has returned without error, Start has succeeded, whatever ctx does
// meanwhile (awaitDelivery).
func (s *Server) complete(ctx context.Context, j *job, p *progress, env map[string]string) error {
	gone, markGone := context.WithCancelCause(context.Background())
	j.gone, j.watched = gone, make(chan struct{})
	go s.watch(j, j.proc, markGone, j.watched)
	p.line("started in %s", seconds(time.Since(p.begin)))
	err := ctx.Err()
	if !p.stalled {
		err = awaitDelivery(ctx, p.stream.Context(), func() error {
			return p.stream.Send(&pluginv1alpha.StartOutput{Output: &pluginv1alpha.StartOutput_StartComplete{
				StartComplete: &pluginv1alpha.StartComplete{ImageEnv: env},
			}})
		})
	}
	// A stalled progress line's Send may still run, and a stream takes one
	// Send at a time, so Start ended with its context. Either way the
	// failed Start kills the job process: marking the job gone first keeps
	// the watcher from reporting that as the VM dying.
	if err != nil {
		markGone(err)
	}
	return err
}

// awaitDelivery is await for StartComplete's send, on a stream whose own
// context is streamCtx, whose success must win over ctx ending at the same
// moment: the runner cancels Start as soon as it reads StartComplete, so
// once the send returned without error, Start has succeeded. Once ctx has
// ended, it sends nothing and returns ctx's status. When ctx ends during the
// send and streamCtx has ended too, the runner ended the stream: gRPC ends a
// blocked send as soon as its stream's context ends, so the send's own
// result is waited for. When only ctx ended, Remove's or the VM's death,
// the runner may never read again: a send that has returned still wins,
// and one still blocked fails with ctx's status, as await's does.
func awaitDelivery(ctx, streamCtx context.Context, send func() error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	done := make(chan error, 1)
	go func() { done <- send() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	if streamCtx.Err() != nil {
		return <-done
	}
	select {
	case err := <-done:
		return err
	default:
		return status.FromContextError(ctx.Err()).Err()
	}
}

// startError is the status a failed Start returns: the call's own context's
// code once it ended, whether the runner or Remove ended it; Unavailable when
// the drain ended its wait for admission; Internal for the plugin's own bug;
// else FailedPrecondition. The image, the VM's boot,
// its agent, its scratch disk or a disk is the job's or the operator's to
// fix, and never Unavailable, which the runner reads as the plugin being
// gone. ctx derives from streamCtx, the stream's own context, and Go ends a
// parent's Done before it cancels the contexts derived from it, so a send
// the stream's end cut off can fail before ctx reports its end: streamCtx's
// end then gives the code.
func startError(ctx, streamCtx context.Context, err error) error {
	for _, c := range []context.Context{ctx, streamCtx} {
		if c.Err() != nil {
			return status.FromContextError(c.Err()).Err()
		}
	}
	if errors.Is(err, errShuttingDown) {
		return errShuttingDown
	}
	if errors.As(err, new(*bugError)) {
		return status.Error(codes.Internal, err.Error())
	}
	return status.Error(codes.FailedPrecondition, err.Error())
}

// progress sends Start's progress lines to the runner, on stdout, which
// shows them in the job's setup log (PROTO-9): each line is `[runner-microvm
// <seconds since Start>s] <text>`, the prefix telling them apart from the
// runner's own lines. A line that can't be sent doesn't fail Start: only
// StartComplete must arrive.
type progress struct {
	ctx    context.Context
	stream pluginv1alpha.BackendPlugin_StartServer
	begin  time.Time
	log    *slog.Logger
	// stalled is set once a line's Send outlived ctx: it may still run,
	// so nothing more is sent on the stream.
	stalled bool
}

// line sends one progress line of format and args.
func (p *progress) line(format string, args ...any) { p.lineUntil(p.ctx, format, args...) }

// lineUntil sends a line as line does, but gives up on it as soon as ctx,
// derived from p.ctx, ends.
func (p *progress) lineUntil(ctx context.Context, format string, args ...any) {
	if p.stalled {
		return
	}
	text := fmt.Sprintf("[runner-microvm %s] %s\n", seconds(time.Since(p.begin)), fmt.Sprintf(format, args...))
	err := await(ctx, func() error {
		return p.stream.Send(&pluginv1alpha.StartOutput{Output: &pluginv1alpha.StartOutput_Data{Data: &pluginv1alpha.DataChunk{
			Stream: pluginv1alpha.DataChunk_STDOUT, Data: []byte(text),
		}}})
	})
	if err != nil {
		p.stalled = ctx.Err() != nil
		p.log.Warn("a progress line was not sent", "err", err)
	}
}

// seconds formats d as seconds to two decimals, as progress lines show
// times.
func seconds(d time.Duration) string { return fmt.Sprintf("%.2fs", d.Seconds()) }

// bugError is a failure only a bug in the plugin can cause.
type bugError struct{ err error }

func (e *bugError) Error() string { return e.err.Error() }

func (e *bugError) Unwrap() error { return e.err }

// boot does Start's work on j, which is locked, sending its progress to p,
// and returns the job's container's image. On error, j.proc and j.agent may
// be set: the caller stops them.
func (s *Server) boot(ctx context.Context, j *job, p *progress) (imagecache.Image, error) {
	vmState := filepath.Join(s.jobStateDir(j.id), "0")
	vmRuntime := filepath.Join(s.cfg.Dirs.Runtime, j.id, "0")
	for _, d := range []string{vmState, vmRuntime} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return imagecache.Image{}, err
		}
	}
	addr, mac, err := vmAddress(0)
	if err != nil {
		return imagecache.Image{}, &bugError{err}
	}
	pulling := time.Now()
	img, err := s.cfg.Cache.Acquire(ctx, imagecache.Request{Job: j.id, VM: 0, Ref: j.image, ForcePull: j.profile.ForcePull})
	if err != nil {
		return imagecache.Image{}, fmt.Errorf("image %s: %w", j.image, err)
	}
	p.line("image %s ready (%s, %d layers) in %s", j.image, img.Digest, len(img.Layers), seconds(time.Since(pulling)))
	if err := s.admit(ctx, j, p); err != nil {
		return imagecache.Image{}, err
	}
	boot := &agentv1.BootConfig{
		LayerDir:      guestLayerDir,
		LayerTag:      layerTag,
		Layers:        img.Layers,
		ScratchDevice: scratchDevice,
		ScratchPaths:  j.profile.ScratchPaths,
		Network:       bootNetwork(addr),
		Hostname:      j.hostname,
		Image:         &agentv1.Image{Env: img.Env, User: img.User, WorkingDir: img.WorkingDir, DefaultPath: defaultPath},
		Hosts:         []*agentv1.Host{{Name: j.hostname, Address: addr.String()}},
		VsockPort:     agentPort,
		LogLevel:      s.cfg.LogLevel,
	}
	for _, d := range guestDirs {
		boot.GuestDirs = append(boot.GuestDirs, &agentv1.GuestDir{Path: d.path, Mode: d.mode})
	}
	// Each mount is a share of its own, under a tag only its position
	// names; the job process checks its source again before the VM starts
	// (STO-1, STO-2, STO-5).
	var mounts []*jobv1.Mount
	for i, m := range j.profile.Mounts {
		mounts = append(mounts, &jobv1.Mount{Source: m.Source, Tag: mountTag(i), ReadWrite: m.ReadWrite})
		boot.Mounts = append(boot.Mounts, &agentv1.Mount{Tag: mountTag(i), Target: m.Target, ReadWrite: m.ReadWrite})
	}
	// Each disk is attached under a block id only its position names, which
	// the agent finds it by; the job process opens its source before the
	// VM starts (STO-1, STO-4).
	var disks []*jobv1.Disk
	for i, d := range j.profile.Disks {
		disks = append(disks, &jobv1.Disk{Source: d.Source, BlockId: diskBlockID(i), ReadWrite: d.ReadWrite})
		boot.Disks = append(boot.Disks, &agentv1.Disk{BlockId: diskBlockID(i), Target: d.Target, ReadWrite: d.ReadWrite})
	}
	root := filepath.Join(vmState, "root")
	if err := makeBootRoot(root, s.cfg.Agent, boot); err != nil {
		return imagecache.Image{}, fmt.Errorf("boot root %s: %w", root, err)
	}
	vm := &jobv1.VM{
		Name:             j.hostname,
		Cpus:             j.profile.CPUs,
		MemoryMib:        j.profile.MemoryMiB,
		ScratchDisk:      filepath.Join(vmState, "scratch.ext4"),
		ScratchSizeBytes: j.profile.ScratchBytes(),
		VmLog:            filepath.Join(vmState, vmLogName),
		Console:          filepath.Join(vmState, consoleName),
		AgentSocket:      filepath.Join(vmRuntime, "agent.sock"),
		AgentPort:        agentPort,
		RootDir:          root,
		LayerDir:         img.LinkDir,
		LayerTag:         layerTag,
		Mounts:           mounts,
		Disks:            disks,
		GuestAddress:     addr.String(),
		GuestMac:         mac.String(),
		Boot:             boot,
	}
	booting := time.Now()
	proc, err := s.cfg.Launcher.Launch(ctx, &jobv1.Spec{
		Job:             j.id,
		JobBinary:       s.cfg.JobBinary,
		LibDir:          s.cfg.LibDir,
		LibkrunLogLevel: s.cfg.LibkrunLogLevel,
		LogLevel:        s.cfg.LogLevel,
		MkfsExt4:        s.cfg.MkfsExt4,
		Network:         jobNetwork(),
		Vms:             []*jobv1.VM{vm},
	}, filepath.Join(s.jobStateDir(j.id), jobLogName))
	if err != nil {
		return imagecache.Image{}, fmt.Errorf("job process: %w", err)
	}
	j.proc = proc
	// The agent socket's path can outgrow connect's 107 bytes, so it is
	// dialled through the runtime directory.
	agent, err := agentclient.New(s.runtime.path(j.id + "/0/agent.sock"))
	if err != nil {
		return imagecache.Image{}, &bugError{err}
	}
	j.agent = agent
	if err := s.waitReady(ctx, j, vm); err != nil {
		return imagecache.Image{}, err
	}
	p.line("VM %s (%d CPUs, %d MiB) ready in %s", vm.GetName(), vm.GetCpus(), vm.GetMemoryMib(), seconds(time.Since(booting)))
	return img, nil
}

// admit admits j, which is locked, to its disks and then the memory budget,
// once its image is ready and before its job process starts, waiting in
// arrival order while a disk isn't free for it or it doesn't fit (QUE-2,
// QUE-3). Each disk is keyed by its source's device and inode, read here,
// so two paths to one file share its queue; a source missing then fails
// Start naming it, and one whose stat hangs ends with ctx (statUntil). Each wait sends a progress line and logs a line when it
// starts and when it ends; a job admitted at once does neither (QUE-8). A
// wait ends early only with ctx, or with the drain, which fails it with
// errShuttingDown (QUE-6), even while its waiting line's send blocks: that
// send gives up when the wait ends.
func (s *Server) admit(ctx context.Context, j *job, p *progress) error {
	disks, err := admissionDisks(ctx, s.statUntil, j.profile.Disks)
	if err != nil {
		return err
	}
	need := jobMemoryMiB(j.profile)
	waitCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if s.drainCtx.Err() != nil {
		// AfterFunc would end the wait only once it had begun: a Start
		// during the drain must never begin one.
		cancel(context.Cause(s.drainCtx)) //nolint:contextcheck // the drain only cancels the wait's own context, derived from ctx
	}
	stop := context.AfterFunc(s.drainCtx, func() { cancel(context.Cause(s.drainCtx)) }) //nolint:contextcheck // the drain only cancels the wait's own context, derived from ctx
	defer stop()
	return s.admission.Admit(waitCtx, j.id, disks, need, admission.Observer{
		Waiting: func(w admission.Wait) {
			if d := w.Disk; d != nil {
				// A disk is waited for only while some job holds it: a
				// line's head that nobody holds up is admitted.
				s.log.Info("waiting for disk", "env", j.id, "disk", d.Path, "mode", diskMode(d.ReadWrite),
					"holders", strings.Join(w.Holders, ","), "ahead", w.Ahead)
				p.lineUntil(waitCtx, "waiting for disk %s (held by %s; %d ahead)", d.Path, strings.Join(w.Holders, ", "), w.Ahead)
				return
			}
			s.log.Info("waiting for memory", "env", j.id, "need_mib", w.NeedMiB, "free_mib", w.FreeMiB, "cap_mib", w.CapMiB, "ahead", w.Ahead)
			p.lineUntil(waitCtx, "waiting for memory: needs %d MiB, %d of %d MiB free; %d ahead", w.NeedMiB, w.FreeMiB, w.CapMiB, w.Ahead)
		},
		Waited: func(w admission.Wait, took time.Duration, err error) {
			took = took.Round(time.Millisecond)
			if d := w.Disk; d != nil {
				if err != nil {
					s.log.Info("stopped waiting for disk", "env", j.id, "disk", d.Path, "mode", diskMode(d.ReadWrite), "took", took, "err", err)
					return
				}
				s.log.Info("got disk", "env", j.id, "disk", d.Path, "mode", diskMode(d.ReadWrite), "took", took)
				p.line("got disk %s after %s", d.Path, seconds(took))
				return
			}
			if err != nil {
				s.log.Info("stopped waiting for memory", "env", j.id, "need_mib", w.NeedMiB, "took", took, "err", err)
				return
			}
			s.log.Info("got memory", "env", j.id, "need_mib", w.NeedMiB, "took", took)
			p.line("got memory after %s", seconds(took))
		},
	})
}

// admissionDisks returns ds as admission takes them: each keyed by its
// source's device and inode, read with stat, which follows symlinks as the
// job process's open does, so the key is the file the disk lock will be on.
// A source that is missing, or can't be read, is an error naming it, as the
// job process's own check is (STO-5). Each stat ends early with ctx.
func admissionDisks(ctx context.Context, stat func(context.Context, string) (fs.FileInfo, error), ds []config.Disk) ([]admission.Disk, error) {
	out := make([]admission.Disk, 0, len(ds))
	for _, d := range ds {
		fi, err := stat(ctx, d.Source)
		var pe *fs.PathError
		switch {
		case ctx.Err() != nil:
			return nil, context.Cause(ctx)
		case errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("disk %s: does not exist", d.Source)
		case errors.As(err, &pe):
			return nil, fmt.Errorf("disk %s: %w", d.Source, pe.Err)
		case err != nil:
			return nil, fmt.Errorf("disk %s: %w", d.Source, err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, &bugError{fmt.Errorf("disk %s: stat returned no device and inode", d.Source)}
		}
		out = append(out, admission.Disk{
			Key:  admission.DiskKey{Dev: st.Dev, Ino: st.Ino},
			Path: d.Source, ReadWrite: d.ReadWrite,
		})
	}
	return out, nil
}

// statUntil is statDisk of path, given up on once ctx ends: a source on a
// hung mount (a dead NFS or FUSE server) blocks stat with no way to cancel
// it, and Start holds its job's lock meanwhile, which Remove, and so the
// drain, waits for. The stat runs on in its own goroutine, holding nothing
// but path, until the kernel returns, and each stuck one holds an OS
// thread, so a path has at most one stat in flight: a Start that finds an
// earlier one of path still running fails at once naming it, and path
// leaves statting when that stat returns.
func (s *Server) statUntil(ctx context.Context, path string) (fs.FileInfo, error) {
	s.statMu.Lock()
	if _, ok := s.statting[path]; ok {
		s.statMu.Unlock()
		return nil, errors.New("an earlier stat of it is still stuck, as on a hung mount")
	}
	s.statting[path] = struct{}{}
	s.statMu.Unlock()
	type result struct {
		fi  fs.FileInfo
		err error
	}
	done := make(chan result, 1)
	go func() {
		fi, err := s.statDisk(path)
		s.statMu.Lock()
		delete(s.statting, path)
		s.statMu.Unlock()
		done <- result{fi, err}
	}()
	select {
	case r := <-done:
		return r.fi, r.err
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// disksStatting is how many disk sources have a stat in flight.
func (s *Server) disksStatting() int {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	return len(s.statting)
}

// diskMode is a disk's mode as log lines show it: ro or rw.
func diskMode(readWrite bool) string {
	if readWrite {
		return "rw"
	}
	return "ro"
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
		return fmt.Errorf("VM %s: its job process exited during boot%s", vm.GetName(), vmOutput(tail(vm.GetVmLog()), tail(vm.GetConsole())))
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
				vmOutput(tail(vm.GetVmLog()), tail(vm.GetConsole())))
		case ev, ok := <-events:
			if !ok {
				if exited == nil {
					return exitedErr()
				}
				events = nil
				continue
			}
			if x := ev.GetVmExited(); x != nil {
				return errors.New(vmExited(vm.GetName()+" exited during boot", x))
			}
			// The reason names the cause first: the job network, the
			// scratch disk, a disk or the VM, or, for the plugin's own
			// bug, the job spec.
			if f := ev.GetFailed(); f != nil {
				if f.GetBug() {
					return &bugError{errors.New(f.GetReason())}
				}
				return errors.New(f.GetReason())
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
			s.log.Error("the job's VM died; its calls fail until the runner removes it", "env", j.id, "err", err)
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
// its last output, as a failed Start's error does. It is not Unavailable,
// which the runner reads as the plugin being gone.
func vmExitedError(name string, x *jobv1.VMExited) error {
	return status.Error(codes.FailedPrecondition, vmExited(name+" exited", x))
}

// vmExited is the message for VM x, which what names: `VM <what>
// (<code>)`, then the VM's last output from x's tails.
func vmExited(what string, x *jobv1.VMExited) string {
	return fmt.Sprintf("VM %s (%d)%s", what, x.GetExitCode(), vmOutput(x.GetLogTail(), x.GetConsoleTail()))
}

// vmOutput formats a VM's last output for an error: the last tailLines
// lines of the last tailBytes of its VM log and of its console, each on the
// lines after its heading, with their newlines, and nothing for an empty
// one. A tail comes from a file the guest writes, so it is cut to size here
// whatever its source, and loses its invalid UTF-8 and carriage returns.
func vmOutput(log, console string) string {
	var b strings.Builder
	for _, f := range []struct{ heading, text string }{{"VM log", log}, {"console", console}} {
		if l := lastLines(f.text); l != "" {
			fmt.Fprintf(&b, "\n%s:\n%s", f.heading, l)
		}
	}
	return b.String()
}

// lastLines returns the last tailLines lines of the last tailBytes of s,
// without a trailing newline.
func lastLines(s string) string {
	if len(s) > tailBytes {
		s = s[len(s)-tailBytes:]
	}
	s = strings.ToValidUTF8(s, "")
	s = strings.TrimRight(strings.ReplaceAll(s, "\r", ""), "\n")
	end := len(s)
	for range tailLines {
		i := strings.LastIndexByte(s[:end], '\n')
		if i < 0 {
			return s
		}
		end = i
	}
	return s[end+1:]
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
// An entry without `=` names no variable and is dropped, as Exec drops it.
func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

// makeBootRoot makes the VM's boot root at root: empty proc, sys and dev
// mount points, so images without them boot (GST-2), and in its
// .runner-microvm directory a hard link to the agent, not a copy, and the
// agent's boot config as a file. libkrun shares it read-only as the VM's
// root, and it is the bottom layer of the guest's root, so the agent and its
// config stay visible in the job's root: the config holds nothing a step
// can't read anyway, such as the image's environment.
func makeBootRoot(root, agent string, boot *agentv1.BootConfig) error {
	b, err := proto.Marshal(boot)
	if err != nil {
		return err
	}
	if err := os.Mkdir(root, 0o755); err != nil { //nolint:gosec // G301: the guest's /, which every guest user must traverse
		return err
	}
	for _, d := range []string{"proc", "sys", "dev", agentDir} {
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
