// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"

	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/imagecache"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/joblaunch"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/logging"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/payload"
)

// Main runs the plugin until SIGINT or SIGTERM, and returns its exit code.
func Main(args []string) int {
	log := logging.Setup(Name)
	if len(args) > 0 {
		log.Error("runner-microvm takes no arguments yet", "args", args)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log); err != nil {
		log.Error("stopped", "err", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, log *slog.Logger) error {
	dirs, err := DirsFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	// The locks come before anything else writes to the directories, and
	// are held until the plugin exits.
	locks, err := lockDirs(dirs)
	if err != nil {
		return err
	}
	defer func() { _ = locks.Close() }()
	// SHORTCUT(#11): no cleanup at startup of what a crashed plugin left.
	if err := joblaunch.SetSubreaper(); err != nil {
		return err
	}
	jobBinary, err := writeJobBinary(ctx, filepath.Join(dirs.State, "bin"))
	if err != nil {
		return err
	}
	agent, err := payload.Agent()
	if err != nil {
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
		Agent:     agent,
		Cache:     imagecache.New(log),
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
		"job_binary", jobBinary, "lib_dir", libDir)
	err = Serve(ctx, lis, srv)
	// Every job's VM ends with the plugin.
	return errors.Join(err, srv.Close(context.WithoutCancel(ctx)))
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
	sum := sha256.Sum256(bin)
	path := filepath.Join(dir, "runner-microvm-job-"+hex.EncodeToString(sum[:])[:12])
	if err := writeFileAtomic(dir, path, bin); err != nil {
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

// writeFileAtomic writes path in dir so that a crash leaves either the old
// file or the whole new one.
func writeFileAtomic(dir, path string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(0o700)
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
