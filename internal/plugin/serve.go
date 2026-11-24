// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/config"
	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/imagecache"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/joblaunch"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/logging"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/payload"
)

// usage is what runner-microvm prints for no subcommand, an unknown one, or
// an argument.
const usage = `usage: runner-microvm <command>

Commands:
  serve   run the plugin; SIGINT or SIGTERM drains it, and a second one stops
          it at once; SIGHUP reloads the profiles
  check   validate the config file, print every error, and exit

The config file is ` + config.DefaultPath + `, or the file ` + config.PathEnv + ` names.
`

// Main runs the subcommand in args, reading the environment through getenv,
// and returns its exit code: serve runs the plugin until a drain, which
// SIGINT or SIGTERM begins, has ended;
// check validates the config file and exits 0, or 1 after printing every
// error. Anything else prints the usage and exits 2.
func Main(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) != 1 {
		_, _ = io.WriteString(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return serveCmd(getenv)
	case "check":
		return checkCmd(stdout, stderr, getenv)
	default:
		_, _ = io.WriteString(stderr, usage)
		return 2
	}
}

// checkCmd is check: it validates the config file, and reports to stdout that it is valid,
// or every error to stderr.
func checkCmd(stdout, stderr io.Writer, getenv func(string) string) int {
	cfg, err := config.Load(getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if cfg.File == "" {
		_, _ = fmt.Fprintf(stdout, "no config file at %s: every setting takes its built-in value\n", config.DefaultPath)
	} else {
		_, _ = fmt.Fprintf(stdout, "config %s: valid\n", cfg.File)
	}
	return 0
}

// serveCmd is serve: it runs the plugin until its drain, which SIGINT or
// SIGTERM begins, has ended, or a second one of them, and reloads the config
// file on each SIGHUP.
func serveCmd(getenv func(string) string) int {
	log := logging.Setup(Name)
	// First, so a SIGHUP during startup can't end the plugin, which is
	// SIGHUP's default action. One queued signal is enough: a reload reads
	// the file as it is then. Two queued SIGINTs or SIGTERMs are: the first
	// drains, the second stops at once.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	term := make(chan os.Signal, 2)
	signal.Notify(term, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(term)
	cfg, err := config.Load(getenv)
	if err != nil {
		log.Error("stopped: invalid config", "err", err)
		return 1
	}
	logConfig(log, cfg)
	if err := run(context.Background(), log, cfg, hup, term, embeddedPayload, newImageCache, joblaunch.New); err != nil {
		log.Error("stopped", "err", err)
		return 1
	}
	return 0
}

// libDirSetting is lib_dir as the log shows it.
func libDirSetting(dir string) string {
	if dir == "" {
		return "unset (by soname)"
	}
	return dir
}

// logConfig logs the config file read, or that there was none, every
// plugin-level setting in effect, and each profile, default always among
// them.
func logConfig(log *slog.Logger, cfg *config.Config) {
	if cfg.File == "" {
		log.Info("no config file: every setting takes its built-in value", "path", config.DefaultPath)
	} else {
		log.Info("read the config file", "path", cfg.File)
	}
	maxMemory := strconv.FormatUint(cfg.MaxMemoryMiB, 10)
	if cfg.MaxMemoryMiB == 0 {
		maxMemory = "0 (no limit)"
	}
	log.Info("settings", "listen", "unix://"+cfg.Listen, "state_dir", cfg.StateDir, "cache_dir", cfg.CacheDir,
		"runtime_dir", cfg.RuntimeDir, "max_memory_mib", maxMemory, "cache_max_gib", cfg.CacheMaxGiB,
		"lib_dir", libDirSetting(cfg.LibDir))
	logProfiles(log, cfg.Profiles)
}

// newCacheFunc opens the image cache in dir, with a size cap of maxSize
// bytes. run takes it so tests can see the cap it passes.
type newCacheFunc func(log *slog.Logger, dir string, maxSize int64) (ImageCache, error)

// newImageCache is the newCacheFunc serve uses: imagecache.New.
func newImageCache(log *slog.Logger, dir string, maxSize int64) (ImageCache, error) {
	return imagecache.New(log, dir, imagecache.WithMaxSize(maxSize))
}

// newLauncherFunc makes the job launcher. run takes it so tests can see that
// run closes the launcher it made.
type newLauncherFunc func(log *slog.Logger) (*joblaunch.Launcher, error)

// payloadFunc returns the job binary and the agent that run writes to
// <state>/bin/. run takes it so tests can give it a fake job binary.
type payloadFunc func() (jobBinary, agent []byte, err error)

// embeddedPayload is the payloadFunc serve uses: the job binary and the
// agent embedded in runner-microvm.
func embeddedPayload() (jobBinary, agent []byte, err error) {
	if jobBinary, err = payload.JobBinary(); err != nil {
		return nil, nil, err
	}
	if agent, err = payload.Agent(); err != nil {
		return nil, nil, err
	}
	return jobBinary, agent, nil
}

// run runs the plugin with cfg until ctx ends or its drain has ended,
// reloading the config file on each signal from hup; the first signal from
// term drains it, and a second stops it at once (serveWithReloads). A signal
// from term during startup stops it before it serves. It writes the job
// binary and the agent pay returns, opens the image cache with newCache, and
// makes the job launcher with newLauncher, which it closes on every return.
func run(ctx context.Context, log *slog.Logger, cfg *config.Config, hup, term <-chan os.Signal, pay payloadFunc,
	newCache newCacheFunc, newLauncher newLauncherFunc,
) error {
	// An architecture libkrun doesn't support, or a missing host tool,
	// stops startup before anything is written.
	arch, err := runnerArch(runtime.GOARCH)
	if err != nil {
		return err
	}
	mkfs, err := lookMkfs()
	if err != nil {
		return err
	}
	dirs := dirsOf(cfg)
	// The locks come before anything else writes to the directories, and
	// are held until the plugin exits.
	locks, err := lockDirs(dirs)
	if err != nil {
		return err
	}
	defer func() { _ = locks.Close() }()
	// The job launcher, its forking thread and its liveness pipe, last
	// until run returns, after the server has ended every job.
	launcher, err := newLauncher(log)
	if err != nil {
		return err
	}
	defer launcher.Close()
	// The image cache's own startup cleanup needs only the locks.
	cache, err := newCache(log, dirs.Cache, cfg.CacheMaxBytes())
	if err != nil {
		return err
	}
	// SHORTCUT(#11): no cleanup at startup of what a crashed plugin left,
	// beyond the stale files in <state>/bin/ and the image cache's own
	// (imagecache.New).
	if err := joblaunch.SetSubreaper(); err != nil {
		return err
	}
	jobPayload, agentPayload, err := pay()
	if err != nil {
		return err
	}
	bin := filepath.Join(dirs.State, "bin")
	jobBinary, err := writeJobBinary(ctx, bin, jobPayload)
	if err != nil {
		return err
	}
	agent, err := writeAgent(bin, agentPayload)
	if err != nil {
		return err
	}
	// The locks prove no other plugin uses bin/, so every other file there
	// is a stale hash or a crashed write's temp file.
	if err := pruneDir(bin, jobBinary, agent); err != nil {
		return err
	}
	// Only the VM child loads libkrun, so the job binary checks the
	// libraries, exactly as a VM child loads them, before the plugin
	// serves: a plugin that fails every Start is worse than one that won't
	// start.
	libs, sig, err := checkLibsUntilSignal(ctx, term, jobBinary, cfg.LibDir)
	if err != nil {
		return err
	}
	if sig != nil {
		log.Info("stopped before serving", "signal", sig)
		return nil
	}
	log.Info("the job binary loaded libkrun and libkrunfw", "libkrun", libs.libkrun, "libkrunfw", libs.libkrunfw, "lib_dir", libDirSetting(cfg.LibDir))
	ready, err := parseReadyTimeout(testReadyTimeout)
	if err != nil {
		return err
	}
	srv, err := NewServer(log, Config{
		Dirs:         dirs,
		LibDir:       cfg.LibDir,
		JobBinary:    jobBinary,
		MkfsExt4:     mkfs,
		Agent:        agent,
		Profiles:     cfg.Profiles,
		Cache:        cache,
		Launcher:     launcher,
		Arch:         arch,
		ReadyTimeout: ready,
	})
	if err != nil {
		return err
	}
	// With no job yet, a drain asked for during startup ends at once.
	select {
	case sig := <-term:
		log.Info("stopped before serving", "signal", sig)
		return srv.Close(context.WithoutCancel(ctx))
	default:
	}
	lis, closeLis, err := listen(ctx, dirs.Listen)
	if err != nil {
		return errors.Join(err, srv.Close(context.WithoutCancel(ctx)))
	}
	defer closeLis()
	log.Info("serving", "listen", dirs.Listen, "state", dirs.State, "cache", dirs.Cache, "runtime", dirs.Runtime,
		"job_binary", jobBinary, "agent", agent, "mkfs_ext4", mkfs)
	return serveWithReloads(ctx, log, lis, srv, hup, term, cfg, (*config.Config).Reload)
}

// serveWithReloads serves srv on lis, reloading the config file with read on
// each signal from hup, then closes srv, which removes every job left. The
// first signal from term begins srv's drain, and serving ends once the
// drain has; a second signal, or ctx ending, ends it at once. It doesn't
// wait for a reload still reading the file: one blocked on a stalled network
// mount would keep the plugin, and its directory locks, alive until SIGKILL.
// That reload finishes on its own and finds srv closed, so it changes
// nothing; it touches nothing else the caller frees.
func serveWithReloads(ctx context.Context, log *slog.Logger, lis net.Listener, srv *Server, hup, term <-chan os.Signal, cfg *config.Config, read readConfig) error {
	ctx, stopNow := context.WithCancel(ctx)
	defer stopNow()
	go reloadOnSignal(ctx, log, hup, srv, cfg, read)
	var signals sync.WaitGroup
	signals.Go(func() { drainOnSignals(ctx, log, term, srv, stopNow) })
	err := Serve(ctx, lis, srv)
	stopNow()
	signals.Wait()
	// Every job's VM ends with the plugin: after a drain there is none;
	// after a second signal, Close removes each one as Remove does.
	return errors.Join(err, srv.Close(context.WithoutCancel(ctx)))
}

// drainOnSignals begins srv's drain at the first signal from term, and calls
// stopNow at a second one, until ctx ends or the drain has ended.
func drainOnSignals(ctx context.Context, log *slog.Logger, term <-chan os.Signal, srv *Server, stopNow context.CancelFunc) {
	select {
	case <-ctx.Done():
		return
	case sig := <-term:
		log.Info("draining: taking no new jobs, and serving each job until it is removed; a second signal removes every job at once",
			"signal", sig, "jobs", srv.jobCount())
		srv.Drain()
	}
	select {
	case <-ctx.Done():
	case <-srv.Drained():
		log.Info("drained: no job is left; stopping")
	case sig := <-term:
		log.Warn("a second signal: removing every job", "signal", sig, "jobs", srv.jobCount())
		stopNow()
	}
}

// testReadyTimeout is the ready timeout, as a duration string, that a test's
// own build of the plugin sets with -ldflags -X, so that the test of a VM
// whose agent never answers needn't wait the real 30 s. It is empty in every
// shipped binary, which has no config key or flag for it.
var testReadyTimeout string

// parseReadyTimeout returns testReadyTimeout's duration, or zero, which
// NewServer takes as the default, when it is empty.
func parseReadyTimeout(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("the test-only ready timeout: %w", err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("the test-only ready timeout %q is not positive", s)
	}
	return d, nil
}

// lookMkfs finds the host's mkfs.ext4 on PATH. The job process formats each
// VM's scratch disk with it, so without it no job could start (VM-5).
func lookMkfs() (string, error) {
	path, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		return "", fmt.Errorf("mkfs.ext4 is not on PATH: runner-microvm formats each VM's scratch disk with it; install e2fsprogs: %w", err)
	}
	return filepath.Abs(path)
}

// writeJobBinary writes the job binary bin to
// <dir>/runner-microvm-job-<first 12 hex of its sha256>, always afresh: a
// temp file, fsync, rename, then fsync of dir. It then runs it once with
// `version`, so a directory that can't execute fails startup.
func writeJobBinary(ctx context.Context, dir string, bin []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := hashedPath(dir, "runner-microvm-job", bin)
	if err := writeFileAtomic(dir, path, bin, 0o700); err != nil {
		return "", fmt.Errorf("write the job binary: %w", err)
	}
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out := &cappedBuffer{max: maxJobBinaryOutput}
	if err := runJobBinary(vctx, path, out, out, "version"); err != nil {
		return "", fmt.Errorf("run the job binary %s in %s, the state directory: %w: %s", path, dir, err, out)
	}
	return path, nil
}

// runJobBinary runs the job binary at path with args until it exits, its
// outputs on stdout and stderr, or kills it when ctx ends. It leads a process
// group of its own, so a SIGINT a terminal sends the plugin's group doesn't
// reach it, and ctx's end kills the whole group, whatever it started. It
// gets Pdeathsig SIGKILL, so it dies with the plugin: this goroutine's OS
// thread forks it and stays locked until it is reaped, since Pdeathsig fires
// when the forking thread exits, not the process.
func runJobBinary(ctx context.Context, path string, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, path, args...) //nolint:gosec // G204: the job binary the plugin just wrote
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// What the group started may hold the output pipes past the kill.
	cmd.WaitDelay = time.Second
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	return cmd.Run()
}

// libsTimeout bounds the job binary's check of the libraries: loading two
// libraries takes milliseconds, but one on a stalled network mount must not
// hang startup.
const libsTimeout = 30 * time.Second

// libFiles are the files the dynamic loader picked for libkrun and
// libkrunfw.
type libFiles struct {
	libkrun, libkrunfw string
}

// The sonames the job binary's libs reports the files of.
const (
	libkrunSoname   = "libkrun.so.1"
	libkrunfwSoname = "libkrunfw.so.5"
)

// maxJobBinaryOutput bounds what the plugin keeps of each of the job
// binary's outputs at startup: a version, two lines of paths, or an error.
const maxJobBinaryOutput = 64 << 10

// checkLibsUntilSignal runs checkLibs, and ends it early at a signal from
// term, which it then returns, with no error: a SIGINT or SIGTERM during the
// check stops the plugin before it serves, as at any other point of startup.
func checkLibsUntilSignal(ctx context.Context, term <-chan os.Signal, jobBinary, libDir string) (libFiles, os.Signal, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	got := make(chan os.Signal, 1)
	var watch sync.WaitGroup
	watch.Go(func() {
		select {
		case sig := <-term:
			got <- sig
			cancel()
		case <-ctx.Done():
		}
	})
	libs, err := checkLibs(ctx, jobBinary, libDir)
	cancel()
	watch.Wait()
	select {
	case sig := <-got:
		return libFiles{}, sig, nil
	default:
		return libs, nil, err
	}
}

// checkLibs runs the job binary jobBinary with `libs`, which loads libkrun
// and libkrunfw as a VM child does, from libDir or, when it is empty, by
// soname, and requires every function the VM child calls. It returns the
// files the loader picked, or the job binary's error.
func checkLibs(ctx context.Context, jobBinary, libDir string) (libFiles, error) {
	where, args := "by soname", []string{"libs"}
	if libDir != "" {
		where, args = "from lib_dir "+libDir, append(args, libDir)
	}
	ctx, cancel := context.WithTimeout(ctx, libsTimeout)
	defer cancel()
	stdout, stderr := &cappedBuffer{max: maxJobBinaryOutput}, &cappedBuffer{max: maxJobBinaryOutput}
	if err := runJobBinary(ctx, jobBinary, stdout, stderr, args...); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w after %s", ctx.Err(), libsTimeout)
		}
		return libFiles{}, fmt.Errorf("load libkrun and libkrunfw %s: %w: %s", where, err, strings.TrimSpace(stderr.String()))
	}
	files := map[string]string{}
	for line := range strings.Lines(stdout.String()) {
		if name, file, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "="); ok {
			files[name] = file
		}
	}
	libs := libFiles{libkrun: files[libkrunSoname], libkrunfw: files[libkrunfwSoname]}
	if libs.libkrun == "" || libs.libkrunfw == "" {
		return libFiles{}, fmt.Errorf("load libkrun and libkrunfw %s: the job binary's libs did not name both files: %q", where, stdout.String())
	}
	return libs, nil
}

// cappedBuffer keeps the first max bytes written to it, and drops the rest.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

// writeAgent writes the agent to
// <dir>/runner-microvm-agent-<first 12 hex of its sha256>, always afresh, the
// same way as the job binary. Each VM's boot root holds a hard link to it, so
// it is written once per plugin, not once per VM.
func writeAgent(dir string, agent []byte) (string, error) {
	path := hashedPath(dir, "runner-microvm-agent", agent)
	// Executable by the guest's root, which runs it from the boot root.
	if err := writeFileAtomic(dir, path, agent, 0o755); err != nil {
		return "", fmt.Errorf("write the agent: %w", err)
	}
	return path, nil
}

// hashedPath is <dir>/<name>-<first 12 hex of data's sha256>: the content
// hash makes upgrades clean.
func hashedPath(dir, name string, data []byte) string {
	sum := sha256.Sum256(data)
	return filepath.Join(dir, name+"-"+hex.EncodeToString(sum[:])[:12])
}

// pruneDir deletes everything in dir but the files keep names.
func pruneDir(dir string, keep ...string) error {
	es, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range es {
		p := filepath.Join(dir, e.Name())
		if !slices.Contains(keep, p) {
			errs = append(errs, os.RemoveAll(p))
		}
	}
	return errors.Join(errs...)
}

// writeFileAtomic writes path in dir with mode perm so that a crash leaves
// either the old file or the whole new one.
func writeFileAtomic(dir, path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(perm)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if err = errors.Join(err, tmp.Close()); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir) //nolint:gosec // G304: the plugin's own state directory
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// socketMode is the listen socket's mode, whatever the umask: a runner in
// the socket's group connects, and no one else. The group is the process's,
// or the directory's when it is setgid; choosing it is the operator's
// packaging.
const socketMode = 0o660

// listen binds the listen socket at path, mode 0660 whatever the umask. A
// socket already at path is a crashed plugin's, since the listen-socket
// directory's lock proves no live plugin owns it, so it is deleted first;
// anything else there stops startup, naming it, since it is not the
// plugin's to delete. The socket is bound under a temporary name, given its
// mode, then renamed into place, so no client can connect while the umask's
// mode is on it. The returned func closes the listener and deletes the
// socket.
func listen(ctx context.Context, path string) (net.Listener, func(), error) {
	dir, err := openSockDir(filepath.Dir(path))
	if err != nil {
		return nil, nil, err
	}
	name := filepath.Base(path)
	// A crashed plugin may also have left its temporary socket.
	tmp := "." + name + ".new"
	for _, n := range []string{name, tmp} {
		if err := removeLeftoverSocket(dir, n, filepath.Join(filepath.Dir(path), n)); err != nil {
			_ = dir.Close()
			return nil, nil, fmt.Errorf("listen on %s: %w", path, err)
		}
	}
	lis, err := bindSocket(ctx, dir, tmp, name)
	if err != nil {
		_ = dir.Close()
		return nil, nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	// The listener closes through the directory, so the directory closes
	// after it.
	return lis, func() {
		_ = lis.Close()
		_ = unix.Unlinkat(dir.fd, name, 0)
		_ = dir.Close()
	}, nil
}

// bindSocket binds a unix socket at tmp in dir, sets its mode, and renames
// it to name. The listener never unlinks its socket on Close: tmp is gone
// once it is renamed.
func bindSocket(ctx context.Context, dir *sockDir, tmp, name string) (*net.UnixListener, error) {
	l, err := new(net.ListenConfig).Listen(ctx, "unix", dir.path(tmp))
	if err != nil {
		return nil, err
	}
	lis := l.(*net.UnixListener)
	lis.SetUnlinkOnClose(false)
	err = unix.Fchmodat(dir.fd, tmp, socketMode, 0)
	if err == nil {
		err = unix.Renameat(dir.fd, tmp, dir.fd, name)
	}
	if err != nil {
		_ = lis.Close()
		_ = unix.Unlinkat(dir.fd, tmp, 0)
		return nil, err
	}
	return lis, nil
}

// removeLeftoverSocket deletes the socket at name in dir, whose path is
// path, and does nothing when there is no file there. Any other kind of file
// is an error naming it.
func removeLeftoverSocket(dir *sockDir, name, path string) error {
	var st unix.Stat_t
	err := unix.Fstatat(dir.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return fmt.Errorf("a %s is there, not a socket: runner-microvm deletes only a socket a stopped plugin left; "+
			"move %s, or set listen to another path", fileKind(st.Mode), path)
	}
	if err := unix.Unlinkat(dir.fd, name, 0); err != nil {
		return fmt.Errorf("delete the socket a stopped plugin left at %s: %w", path, err)
	}
	return nil
}

// fileKind names the kind of file mode, a stat's st_mode, is.
func fileKind(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return "regular file"
	case unix.S_IFDIR:
		return "directory"
	case unix.S_IFLNK:
		return "symbolic link"
	case unix.S_IFIFO:
		return "FIFO"
	case unix.S_IFCHR:
		return "character device"
	case unix.S_IFBLK:
		return "block device"
	}
	return "file of unknown kind"
}

// keepalivePolicy accepts client pings every 5 s or slower, also with no
// call in flight, so a call that waits silently for minutes, a queued Start
// or a step that prints nothing, is never cut off (PROTO-15). The runner
// pings every 30 s, which grpc-go's default policy (5 minutes) answers with
// GOAWAY too_many_pings, ending every call on the connection.
var keepalivePolicy = keepalive.EnforcementPolicy{
	MinTime:             5 * time.Second,
	PermitWithoutStream: true,
}

// keepaliveParams send no keepalive pings of the server's own: a Time this
// long never passes. The runner's client pings; the plugin only answers.
var keepaliveParams = keepalive.ServerParameters{Time: time.Duration(math.MaxInt64)}

// maxMessage is the largest message the plugin accepts: the runner client's
// own limit, so nothing the runner may send is refused. The agent keeps
// gRPC's 4 MiB default; CopyIn re-chunks toward it (agentChunk).
const maxMessage = 32 << 20

// stopGrace bounds how long serving waits, once a drain has ended, for the
// calls still in flight to send their answers: by then every job is gone, so
// each such call is a Remove that has finished or a call that fails at once.
// A health Watch never ends by itself, and must not hold the plugin up.
const stopGrace = time.Second

// Serve serves srv and the standard health service on lis until ctx ends,
// then stops at once, ending every call, or until srv's drain has ended,
// then stops once the calls in flight have answered, or after stopGrace.
func Serve(ctx context.Context, lis net.Listener, srv *Server) error {
	gs := grpc.NewServer(
		grpc.KeepaliveEnforcementPolicy(keepalivePolicy),
		grpc.KeepaliveParams(keepaliveParams),
		grpc.MaxRecvMsgSize(maxMessage),
	)
	pluginv1alpha.RegisterBackendPluginServer(gs, srv)
	// SERVING until the drain begins, then NOT_SERVING (Server.Drain).
	healthv1.RegisterHealthServer(gs, srv.health)

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			gs.Stop()
		case <-srv.Drained():
			stopGracefully(ctx, gs)
		case <-done:
		}
	}()
	err := gs.Serve(lis)
	close(done)
	<-stopped
	if errors.Is(err, grpc.ErrServerStopped) || ctx.Err() != nil {
		return nil
	}
	return err
}

// stopGracefully stops gs once its calls in flight have answered, or at
// once when ctx ends or after stopGrace.
func stopGracefully(ctx context.Context, gs *grpc.Server) {
	graceful := make(chan struct{})
	go func() { defer close(graceful); gs.GracefulStop() }()
	timer := time.NewTimer(stopGrace)
	defer timer.Stop()
	select {
	case <-graceful:
		return
	case <-ctx.Done():
	case <-timer.C:
	}
	// Stop ends every call, so GracefulStop returns at once.
	gs.Stop()
	<-graceful
}
