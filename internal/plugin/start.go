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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// Start pulls the image of every VM of the job, the job's container and
// each service, waits for the job's admission to its disks and memory,
// starts its job process, which boots every VM, waits until every VM is
// ready (waitReady), and ends with StartComplete carrying the job's
// container's image's environment (PROTO-8). Before that it sends a
// progress line for each image, each wait if there is one, each VM and the
// total (PROTO-9). A failed Start
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
		s.log.Error("start failed", "env", j.id, "err", withoutOutput(err))
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

// jobVM is one VM of a job as Start boots it: the job's container, at index
// 0, or one of its services.
type jobVM struct {
	index int
	// name is the VM's name on the job network, in /etc/hosts, log lines
	// and messages; hostname its kernel's hostname, a DNS label.
	name, hostname string
	// ref is the image the VM runs.
	ref string
	// service is the service the VM runs, nil for the job's container.
	service *service
	// img is its image, once acquired.
	img imagecache.Image
}

// vms returns j's VMs, in the job spec's order: the job's container, then
// each service as the runner listed it.
func (j *job) vms() []*jobVM {
	vms := []*jobVM{{index: 0, name: j.hostname, hostname: j.hostname, ref: j.image}}
	for i := range j.services {
		svc := &j.services[i]
		vms = append(vms, &jobVM{index: i + 1, name: svc.name, hostname: svc.hostname, ref: svc.image, service: svc})
	}
	return vms
}

// vmName is the name of VM index of j: the job's container's hostname, or
// a service's name.
func (j *job) vmName(index int) string {
	if index > 0 && index <= len(j.services) {
		return j.services[index-1].name
	}
	return j.hostname
}

// what names vm in a message's cause: nothing for the job's container,
// whose image is the job's, `service <name>: ` for a service.
func (vm *jobVM) what() string {
	if vm.service == nil {
		return ""
	}
	return "service " + vm.name + ": "
}

// boot does Start's work on j, which is locked, sending its progress to p,
// and returns the job's container's image. On error, j.proc and j.agent may
// be set: the caller stops them.
func (s *Server) boot(ctx context.Context, j *job, p *progress) (imagecache.Image, error) {
	vms := j.vms()
	for _, vm := range vms {
		for _, d := range []string{s.vmStateDir(j.id, vm.index), s.vmRuntimeDir(j.id, vm.index)} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				return imagecache.Image{}, err
			}
		}
	}
	if err := s.acquireImages(ctx, j, vms, p); err != nil {
		return imagecache.Image{}, err
	}
	if err := s.admit(ctx, j, p); err != nil {
		return imagecache.Image{}, err
	}
	// Every VM's /etc/hosts names every VM of the job, by its name.
	hosts := make([]*agentv1.Host, len(vms))
	for i, vm := range vms {
		addr, _, err := vmAddress(vm.index)
		if err != nil {
			return imagecache.Image{}, &bugError{err}
		}
		hosts[i] = &agentv1.Host{Name: vm.name, Address: addr.String()}
	}
	specVMs := make([]*jobv1.VM, len(vms))
	for i, vm := range vms {
		// A VM's own hostname maps to its own address, as Docker's does, when
		// it isn't its name.
		own := hosts
		if vm.hostname != vm.name {
			own = append(slices.Clone(hosts), &agentv1.Host{Name: vm.hostname, Address: hosts[i].GetAddress()})
		}
		v, err := s.vmSpec(j, vm, own)
		if err != nil {
			return imagecache.Image{}, err
		}
		specVMs[i] = v
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
		Vms:             specVMs,
	}, filepath.Join(s.jobStateDir(j.id), jobLogName))
	if err != nil {
		return imagecache.Image{}, fmt.Errorf("job process: %w", err)
	}
	j.proc = proc
	// Start dials every VM's agent, and keeps only the job's container's:
	// Exec and Copy name only the job. The agent socket's path can outgrow
	// connect's 107 bytes, so it is dialled through the runtime directory.
	agents := make([]*agentclient.Client, len(vms))
	defer func() {
		for _, a := range agents[1:] {
			if a != nil {
				_ = a.Close()
			}
		}
	}()
	for i, vm := range vms {
		a, err := agentclient.New(s.runtime.path(j.id + "/" + strconv.Itoa(vm.index) + "/agent.sock"))
		if err != nil {
			return imagecache.Image{}, &bugError{err}
		}
		agents[i] = a
		if i == 0 {
			j.agent = a
		}
	}
	if err := s.waitReady(ctx, j, specVMs, agents, p, booting); err != nil {
		return imagecache.Image{}, err
	}
	return vms[0].img, nil
}

// vmStateDir is the state directory of VM index of the job id,
// <state>/jobs/<id>/<n>/, and vmRuntimeDir its runtime directory,
// <runtime>/<id>/<n>/: named by the VM's index, never its name, which a
// workflow chooses.
func (s *Server) vmStateDir(id string, index int) string {
	return filepath.Join(s.jobStateDir(id), strconv.Itoa(index))
}

func (s *Server) vmRuntimeDir(id string, index int) string {
	return filepath.Join(s.cfg.Dirs.Runtime, id, strconv.Itoa(index))
}

// acquireImages acquires the image of every VM of j at once, before the
// wait for admission, so a bad reference or a registry error fails at once;
// the image cache's plugin-wide limit bounds their pulls together. It sends
// each image's progress line as it is ready. A service's image with no
// command, neither an Entrypoint nor a Cmd, fails it too. The first failure
// cancels the other Acquires, and is returned once every one has returned,
// naming its service.
func (s *Server) acquireImages(ctx context.Context, j *job, vms []*jobVM, p *progress) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type acquired struct {
		vm   *jobVM
		img  imagecache.Image
		took time.Duration
		err  error
	}
	results := make(chan acquired, len(vms))
	for _, vm := range vms {
		go func() {
			begin := time.Now()
			img, err := s.cfg.Cache.Acquire(ctx, imagecache.Request{Job: j.id, VM: vm.index, Ref: vm.ref, ForcePull: j.profile.ForcePull})
			if err == nil && vm.service != nil {
				err = checkCommand(img)
			}
			results <- acquired{vm, img, time.Since(begin), err}
		}()
	}
	var first error
	for range vms {
		r := <-results
		if r.err != nil {
			if first == nil {
				first = fmt.Errorf("%simage %s: %w", r.vm.what(), r.vm.ref, r.err)
				cancel()
			}
			continue
		}
		r.vm.img = r.img
		if first != nil {
			continue
		}
		if r.vm.service == nil {
			p.line("image %s ready (%s, %d layers) in %s", r.vm.ref, r.img.Digest, len(r.img.Layers), seconds(r.took))
		} else {
			p.line("image %s ready for service %s (%s, %d layers) in %s", r.vm.ref, r.vm.name, r.img.Digest, len(r.img.Layers), seconds(r.took))
		}
	}
	return first
}

// checkCommand fails unless img has a command a service can run: its
// Entrypoint followed by its Cmd, with a program to start. The plugin
// protocol passes a service no command of its own, and forgejo-runner's
// Docker backend fails such a service too.
func checkCommand(img imagecache.Image) error {
	argv := serviceArgv(img)
	switch {
	case len(argv) == 0:
		return errors.New("it has neither an Entrypoint nor a Cmd, so the service has no command to run")
	case argv[0] == "":
		return errors.New("its command, its Entrypoint followed by its Cmd, starts with an empty program name")
	}
	return nil
}

// serviceArgv is a service's command from its image: the Entrypoint
// followed by the Cmd, as Docker runs it.
func serviceArgv(img imagecache.Image) []string {
	return append(slices.Clone(img.Entrypoint), img.Cmd...)
}

// vmSpec returns the job spec's entry for vm of j, every VM of the job
// named in hosts, and makes its boot root. Every VM has the shape of j's
// profile: its CPUs, memory, scratch size and scratch paths. Only the job's
// container gets the profile's mounts and disks, which are the job's
// storage: a read-write disk can't be attached to two VMs of one job, and
// a service has no use for the job's caches. A service's boot config
// carries its command, never its own environment, whose values may be
// secrets, which must not appear in host files (LOG-2): StartService
// carries that.
func (s *Server) vmSpec(j *job, vm *jobVM, hosts []*agentv1.Host) (*jobv1.VM, error) {
	addr, mac, err := vmAddress(vm.index)
	if err != nil {
		return nil, &bugError{err}
	}
	img := vm.img
	boot := &agentv1.BootConfig{
		LayerDir:      guestLayerDir,
		LayerTag:      layerTag,
		Layers:        img.Layers,
		ScratchDevice: scratchDevice,
		ScratchPaths:  j.profile.ScratchPaths,
		Network:       bootNetwork(addr),
		Hostname:      vm.hostname,
		Image:         &agentv1.Image{Env: img.Env, User: img.User, WorkingDir: img.WorkingDir, DefaultPath: defaultPath},
		Hosts:         hosts,
		VsockPort:     agentPort,
		LogLevel:      s.cfg.LogLevel,
	}
	var mounts []*jobv1.Mount
	var disks []*jobv1.Disk
	if svc := vm.service; svc != nil {
		boot.Service = &agentv1.Service{Argv: serviceArgv(img)}
	} else {
		// The directories steps expect.
		for _, d := range guestDirs {
			boot.GuestDirs = append(boot.GuestDirs, &agentv1.GuestDir{Path: d.path, Mode: d.mode})
		}
		// Each mount is a share of its own, under a tag only its position
		// names; the job process checks its source again before the VM
		// starts (STO-1, STO-2, STO-5).
		for i, m := range j.profile.Mounts {
			mounts = append(mounts, &jobv1.Mount{Source: m.Source, Tag: mountTag(i), ReadWrite: m.ReadWrite})
			boot.Mounts = append(boot.Mounts, &agentv1.Mount{Tag: mountTag(i), Target: m.Target, ReadWrite: m.ReadWrite})
		}
		// Each disk is attached under a block id only its position names,
		// which the agent finds it by; the job process opens its source
		// before the VM starts (STO-1, STO-4).
		for i, d := range j.profile.Disks {
			disks = append(disks, &jobv1.Disk{Source: d.Source, BlockId: diskBlockID(i), ReadWrite: d.ReadWrite})
			boot.Disks = append(boot.Disks, &agentv1.Disk{BlockId: diskBlockID(i), Target: d.Target, ReadWrite: d.ReadWrite})
		}
	}
	state := s.vmStateDir(j.id, vm.index)
	root := filepath.Join(state, "root")
	if err := makeBootRoot(root, s.cfg.Agent, boot); err != nil {
		return nil, fmt.Errorf("boot root %s: %w", root, err)
	}
	return &jobv1.VM{
		Name:             vm.name,
		Cpus:             j.profile.CPUs,
		MemoryMib:        j.profile.MemoryMiB,
		ScratchDisk:      filepath.Join(state, "scratch.ext4"),
		ScratchSizeBytes: j.profile.ScratchBytes(),
		VmLog:            filepath.Join(state, vmLogName),
		Console:          filepath.Join(state, consoleName),
		AgentSocket:      filepath.Join(s.vmRuntimeDir(j.id, vm.index), "agent.sock"),
		AgentPort:        agentPort,
		RootDir:          root,
		LayerDir:         img.LinkDir,
		LayerTag:         layerTag,
		Mounts:           mounts,
		Disks:            disks,
		GuestAddress:     addr.String(),
		GuestMac:         mac.String(),
		Boot:             boot,
	}, nil
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
	need := jobMemoryMiB(j.profile, j.services)
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

// waitReady waits until every VM of j, vms in the job spec's order and
// agents their clients, is ready, sending each VM's progress line as it is,
// timed from booting: the job's container once its agent answers Ping, a
// service's VM once its agent has answered Ping and then StartService, sent
// once, with the service's env, has started its command. It fails as soon
// as any VM exits or the job process fails, naming the VM, once a service's
// command can't start, quoting the agent, or once the ready timeout, which
// bounds the Pings and StartServices together, passes before every VM is
// ready (VM-8).
func (s *Server) waitReady(ctx context.Context, j *job, vms []*jobv1.VM, agents []*agentclient.Client, p *progress, booting time.Time) error {
	readyCtx, cancel := context.WithTimeout(ctx, s.cfg.ReadyTimeout)
	defer cancel()
	type answer struct {
		i int
		// pinged says the agent answered Ping, so err, if any, is
		// StartService's.
		pinged bool
		err    error
	}
	answers := make(chan answer, len(agents))
	// starting[i] is set once VM i's StartService is sent.
	starting := make([]atomic.Bool, len(agents))
	var wg sync.WaitGroup
	for i, a := range agents {
		wg.Go(func() {
			err := a.WaitReady(readyCtx)
			if err != nil || !isService(vms[i]) {
				answers <- answer{i, false, err}
				return
			}
			starting[i].Store(true)
			// Never sent again: a second call fails, and the service
			// would start once anyway.
			answers <- answer{i, true, a.StartService(readyCtx, j.services[i-1].env.Reveal())}
		})
	}
	// Every goroutine ends, once readyCtx does at the latest.
	defer func() { cancel(); wg.Wait() }()
	// answered[i] is set once VM i's answer is read.
	ready, answered := make([]bool, len(vms)), make([]bool, len(vms))
	left := len(vms)
	// Once the job process has exited, its events still to come say why:
	// they end when the launcher reaches the end of its stdout, which no
	// process is left to hold open, so they are read to their end before
	// the exit is reported. It names the first VM not yet ready.
	events, exited := j.proc.Events, j.proc.Exited
	exitedErr := func() error {
		vm := vms[slices.Index(ready, false)]
		return &outputError{fmt.Sprintf("VM %s: its job process exited during boot", vm.GetName()), vmOutput(tail(vm.GetVmLog()), tail(vm.GetConsole())), isService(vm)}
	}
	// A StartService whose connection failed is how the plugin first sees
	// a service's VM die: lost is that failure, reported once the VM's exit
	// hasn't come within the lost agent grace.
	var lost error
	var lostC <-chan time.Time
	for {
		select {
		case a := <-answers:
			vm := vms[a.i]
			answered[a.i] = true
			if a.err == nil {
				ready[a.i] = true
				p.line("VM %s (%d CPUs, %d MiB) ready in %s", vm.GetName(), vm.GetCpus(), vm.GetMemoryMib(), seconds(time.Since(booting)))
				if left--; left == 0 {
					return nil
				}
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			output := vmOutput(tail(vm.GetVmLog()), tail(vm.GetConsole()))
			switch {
			case !a.pinged:
				summary := fmt.Sprintf("VM %s: the agent did not answer within %v", vm.GetName(), s.cfg.ReadyTimeout)
				return &outputError{summary, output, isService(vm)}
			// gRPC ends a call at its deadline on its own, through the
			// server's grpc-timeout or the stream's reset, so its
			// DeadlineExceeded can come before readyCtx reports its end.
			// The agent never sends that code itself.
			case readyCtx.Err() != nil || status.Code(a.err) == codes.DeadlineExceeded:
				summary := fmt.Sprintf("VM %s: the agent did not start its service within %v", vm.GetName(), s.cfg.ReadyTimeout)
				return &outputError{summary, output, true}
			case status.Code(a.err) == codes.FailedPrecondition:
				return &outputError{serviceNotStarted(vm, a.err), output, true}
			case lost == nil:
				lost = &outputError{fmt.Sprintf("VM %s: the agent's connection failed as it started its service: %v", vm.GetName(), a.err), output, true}
				timer := time.NewTimer(s.cfg.LostAgentGrace)
				defer timer.Stop()
				lostC = timer.C
			}
		case <-lostC:
			return lost
		case ev, ok := <-events:
			if !ok {
				if exited == nil {
					return exitedErr()
				}
				events = nil
				continue
			}
			if x := ev.GetVmExited(); x != nil {
				i := int(x.GetVm())
				if i < 0 || i >= len(vms) {
					return &bugError{fmt.Errorf("the job process reported VM %d exited, in a job of %d VMs", i, len(vms))}
				}
				// The agent answers a command that can't start before it
				// exits, and its answer says why the VM did: it wins over
				// the exit it caused. The call ends with readyCtx at the
				// latest, and at once as the VM's socket closes.
				if starting[i].Load() && !answered[i] {
					for a := range answers {
						if a.i == i {
							if status.Code(a.err) == codes.FailedPrecondition && readyCtx.Err() == nil {
								return &outputError{serviceNotStarted(vms[i], a.err), vmOutput(x.GetLogTail(), x.GetConsoleTail()), true}
							}
							break
						}
					}
				}
				return vmExited(vms[i].GetName()+" exited during boot", x, isService(vms[i]))
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

// maxAgentMessage is how much of an agent's error message a Start's error
// quotes: the guest sends it, so its size is never taken on trust.
const maxAgentMessage = 1 << 10

// serviceNotStarted is the summary of a failed Start whose service's VM, vm,
// answered StartService with err, the agent's FailedPrecondition: the VM's
// name, the service's, and the agent's message, which names only the
// command's argv[0] and why, never a value of its env (LOG-2).
func serviceNotStarted(vm *jobv1.VM, err error) string {
	msg := status.Convert(err).Message()
	if len(msg) > maxAgentMessage {
		msg = msg[:maxAgentMessage]
	}
	return fmt.Sprintf("VM %s: its service did not start: %s", vm.GetName(), strings.ToValidUTF8(msg, ""))
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
			if x := ev.GetVmExited(); x != nil {
				if x.GetVm() == 0 {
					gone(vmExitedError(j.hostname, x))
				} else if j.gone.Err() == nil && j.ctx.Err() == nil {
					// A service that stops doesn't end the job, as under
					// Docker: a step fails against it. Once the job's
					// container is gone, the job process stops the rest.
					s.log.Warn("a service's VM exited; the job runs on", "env", j.id, "vm", j.vmName(int(x.GetVm())),
						"exit_code", x.GetExitCode())
				}
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
	return status.Error(codes.FailedPrecondition, vmExited(name+" exited", x, false).Error())
}

// vmExited is the error for VM x, which what names: `VM <what> (<code>)`,
// then the VM's last output from x's tails; service says the VM is a
// service's (outputError).
func vmExited(what string, x *jobv1.VMExited, service bool) *outputError {
	return &outputError{fmt.Sprintf("VM %s (%d)", what, x.GetExitCode()), vmOutput(x.GetLogTail(), x.GetConsoleTail()), service}
}

// isService says whether vm runs one of the job's services, not the job's
// container.
func isService(vm *jobv1.VM) bool { return vm.GetBoot().GetService() != nil }

// outputError is a failure that quotes what a VM wrote: summary says what
// failed (the VM's name, what happened, its exit code), and output is the
// VM's last output after it, such as vmOutput's tails. Its message is both,
// and goes to the runner, which masks the job's secrets in what it shows.
// The plugin's own log masks nothing, so when service is set it shows only
// summary (withoutOutput): a service's output is produced under its env,
// which may carry the workflow's secrets. The job's container's output, the
// kernel's and the agent's, which LOG-2 keeps free of them, stays in the
// log, which then says why it failed to boot.
type outputError struct {
	summary, output string
	service         bool
}

func (e *outputError) Error() string { return e.summary + e.output }

// withoutOutput is err's message for the plugin's log: each service's
// outputError in err's tree, through Unwrap, is cut to its summary.
func withoutOutput(err error) string {
	msg := err.Error()
	var walk func(error)
	walk = func(err error) {
		switch e := err.(type) { //nolint:errorlint // walks the tree itself, to find every outputError, not the first
		case *outputError:
			if e.service && e.output != "" {
				msg = strings.ReplaceAll(msg, e.Error(), e.summary)
			}
		case interface{ Unwrap() error }:
			walk(e.Unwrap())
		case interface{ Unwrap() []error }:
			for _, e := range e.Unwrap() {
				walk(e)
			}
		}
	}
	walk(err)
	return msg
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
