// SPDX-License-Identifier: GPL-3.0-or-later

package job

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protodelim"
)

// dieBound is how soon a VM child must be gone once its parent is.
const dieBound = 5 * time.Second

// builtDir holds the job binary the tests build; TestMain deletes it.
var builtDir string

func TestMain(m *testing.M) {
	code := m.Run()
	if builtDir != "" {
		_ = os.RemoveAll(builtDir)
	}
	os.Exit(code)
}

// realJobBinary builds the job binary once and returns its path: these
// tests need the real VM child's side of the liveness pipe, and no test runs
// its own binary.
var realJobBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "job-")
	if err != nil {
		return "", err
	}
	builtDir = dir
	bin := filepath.Join(dir, "runner-microvm-job")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-o", bin,
		"github.com/chrishoage/forgejo-runner-plugin-microvm/cmd/runner-microvm-job")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	cmd.WaitDelay = 10 * time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", errors.New("build the job binary: " + err.Error() + "\n" + string(out))
	}
	return bin, nil
})

// startJobBinary runs the real job binary with args, as startProgram does.
func startJobBinary(t *testing.T, live *os.File, args ...string) (*exec.Cmd, *syncBuf) {
	t.Helper()
	bin, err := realJobBinary()
	if err != nil {
		t.Fatal(err)
	}
	return startProgram(t, nil, live, bin, args...)
}

// startProgram runs name with args, in a process group of its own, with
// stdin, or if nil a pipe nothing writes, so a job binary waits for a spec,
// as its stdin, and live, if not nil, as its fd 3. It returns the command
// and its stderr.
func startProgram(t *testing.T, stdin, live *os.File, name string, args ...string) (*exec.Cmd, *syncBuf) {
	t.Helper()
	if stdin == nil {
		stdinR, stdinW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = stdinR.Close()
			_ = stdinW.Close()
		})
		stdin = stdinR
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	stderr := &syncBuf{}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stderr = stdin, stderr
	if live != nil {
		cmd.ExtraFiles = []*os.File{live}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	return cmd, stderr
}

// waitBound waits up to dieBound for cmd to exit and returns its status.
func waitBound(t *testing.T, cmd *exec.Cmd) syscall.WaitStatus {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); _ = cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(dieBound):
		t.Fatalf("%s still runs %v later", cmd.Path, dieBound)
	}
	ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
	return ws
}

// A VM child exits promptly once the liveness pipe's write end closes, as
// it does when the plugin dies, even before it has read its spec.
func TestVMChildDiesWithTheLivenessPipe(t *testing.T) {
	t.Parallel()
	live, w := livenessPipe(t)
	// Nothing ever writes its stdin, so only its liveness pipe can end it.
	cmd, stderr := startJobBinary(t, live, "vm")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if ws := waitBound(t, cmd); !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("the VM child ended with %v, want SIGKILL", ws)
	}
	if !strings.Contains(stderr.String(), "the plugin is gone") {
		t.Errorf("the VM child's log does not say the plugin is gone:\n%s", stderr.String())
	}
}

// A job process kills its whole process group once the liveness pipe's write
// end closes, not only itself: here a process mkfs left running, which has
// no Pdeathsig of its own to end it when its parent dies.
func TestJobProcessKillsItsGroupWithTheLivenessPipe(t *testing.T) {
	t.Parallel()
	bin, err := realJobBinary()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	spec := testJobSpec(t, bin)
	spec.MkfsExt4 = fakeMkfs(t, "sleep 1000 &\necho $! >"+pidFile+"\nwait")
	specFile := filepath.Join(t.TempDir(), "spec")
	var buf bytes.Buffer
	if _, err := protodelim.MarshalTo(&buf, spec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specFile, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.Open(specFile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	live, w := livenessPipe(t)
	cmd, stderr := startProgram(t, stdin, live, bin)
	var orphan int
	waitFor(t, func() bool {
		b, err := os.ReadFile(pidFile)
		if err != nil || !bytes.HasSuffix(b, []byte("\n")) {
			return false
		}
		orphan, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil
	})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if ws := waitBound(t, cmd); !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("the job process ended with %v, want SIGKILL", ws)
	}
	deadline := time.Now().Add(dieBound)
	for running(orphan) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d of the job process's group outlived it by %v:\n%s", orphan, dieBound, stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// running reports whether pid is a process that has not exited: neither gone
// from /proc nor a zombie.
func running(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The state follows the command, which is in parentheses.
	i := bytes.LastIndexByte(b, ')')
	return i < 0 || i+2 >= len(b) || b[i+2] != 'Z'
}

// A job binary started without the liveness pipe on fd 3 runs nothing:
// neither with fd 3 closed nor with a file there that isn't a pipe.
func TestJobBinaryNeedsTheLivenessPipe(t *testing.T) {
	t.Parallel()
	notAPipe, err := os.Create(filepath.Join(t.TempDir(), "file"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = notAPipe.Close() }()
	bin, err := realJobBinary()
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"vm"}} {
		cmd, stderr := startJobBinary(t, notAPipe, args...)
		if ws := waitBound(t, cmd); ws.ExitStatus() != 1 {
			t.Errorf("%q, a file on fd 3: exit %v, want 1", args, ws)
		}
		if !strings.Contains(stderr.String(), "fd 3 is not the liveness pipe") {
			t.Errorf("%q, a file on fd 3: the log does not say so:\n%s", args, stderr.String())
		}
		// sh closes fd 3, whatever the test process left there, and
		// execs the job binary in its place.
		cmd, stderr = startProgram(t, nil, nil, "sh", append([]string{"-c", `exec 3<&-; exec "$0" "$@"`, bin}, args...)...)
		if ws := waitBound(t, cmd); ws.ExitStatus() != 1 {
			t.Errorf("%q, fd 3 closed: exit %v, want 1", args, ws)
		}
		// Go's runtime may open a file of its own on a free fd 3.
		if !strings.Contains(stderr.String(), "no liveness pipe on fd 3") && !strings.Contains(stderr.String(), "fd 3 is not the liveness pipe") {
			t.Errorf("%q, fd 3 closed: the log does not name the missing pipe:\n%s", args, stderr.String())
		}
	}
}

// A VM child is killed when the job process's thread that forked it exits:
// Pdeathsig, so a VM child never outlives a job process killed outright.
func TestVMChildDiesWithItsThread(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	live, _ := livenessPipe(t)
	th := startThread()
	if th.tid == os.Getpid() {
		t.Fatal("the job process forks on the main thread, which never exits")
	}
	spec := testJobSpec(t, fakeJobBinary(t, "echo $$ >"+pidFile+"; exec sleep 1000"))
	type result struct {
		code int32
		err  error
	}
	done := make(chan result, 1)
	go func() {
		events, err := runOn(t, spec, live, th)
		var code int32 = -1
		if n := len(events); n > 0 {
			if x := events[n-1].GetVmExited(); x != nil {
				code = x.GetExitCode()
			}
		}
		done <- result{code, err}
	}()
	waitFor(t, func() bool {
		b, err := os.ReadFile(pidFile)
		return err == nil && bytes.HasSuffix(b, []byte("\n"))
	})
	th.stop()
	select {
	case r := <-done:
		if r.err != nil || r.code != 128+9 {
			t.Errorf("run = %d, %v; want VMExited with 137", r.code, r.err)
		}
	case <-time.After(dieBound):
		t.Fatalf("the VM child outlived its thread by %v", dieBound)
	}
}

// waitFor polls cond for up to 30 s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// syncBuf is a bytes.Buffer safe for a command's writes and the test's
// reads.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
