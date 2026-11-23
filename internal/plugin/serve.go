// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
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
  serve   run the plugin until SIGINT or SIGTERM; SIGHUP reloads the profiles
  check   validate the config file, print every error, and exit

The config file is ` + config.DefaultPath + `, or the file ` + config.PathEnv + ` names.
`

// Main runs the subcommand in args, reading the environment through getenv,
// and returns its exit code: serve runs the plugin until SIGINT or SIGTERM;
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

// serveCmd is serve: it runs the plugin until SIGINT or SIGTERM, and
// reloads the config file on each SIGHUP.
func serveCmd(getenv func(string) string) int {
	log := logging.Setup(Name)
	// First, so a SIGHUP during startup can't end the plugin, which is
	// SIGHUP's default action. One queued signal is enough: a reload reads
	// the file as it is then.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	cfg, err := config.Load(getenv)
	if err != nil {
		log.Error("stopped: invalid config", "err", err)
		return 1
	}
	logConfig(log, cfg)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log, cfg, hup, newImageCache); err != nil {
		log.Error("stopped", "err", err)
		return 1
	}
	return 0
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
		"runtime_dir", cfg.RuntimeDir, "max_memory_mib", maxMemory, "cache_max_gib", cfg.CacheMaxGiB)
	logProfiles(log, cfg.Profiles)
}

// newCacheFunc opens the image cache in dir, with a size cap of maxSize
// bytes. run takes it so tests can see the cap it passes.
type newCacheFunc func(log *slog.Logger, dir string, maxSize int64) (ImageCache, error)

// newImageCache is the newCacheFunc serve uses: imagecache.New.
func newImageCache(log *slog.Logger, dir string, maxSize int64) (ImageCache, error) {
	return imagecache.New(log, dir, imagecache.WithMaxSize(maxSize))
}

// run runs the plugin with cfg until ctx ends, reloading the config file on
// each signal from hup. It opens the image cache with newCache.
func run(ctx context.Context, log *slog.Logger, cfg *config.Config, hup <-chan os.Signal, newCache newCacheFunc) error {
	// A missing host tool stops startup before anything is written.
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
	bin := filepath.Join(dirs.State, "bin")
	jobBinary, err := writeJobBinary(ctx, bin)
	if err != nil {
		return err
	}
	agent, err := writeAgent(bin)
	if err != nil {
		return err
	}
	// The locks prove no other plugin uses bin/, so every other file there
	// is a stale hash or a crashed write's temp file.
	if err := pruneDir(bin, jobBinary, agent); err != nil {
		return err
	}
	libDir, err := libDirFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	srv, err := NewServer(log, Config{
		Dirs:      dirs,
		LibDir:    libDir,
		JobBinary: jobBinary,
		MkfsExt4:  mkfs,
		Agent:     agent,
		Profiles:  cfg.Profiles,
		Cache:     cache,
		Launcher:  joblaunch.New(log),
	})
	if err != nil {
		return err
	}
	lis, closeLis, err := listen(ctx, dirs.Listen)
	if err != nil {
		return errors.Join(err, srv.Close(context.WithoutCancel(ctx)))
	}
	defer closeLis()
	log.Info("serving", "listen", dirs.Listen, "state", dirs.State, "cache", dirs.Cache, "runtime", dirs.Runtime,
		"job_binary", jobBinary, "agent", agent, "lib_dir", libDir, "mkfs_ext4", mkfs)
	return serveWithReloads(ctx, log, lis, srv, hup, cfg, (*config.Config).Reload)
}

// serveWithReloads serves srv on lis until ctx ends, reloading the config
// file with read on each signal from hup, then closes srv. It doesn't wait
// for a reload still reading the file: one blocked on a stalled network
// mount would keep the plugin, and its directory locks, alive until SIGKILL.
// That reload finishes on its own and finds srv closed, so it changes
// nothing; it touches nothing else the caller frees.
func serveWithReloads(ctx context.Context, log *slog.Logger, lis net.Listener, srv *Server, hup <-chan os.Signal, cfg *config.Config, read readConfig) error {
	reloadCtx, stopReloads := context.WithCancel(ctx)
	defer stopReloads()
	go reloadOnSignal(reloadCtx, log, hup, srv, cfg, read)
	err := Serve(ctx, lis, srv)
	// Every job's VM ends with the plugin.
	return errors.Join(err, srv.Close(context.WithoutCancel(ctx)))
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

// LibDirEnv names the directory holding libkrun and libkrunfw (VM-10).
//
// SHORTCUT(#11): provisional pick: RUNNER_MICROVM_LIBDIR, else the libraries
// are loaded by soname.
const LibDirEnv = "RUNNER_MICROVM_LIBDIR"

// libDirFromEnv returns RUNNER_MICROVM_LIBDIR as an absolute path, or empty
// when it is unset. The VM child changes its working directory before it
// loads libkrun, so a relative path would name another directory there.
func libDirFromEnv(getenv func(string) string) (string, error) {
	dir := getenv(LibDirEnv)
	if dir == "" {
		return "", nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("%s=%s: %w", LibDirEnv, dir, err)
	}
	return abs, nil
}

// writeJobBinary writes the embedded job binary to
// <dir>/runner-microvm-job-<first 12 hex of its sha256>, always afresh: a
// temp file, fsync, rename, then fsync of dir. It then runs it once with
// `version`, so a directory that can't execute fails startup.
func writeJobBinary(ctx context.Context, dir string) (string, error) {
	bin, err := payload.JobBinary()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := hashedPath(dir, "runner-microvm-job", bin)
	if err := writeFileAtomic(dir, path, bin, 0o700); err != nil {
		return "", fmt.Errorf("write the job binary: %w", err)
	}
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(vctx, path, "version") //nolint:gosec // G204: the job binary the plugin just wrote
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("run the job binary %s in %s, the state directory: %w: %s", path, dir, err, out)
	}
	return path, nil
}

// writeAgent writes the embedded agent to
// <dir>/runner-microvm-agent-<first 12 hex of its sha256>, always afresh, the
// same way as the job binary. Each VM's boot root holds a hard link to it, so
// it is written once per plugin, not once per VM.
func writeAgent(dir string) (string, error) {
	agent, err := payload.Agent()
	if err != nil {
		return "", err
	}
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

// listen binds the listen socket. Its mode comes from the umask.
//
// SHORTCUT(#11): the socket's mode is the umask's, not one the operator
// sets (provisional pick).
//
// SHORTCUT(#11): a socket left by a crashed plugin is not deleted, so
// startup fails until it is.
func listen(ctx context.Context, path string) (net.Listener, func(), error) {
	dir, err := openSockDir(filepath.Dir(path))
	if err != nil {
		return nil, nil, err
	}
	lis, err := new(net.ListenConfig).Listen(ctx, "unix", dir.path(filepath.Base(path)))
	if err != nil {
		_ = dir.Close()
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, nil, fmt.Errorf("listen on %s: a file is already there: another plugin serves on it, "+
				"or a stopped one left its socket, which must be deleted first: %w", path, err)
		}
		return nil, nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	// The listener unlinks its socket on Close through the directory, so
	// the directory closes after it.
	return lis, func() { _ = lis.Close(); _ = dir.Close() }, nil
}

// SHORTCUT(#6): the keepalive policy is provisional: client pings every 5 s
// are accepted, also with no active stream, so a call that waits silently
// for minutes is never cut off (PROTO-15).
var keepalivePolicy = keepalive.EnforcementPolicy{
	MinTime:             5 * time.Second,
	PermitWithoutStream: true,
}

// Serve serves srv and the standard health service on lis until ctx ends,
// then stops at once, ending every call.
func Serve(ctx context.Context, lis net.Listener, srv *Server) error {
	gs := grpc.NewServer(grpc.KeepaliveEnforcementPolicy(keepalivePolicy))
	pluginv1alpha.RegisterBackendPluginServer(gs, srv)
	// The health service always reports SERVING.
	healthv1.RegisterHealthServer(gs, health.NewServer())

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			gs.Stop()
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
