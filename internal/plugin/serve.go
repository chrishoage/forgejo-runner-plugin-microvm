// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"

	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/logging"
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
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{
		{dirs.State, 0o700},
		{dirs.Cache, 0o755},
		{dirs.Runtime, 0o755},
		{filepath.Dir(dirs.Listen), 0o755},
	} {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return err
		}
	}
	lis, closeLis, err := listen(ctx, dirs.Listen)
	if err != nil {
		return err
	}
	defer closeLis()
	log.Info("serving", "listen", dirs.Listen, "state", dirs.State, "cache", dirs.Cache, "runtime", dirs.Runtime)
	return Serve(ctx, lis, NewServer(log))
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
