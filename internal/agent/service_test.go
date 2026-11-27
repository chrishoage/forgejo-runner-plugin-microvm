// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/testbin"
)

// serviceBound is how long a service test waits for a condition: a command
// starting, a file appearing, run returning.
const serviceBound = 30 * time.Second

// syncBuffer is a log's buffer that run's goroutine writes while the test
// reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// serviceRun is one run of the agent, with a fake guest, in a test root.
type serviceRun struct {
	root    string
	log     syncBuffer
	console *os.File
	// sock is the unix socket the fake listen listens on.
	sock string
	// listenErr, when set, fails the fake listen.
	listenErr error
	// lis is the fake listen's listener, once it has listened; closing
	// it fails the agent's listener.
	lis chan net.Listener
	// stopGrace, when positive, replaces the guest's stopGrace.
	stopGrace time.Duration
	conn      *grpc.ClientConn
	code      chan int
}

// newServiceRun returns a run of the agent in a test root whose console is
// a file of the test's, and a client for it.
func newServiceRun(t *testing.T) *serviceRun {
	t.Helper()
	console, err := os.Create(filepath.Join(t.TempDir(), "console"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = console.Close() })
	// A unix socket's path holds at most 107 bytes (startServer).
	dir, err := os.MkdirTemp("", "svc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")
	conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &serviceRun{
		root: testRoot(t), console: console, sock: sock, conn: conn,
		lis: make(chan net.Listener, 1), code: make(chan int, 1),
	}
}

// bootConfig returns a boot config whose image runs as testUser in a
// working directory the image lacks, with the test's own PATH, and whose
// service runs argv.
func (r *serviceRun) bootConfig(argv []string) *agentv1.BootConfig {
	cfg := testBootConfig()
	cfg.Image = &agentv1.Image{
		Env:  []string{"PATH=" + os.Getenv("PATH"), "IMAGE_ONLY=from-image", "OVERRIDDEN=from-image"},
		User: testUser, WorkingDir: filepath.Join(r.root, "srv", "app"),
	}
	cfg.Service = &agentv1.Service{Argv: argv}
	return cfg
}

// start runs the agent with cfg in a goroutine of its own; r.code gets
// what it returns. A run still going when the test ends has its listener
// failed, and is waited for.
func (r *serviceRun) start(t *testing.T, cfg *agentv1.BootConfig) {
	t.Helper()
	g := guest{
		prepare: func(*agentv1.BootConfig) ([]rwDisk, error) { return nil, nil },
		root:    r.root,
		listen: func(uint32) (net.Listener, error) {
			if r.listenErr != nil {
				return nil, r.listenErr
			}
			lis, err := new(net.ListenConfig).Listen(context.Background(), "unix", r.sock)
			if err == nil {
				r.lis <- lis
			}
			return lis, err
		},
		stdout: r.console, stderr: r.console,
		stopGrace: cmp.Or(r.stopGrace, stopGrace),
	}
	log := slog.New(slog.NewTextHandler(&r.log, nil))
	go func() { r.code <- run(log, cfg, g) }()
	t.Cleanup(func() {
		select {
		case lis := <-r.lis:
			_ = lis.Close()
		default:
		}
		select {
		case <-r.code:
		case <-time.After(serviceBound):
			t.Errorf("the agent still runs %v after its listener failed", serviceBound)
		}
	})
}

// wait returns what run returned, failing t unless it returns within
// serviceBound.
func (r *serviceRun) wait(t *testing.T) int {
	t.Helper()
	select {
	case code := <-r.code:
		// The cleanup in start waits for it too.
		r.code <- code
		return code
	case <-time.After(serviceBound):
		t.Fatalf("the agent still runs after %v; its log:\n%s", serviceBound, r.log.String())
		return 0
	}
}

// returned reports whether run has returned.
func (r *serviceRun) returned() bool {
	select {
	case code := <-r.code:
		r.code <- code
		return true
	default:
		return false
	}
}

// client returns a client for the agent.
func (r *serviceRun) client() agentv1.AgentServiceClient {
	return agentv1.NewAgentServiceClient(r.conn)
}

// ping pings the agent, waiting for it to listen.
func (r *serviceRun) ping(t *testing.T) {
	t.Helper()
	if _, err := r.client().Ping(testContext(t), &agentv1.PingRequest{}, grpc.WaitForReady(true)); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

// startService calls StartService with env, waiting for the agent to
// listen, and returns the call's error.
func (r *serviceRun) startService(t *testing.T, env map[string]string) error {
	t.Helper()
	_, err := r.client().StartService(testContext(t), &agentv1.StartServiceRequest{Env: env}, grpc.WaitForReady(true))
	return err
}

// mustStartService calls StartService with env, failing t unless it
// succeeds.
func (r *serviceRun) mustStartService(t *testing.T, env map[string]string) {
	t.Helper()
	if err := r.startService(t, env); err != nil {
		t.Fatalf("StartService: %v; the agent's log:\n%s", err, r.log.String())
	}
}

// failListener fails the agent's listener.
func (r *serviceRun) failListener(t *testing.T) {
	t.Helper()
	select {
	case lis := <-r.lis:
		_ = lis.Close()
	case <-time.After(serviceBound):
		t.Fatalf("the agent doesn't listen within %v", serviceBound)
	}
}

// waitForFile returns the contents of path once it holds a whole line,
// failing t unless it does within serviceBound.
func waitForFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(serviceBound)
	for {
		if b, err := os.ReadFile(path); err == nil && bytes.HasSuffix(b, []byte("\n")) {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is not written within %v", path, serviceBound)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForPid returns the pid the service wrote to path, once that process
// runs name, failing t unless it does within serviceBound.
func waitForPid(t *testing.T, path, name string) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(waitForFile(t, path)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(serviceBound)
	for {
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil && strings.TrimSpace(string(b)) == name {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d doesn't run %s within %v", pid, name, serviceBound)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The agent listens and answers Ping without starting the boot config's
// service; StartService starts it: as the image's user, in the image's
// working directory, which it creates, with the call's environment laid
// over the image's, its output on the console. Once the command exits, the
// agent logs its status and returns its exit code, and no value of the
// call's environment is in its log.
func TestServiceStartsOnlyWhenStartServiceAsks(t *testing.T) {
	t.Parallel()
	r := newServiceRun(t)
	marker := filepath.Join(t.TempDir(), "started")
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`echo "service says hi"; echo "service warns" >&2
printf '%%s %%s %%s %%s %%s\n' "$(id -u)" "$(pwd)" "$IMAGE_ONLY" "$OVERRIDDEN" "$SERVICE_ONLY" > %s
read -r line < %s; exit 7`, marker, fifo)
	cfg := r.bootConfig([]string{"sh", "-c", script})
	r.start(t, cfg)
	r.ping(t)

	if log := r.log.String(); strings.Contains(log, "service started") {
		t.Fatalf("the service started before StartService:\n%s", log)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the service ran before StartService: %v", err)
	}
	r.mustStartService(t, map[string]string{"OVERRIDDEN": "from-service", "SERVICE_ONLY": "s3cret"})
	if log := r.log.String(); !strings.Contains(log, `msg="service started"`) || !strings.Contains(log, "argv0=sh") {
		t.Errorf("the log doesn't say the service started:\n%s", log)
	}

	want := fmt.Sprintf("%d %s from-image from-service s3cret\n", os.Getuid(), cfg.GetImage().GetWorkingDir())
	if got := waitForFile(t, marker); got != want {
		t.Errorf("the service wrote %q, want %q", got, want)
	}
	if r.returned() {
		t.Fatal("the agent returned while its service runs")
	}
	if err := os.WriteFile(fifo, []byte("stop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := r.wait(t); code != 7 {
		t.Errorf("the agent returned %d, want the service's 7", code)
	}
	log := r.log.String()
	if !strings.Contains(log, `msg="service exited"`) || !strings.Contains(log, "exit_code=7") {
		t.Errorf("the log doesn't hold the service's exit status:\n%s", log)
	}
	if strings.Contains(log, "s3cret") {
		t.Errorf("the log holds a value of the service's environment:\n%s", log)
	}
	console, err := os.ReadFile(r.console.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(console), "service says hi\n") || !strings.Contains(string(console), "service warns\n") {
		t.Errorf("the console holds %q, want the service's stdout and stderr", console)
	}
}

// The service's process holds only its stdin and the console as its stdout
// and stderr: not the agent's listener nor any other descriptor of the
// agent's, nor one the agent inherited without close-on-exec. A service
// ended by a signal makes the agent return 128 plus the signal's number,
// logged.
func TestServiceHoldsOnlyItsConsole(t *testing.T) {
	t.Parallel()
	// A descriptor the agent inherits, as a parent that opened it without
	// close-on-exec hands it down.
	inherited, err := syscall.Open(os.DevNull, syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(inherited) })
	r := newServiceRun(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	r.start(t, r.bootConfig([]string{"sh", "-c", fmt.Sprintf(`echo $$ > %s; exec sleep 600`, pidFile)}))
	// The call's connection is open as the service starts: a descriptor
	// of the agent's it could inherit.
	r.mustStartService(t, nil)
	pid := waitForPid(t, pidFile, "sleep")

	// sleep's dynamic loader holds each library it loads open for a
	// moment after the exec: what the service inherited stays.
	want := []string{"0", "1", "2"}
	var fds []string
	for deadline := time.Now().Add(serviceBound); ; time.Sleep(5 * time.Millisecond) {
		entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		if err != nil {
			t.Fatal(err)
		}
		fds = fds[:0]
		for _, e := range entries {
			fds = append(fds, e.Name())
		}
		if slices.Equal(fds, want) || time.Now().After(deadline) {
			break
		}
	}
	if !slices.Equal(fds, want) {
		t.Errorf("the service holds descriptors %v, want only 0, 1 and 2", fds)
	}
	for _, fd := range []string{"1", "2"} {
		if target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd)); err != nil || target != r.console.Name() {
			t.Errorf("the service's descriptor %s is %q, %v; want the console %s", fd, target, err, r.console.Name())
		}
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := r.wait(t); code != 128+int(syscall.SIGTERM) {
		t.Errorf("the agent returned %d, want %d", code, 128+int(syscall.SIGTERM))
	}
	if log := r.log.String(); !strings.Contains(log, `msg="service exited"`) || !strings.Contains(log, "signal=terminated") {
		t.Errorf("the log doesn't hold the service's signal:\n%s", log)
	}
}

// A service command that can't start fails StartService with
// FailedPrecondition, naming argv[0] and why, never its PATH or a value of
// the call's environment; then the agent stops serving and returns 1.
func TestServiceThatCantStartFailsStartService(t *testing.T) {
	t.Parallel()
	notExec := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file the lookup takes, which can't start, in a directory of the
	// service's PATH.
	wrongFormat := filepath.Join(t.TempDir(), "s3cret-dir")
	if err := os.Mkdir(wrongFormat, 0o755); err != nil {
		t.Fatal(err)
	}
	testbin.Write(t, filepath.Join(wrongFormat, "wrong-format"), "not a program\n")
	for name, tc := range map[string]struct {
		argv []string
		env  map[string]string
		user string
		want string
	}{
		"not found":      {argv: []string{"no-such-command"}, env: map[string]string{"PATH": "/s3cret/bin"}, want: "service no-such-command: not found in its PATH"},
		"not executable": {argv: []string{notExec}, env: map[string]string{"TOKEN": "s3cret"}, want: "service " + notExec + ": "},
		"wrong format":   {argv: []string{"wrong-format"}, env: map[string]string{"PATH": wrongFormat}, want: "service wrong-format: exec format error"},
		"unknown user":   {argv: []string{"true"}, env: map[string]string{"TOKEN": "s3cret"}, user: "nobody-here", want: "service true: user nobody-here"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newServiceRun(t)
			cfg := r.bootConfig(tc.argv)
			if tc.user != "" {
				cfg.Image.User = tc.user
			}
			r.start(t, cfg)
			err := r.startService(t, tc.env)
			if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), tc.want) ||
				strings.Contains(err.Error(), "s3cret") {
				t.Errorf("StartService: %v; want FailedPrecondition with %q, and not the service's PATH or env", err, tc.want)
			}
			if code := r.wait(t); code != 1 {
				t.Errorf("the agent returned %d, want 1", code)
			}
			if log := r.log.String(); !strings.Contains(log, tc.want) || strings.Contains(log, "s3cret") {
				t.Errorf("the log doesn't hold %q, or holds the service's PATH or env:\n%s", tc.want, log)
			}
			// The agent stopped serving before it returned.
			if _, err := r.client().Ping(testContext(t), &agentv1.PingRequest{}); status.Code(err) != codes.Unavailable {
				t.Errorf("Ping after the agent returned: %v, want Unavailable", err)
			}
		})
	}
}

// Once its service can't start, the agent waits at most its stop grace for
// the calls in flight before it stops serving: an Exec that runs on is
// cancelled, and its command killed.
func TestServiceThatCantStartBoundsTheStop(t *testing.T) {
	t.Parallel()
	r := newServiceRun(t)
	r.stopGrace = 100 * time.Millisecond
	r.start(t, r.bootConfig([]string{"no-such-command"}))
	pidFile := filepath.Join(t.TempDir(), "pid")
	exec, err := r.client().Exec(testContext(t), &agentv1.ExecRequest{
		Argv: []string{"sh", "-c", fmt.Sprintf(`echo $$ > %s; exec sleep 600`, pidFile)},
	}, grpc.WaitForReady(true))
	if err != nil {
		t.Fatal(err)
	}
	pid := waitForPid(t, pidFile, "sleep")
	if err := r.startService(t, nil); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("StartService: %v, want FailedPrecondition", err)
	}
	if code := r.wait(t); code != 1 {
		t.Errorf("the agent returned %d, want 1", code)
	}
	for {
		if _, err := exec.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				t.Error("the Exec ended normally, want it cancelled")
			}
			break
		}
	}
	deadline := time.Now().Add(serviceBound)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("the Exec's command %d still runs %v after the agent returned", pid, serviceBound)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A second StartService fails with FailedPrecondition, and the service
// started by the first runs on.
func TestSecondStartServiceFails(t *testing.T) {
	t.Parallel()
	r := newServiceRun(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	r.start(t, r.bootConfig([]string{"sh", "-c", fmt.Sprintf(`echo $$ > %s; exec sleep 600`, pidFile)}))
	r.mustStartService(t, nil)
	if err := r.startService(t, nil); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("the second StartService: %v, want FailedPrecondition", err)
	}
	pid := waitForPid(t, pidFile, "sleep")
	if r.returned() {
		t.Fatal("the agent returned after a second StartService")
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if code := r.wait(t); code != 128+int(syscall.SIGKILL) {
		t.Errorf("the agent returned %d, want %d", code, 128+int(syscall.SIGKILL))
	}
}

// StartService on a VM whose boot config has no service fails with
// FailedPrecondition, and the agent serves on.
func TestStartServiceWithoutAServiceFails(t *testing.T) {
	t.Parallel()
	r := newServiceRun(t)
	cfg := r.bootConfig(nil)
	cfg.Service = nil
	r.start(t, cfg)
	if err := r.startService(t, nil); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("StartService: %v, want FailedPrecondition", err)
	}
	r.ping(t)
	if r.returned() {
		t.Fatal("the agent returned after StartService failed")
	}
}

// When the agent can't listen, it returns 1 without starting its service.
func TestServiceDoesntStartWhenTheAgentCantListen(t *testing.T) {
	t.Parallel()
	r := newServiceRun(t)
	r.listenErr = errors.New("no vsock")
	marker := filepath.Join(t.TempDir(), "started")
	r.start(t, r.bootConfig([]string{"sh", "-c", "echo > " + marker}))
	if code := r.wait(t); code != 1 {
		t.Errorf("the agent returned %d, want 1", code)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the service ran: %v", err)
	}
}

// When the agent's listener fails once its service runs, it kills the
// service's process group and reaps the service before it returns.
func TestServiceIsKilledWhenTheListenerFails(t *testing.T) {
	t.Parallel()
	r := newServiceRun(t)
	pids := filepath.Join(t.TempDir(), "pids")
	// The service and a child of its own, in its process group.
	r.start(t, r.bootConfig([]string{"sh", "-c", fmt.Sprintf(`sleep 600 & echo "$$ $!" > %s; wait`, pids)}))
	r.mustStartService(t, nil)
	var service, child int
	if _, err := fmt.Sscan(waitForFile(t, pids), &service, &child); err != nil {
		t.Fatal(err)
	}
	r.failListener(t)
	if code := r.wait(t); code != 1 {
		t.Errorf("the agent returned %d, want 1", code)
	}
	// The service is the agent's child, reaped by run; the sleep is not,
	// so it may linger as a zombie of init's until the test ends.
	if err := syscall.Kill(service, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the service %d is not reaped: %v", service, err)
	}
	deadline := time.Now().Add(serviceBound)
	for alive(child) {
		if time.Now().After(deadline) {
			t.Fatalf("the service's child %d still runs %v after the agent returned", child, serviceBound)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
