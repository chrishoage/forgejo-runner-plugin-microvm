// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// result is an Exec's whole output.
type result struct {
	stdout, stderr string
	code           int32
	startError     string
	ended          bool
}

func runExec(ctx context.Context, t *testing.T, c agentv1.AgentServiceClient, req *agentv1.ExecRequest) (result, error) {
	t.Helper()
	stream, err := c.Exec(ctx, req)
	if err != nil {
		return result{}, err
	}
	var r result
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return r, nil
		}
		if err != nil {
			return r, err
		}
		if r.ended {
			t.Errorf("a response after the end: %v", resp)
		}
		switch ev := resp.GetEvent().(type) {
		case *agentv1.ExecResponse_Stdout:
			r.stdout += string(ev.Stdout)
		case *agentv1.ExecResponse_Stderr:
			r.stderr += string(ev.Stderr)
		case *agentv1.ExecResponse_ExitCode:
			r.code, r.ended = ev.ExitCode, true
		case *agentv1.ExecResponse_StartError:
			r.startError, r.ended = ev.StartError, true
		}
	}
}

func sh(script string) *agentv1.ExecRequest {
	return &agentv1.ExecRequest{Argv: []string{"sh", "-c", script}, Env: map[string]string{"PATH": os.Getenv("PATH")}}
}

func TestExec(t *testing.T) {
	t.Parallel()
	c := startAgent(t)
	dir := filepath.Join(t.TempDir(), "work", "dir")
	req := sh(`echo out; echo err >&2; pwd; echo "$SECRET"; exit 3`)
	req.Env["SECRET"] = "s3cret"
	req.WorkingDir = dir
	r, err := runExec(testContext(t), t, c, req)
	if err != nil {
		t.Fatal(err)
	}
	if r.stdout != "out\n"+dir+"\ns3cret\n" || r.stderr != "err\n" || r.code != 3 || !r.ended {
		t.Errorf("Exec = %+v", r)
	}
}

func TestExecSignal(t *testing.T) {
	t.Parallel()
	r, err := runExec(testContext(t), t, startAgent(t), sh(`kill -9 $$`))
	if err != nil {
		t.Fatal(err)
	}
	if r.code != 137 {
		t.Errorf("exit code = %d, want 137", r.code)
	}
}

func TestExecStartErrors(t *testing.T) {
	t.Parallel()
	c := startAgent(t)
	for name, req := range map[string]*agentv1.ExecRequest{
		"no command":   {},
		"not on PATH":  {Argv: []string{"no-such-command-anywhere"}, Env: map[string]string{"PATH": t.TempDir()}},
		"missing path": {Argv: []string{"/no/such/binary"}},
		"user":         {Argv: []string{"true"}, User: "nobody"},
	} {
		t.Run(name, func(t *testing.T) {
			r, err := runExec(testContext(t), t, c, req)
			if err != nil {
				t.Fatal(err)
			}
			if r.startError == "" || r.code != 0 {
				t.Errorf("Exec = %+v, want a start error", r)
			}
		})
	}
}

// The command is found in its own environment's PATH, not the agent's.
func TestExecLooksUpTheRequestsPath(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "hello"), []byte("#!/bin/sh\necho hi from $0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := runExec(testContext(t), t, startAgent(t), &agentv1.ExecRequest{
		Argv: []string{"hello"}, Env: map[string]string{"PATH": bin + ":" + os.Getenv("PATH")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.stdout != "hi from "+filepath.Join(bin, "hello")+"\n" {
		t.Errorf("stdout = %q", r.stdout)
	}
}

// A background child holding the output open doesn't keep Exec running
// (GST-9).
func TestExecReturnsDespiteABackgroundChild(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	begin := time.Now()
	r, err := runExec(testContext(t), t, startAgent(t), sh(`sleep 30 & echo $! >`+pidFile+`; echo done`))
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(begin); d > 10*time.Second {
		t.Errorf("Exec took %v", d)
	}
	if !strings.HasPrefix(r.stdout, "done") || r.code != 0 {
		t.Errorf("Exec = %+v", r)
	}
	if b, err := os.ReadFile(pidFile); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// slowExecStream is an Exec stream whose first stdout Send blocks until Exec
// has handled the command's exit, and whose every Send, but the exit code's,
// then takes longer than the output grace: a runner, or a network, that
// drains slowly. Just before the first blocks, it calls firstSend.
type slowExecStream struct {
	grpc.ServerStream
	ctx       context.Context
	exited    <-chan struct{}
	firstSend func() error
	slowed    bool
	out       []*agentv1.ExecResponse
}

func (s *slowExecStream) Context() context.Context { return s.ctx }

func (s *slowExecStream) Send(resp *agentv1.ExecResponse) error {
	if !s.slowed {
		if resp.GetStdout() == nil {
			return errors.New("the first Send is not stdout")
		}
		s.slowed = true
		if err := s.firstSend(); err != nil {
			return err
		}
		select {
		case <-s.exited:
		case <-s.ctx.Done():
			return errors.New("Exec did not handle the command's exit")
		}
	}
	if _, last := resp.GetEvent().(*agentv1.ExecResponse_ExitCode); !last {
		select {
		case <-time.After(2 * outputGrace):
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}
	s.out = append(s.out, resp)
	return nil
}

// pipeSize sets the size of the pipe r reads to size bytes, as small as the
// kernel gives the pipes of a user over its soft limit
// (fs.pipe-user-pages-soft), and returns the size the pipe has.
func pipeSize(r *os.File, size int) (int, error) {
	rc, err := r.SyscallConn()
	if err != nil {
		return 0, err
	}
	var got uintptr
	var errno syscall.Errno
	err = rc.Control(func(fd uintptr) {
		if _, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_SETPIPE_SZ, uintptr(size)); errno != 0 {
			return
		}
		got, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETPIPE_SZ, 0)
	})
	if err == nil && errno != 0 {
		err = errno
	}
	return int(got), err
}

// Everything the command wrote before it exited is sent, however slowly
// the stream drains: the output grace only bounds the wait for a background
// child holding the output open.
//
// The pipe is shrunk to one page, whatever the host's limits give a new
// pipe, so the test doesn't depend on them: a command that writes more than
// the pipe holds blocks, and never exits, while the stream is stuck in its
// first Send. The command writes half a page, which relay reads, a quarter
// page at a time, and starts to send. Once that Send has begun, the command
// writes the other half and exits, while relay is still stuck in the Send.
// So the second half is in the pipe when the command exits: only relay's
// drain path, which reads on after the exit, sends it. It takes at least
// two drain reads, with a Send between them that outlasts the output grace,
// so the grace must not start before the last of the bytes owed is read.
func TestExecSendsAllOutputToASlowStream(t *testing.T) {
	t.Parallel()
	exited := make(chan struct{})
	started := filepath.Join(t.TempDir(), "started")
	size := os.Getpagesize()
	half := size / 2
	stream := &slowExecStream{
		ctx:       testContext(t),
		exited:    exited,
		firstSend: func() error { return os.WriteFile(started, nil, 0o600) },
	}
	// The first half may take more than one read; the second half is
	// written only after the first Send began.
	req := sh(`head -c ` + strconv.Itoa(half) + ` /dev/zero
until [ -e "$STARTED" ]; do sleep 0.01; done
head -c ` + strconv.Itoa(half) + ` /dev/zero
exit 4`)
	req.Env["STARTED"] = started
	srv := &server{
		execExited:   func() { close(exited) },
		execReadSize: half / 2,
		execStdout: func(r *os.File) {
			if got, err := pipeSize(r, size); err != nil || got != size {
				t.Errorf("pipe size = %d, %v, want %d", got, err, size)
			}
		},
	}
	if err := srv.Exec(req, stream); err != nil {
		t.Fatal(err)
	}
	var stdout, stdoutSends int
	var code int32 = -1
	for _, resp := range stream.out {
		stdout += len(resp.GetStdout())
		if len(resp.GetStdout()) > 0 {
			stdoutSends++
		}
		if ev, ok := resp.GetEvent().(*agentv1.ExecResponse_ExitCode); ok {
			code = ev.ExitCode
		}
	}
	if stdout != size || code != 4 {
		t.Errorf("sent %d bytes of stdout and exit code %d, want %d and 4", stdout, code, size)
	}
	if stdoutSends < 2 {
		t.Errorf("sent the stdout in %d messages, want the second half apart from the first", stdoutSends)
	}
}

// Large output arrives whole, through the agent's gRPC server.
func TestExecLargeOutput(t *testing.T) {
	t.Parallel()
	const size = 8 << 20
	r, err := runExec(testContext(t), t, startAgent(t), sh(`head -c `+strconv.Itoa(size)+` /dev/zero | tr '\0' x; head -c 1000 /dev/zero >&2`))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.stdout) != size || len(r.stderr) != 1000 || r.code != 0 {
		t.Errorf("got %d bytes of stdout, %d of stderr, exit code %d", len(r.stdout), len(r.stderr), r.code)
	}
}

// Cancelling Exec kills the command's whole process group at once (GST-8):
// the command and a background child it started in its group are both gone
// within 5 s, and the call has returned.
func TestExecCancelKillsTheGroup(t *testing.T) {
	t.Parallel()
	c := startAgent(t)
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	stream, err := c.Exec(ctx, sh(`sleep 1000 & echo $$ $!; wait`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, f := range strings.Fields(string(resp.GetStdout())) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("pids %q: %v", resp.GetStdout(), err)
		}
		pids = append(pids, pid)
	}
	if len(pids) != 2 {
		t.Fatalf("pids %q: want the command's and its child's", resp.GetStdout())
	}
	t.Cleanup(func() {
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	cancel()
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
		t.Errorf("the cancelled Exec's stream ended with %v, want Canceled", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for _, pid := range pids {
		for alive(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("process %d of the command's group outlived the cancelled Exec by 5 s", pid)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// alive says whether pid is a process that has not exited: a zombie, which
// has exited but is not yet reaped, is not alive.
func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The state follows the command's name, in parentheses.
	i := bytes.LastIndexByte(b, ')')
	return i < 0 || i+2 >= len(b) || b[i+2] != 'Z'
}

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func tarOf(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if e.typ == 0 {
			e.typ = tar.TypeReg
		}
		if e.mode == 0 {
			e.mode = 0o644
		}
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// copyIn sends archive to dest in chunks of 7 bytes.
func copyIn(t *testing.T, c agentv1.AgentServiceClient, dest string, archive []byte) error {
	t.Helper()
	stream, err := c.CopyIn(testContext(t))
	if err != nil {
		return err
	}
	if err := stream.Send(&agentv1.CopyInRequest{Destination: dest}); err != nil {
		return err
	}
	for len(archive) > 0 {
		n := min(7, len(archive))
		if err := stream.Send(&agentv1.CopyInRequest{Chunk: archive[:n]}); err != nil {
			break // the error comes with CloseAndRecv
		}
		archive = archive[n:]
	}
	_, err = stream.CloseAndRecv()
	return err
}

func TestCopyIn(t *testing.T) {
	t.Parallel()
	c := startAgent(t)
	dest := filepath.Join(t.TempDir(), "var", "run", "act")
	err := copyIn(t, c, dest, tarOf(t,
		entry{name: "workflow/event.json", body: "{}", mode: 0o644},
		entry{name: "workflow/envs.txt", mode: 0o666},
		entry{name: "workflow/link", typ: tar.TypeSymlink, link: "event.json"},
	))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "workflow/link")); err != nil || string(b) != "{}" {
		t.Errorf("event.json through its link = %q, %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(dest, "workflow/envs.txt")); err != nil || fi.Mode().Perm() != 0o666 {
		t.Errorf("envs.txt: %v, %v", fi, err)
	}
	// An empty archive still creates the destination.
	empty := filepath.Join(t.TempDir(), "a", "b")
	if err := copyIn(t, c, empty, tarOf(t)); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(empty); err != nil || !fi.IsDir() {
		t.Errorf("destination of an empty archive: %v", err)
	}
}

func TestCopyInRefuses(t *testing.T) {
	t.Parallel()
	c := startAgent(t)
	for name, tt := range map[string]struct {
		dest    string
		entries []entry
	}{
		"relative destination":   {dest: "relative"},
		"directory by file":      {entries: []entry{{name: "d", typ: tar.TypeDir}, {name: "d", body: "x"}}},
		"through a symlink out":  {entries: []entry{{name: "up", typ: tar.TypeSymlink, link: "../.."}, {name: "up/escaped", body: "x"}}},
		"device node":            {entries: []entry{{name: "null", typ: tar.TypeChar}}},
		"hard link to a missing": {entries: []entry{{name: "l", typ: tar.TypeLink, link: "../../outside"}}},
	} {
		t.Run(name, func(t *testing.T) {
			top := t.TempDir()
			dest := tt.dest
			if dest == "" {
				dest = filepath.Join(top, "a", "dest")
			}
			err := copyIn(t, c, dest, tarOf(t, tt.entries...))
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("CopyIn = %v, want InvalidArgument", err)
			}
			if _, err := os.Lstat(filepath.Join(top, "escaped")); !os.IsNotExist(err) {
				t.Error("an entry landed outside the destination")
			}
		})
	}
}

// A dot-dot name stays inside the destination.
func TestCopyInDotDot(t *testing.T) {
	t.Parallel()
	top := t.TempDir()
	dest := filepath.Join(top, "a", "dest")
	if err := copyIn(t, startAgent(t), dest, tarOf(t, entry{name: "../../escaped", body: "x"})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(top, "escaped")); !os.IsNotExist(err) {
		t.Error("a dot-dot entry landed outside the destination")
	}
	if _, err := os.Stat(filepath.Join(dest, "escaped")); err != nil {
		t.Errorf("not inside either: %v", err)
	}
}

func copyOut(t *testing.T, c agentv1.AgentServiceClient, path string) ([]byte, error) {
	t.Helper()
	stream, err := c.CopyOut(testContext(t), &agentv1.CopyOutRequest{Path: path})
	if err != nil {
		return nil, err
	}
	var b []byte
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return b, nil
		}
		if err != nil {
			return nil, err
		}
		b = append(b, resp.GetChunk()...)
	}
}

func tarEntries(t *testing.T, b []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		out[hdr.Name] = string(body) + hdr.Linkname
	}
}

func TestCopyOut(t *testing.T) {
	t.Parallel()
	c := startAgent(t)
	dir := filepath.Join(t.TempDir(), "workflow")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "envs.txt"), []byte("A=b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub/envs.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	b, err := copyOut(t, c, filepath.Join(dir, "sub", "envs.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tarEntries(t, b); len(got) != 1 || got["envs.txt"] != "A=b\n" {
		t.Errorf("a file: %v", got)
	}
	b, err = copyOut(t, c, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"workflow/": "", "workflow/sub/": "", "workflow/sub/envs.txt": "A=b\n", "workflow/link": "sub/envs.txt"}
	got := tarEntries(t, b)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("a directory: %v, want %v", got, want)
			break
		}
	}
	// A missing path is an empty tar: two zero blocks.
	b, err = copyOut(t, c, filepath.Join(dir, "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, make([]byte, 1024)) {
		t.Errorf("a missing path: %d bytes, want 1024 zeros", len(b))
	}
}
