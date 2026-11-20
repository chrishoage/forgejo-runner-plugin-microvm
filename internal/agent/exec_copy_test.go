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

// slowExecStream is an Exec stream whose first Send blocks until Exec has
// handled the command's exit: a runner, or a network, that drains slowly.
type slowExecStream struct {
	grpc.ServerStream
	ctx    context.Context
	exited <-chan struct{}
	slowed bool
	out    []*agentv1.ExecResponse
}

func (s *slowExecStream) Context() context.Context { return s.ctx }

func (s *slowExecStream) Send(resp *agentv1.ExecResponse) error {
	if !s.slowed {
		s.slowed = true
		select {
		case <-s.exited:
		case <-s.ctx.Done():
			return errors.New("Exec did not handle the command's exit")
		}
	}
	s.out = append(s.out, resp)
	return nil
}

// Everything the command wrote before it exited is sent, however slowly
// the stream drains: the output grace only bounds the wait for a background
// child holding the output open.
func TestExecSendsAllOutputToASlowStream(t *testing.T) {
	t.Parallel()
	exited := make(chan struct{})
	// More than one read takes, less than the pipe holds, so the command
	// exits while most of it is still in the pipe.
	const size = 60000
	stream := &slowExecStream{ctx: testContext(t), exited: exited}
	req := sh(`head -c ` + strconv.Itoa(size) + ` /dev/zero | tr '\0' x; exit 4`)
	if err := (&server{execExited: func() { close(exited) }}).Exec(req, stream); err != nil {
		t.Fatal(err)
	}
	var stdout int
	var code int32 = -1
	for _, resp := range stream.out {
		stdout += len(resp.GetStdout())
		if ev, ok := resp.GetEvent().(*agentv1.ExecResponse_ExitCode); ok {
			code = ev.ExitCode
		}
	}
	if stdout != size || code != 4 {
		t.Errorf("sent %d bytes of stdout and exit code %d, want %d and 4", stdout, code, size)
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

// Cancelling Exec kills the command's whole process group (GST-8).
func TestExecCancelKillsTheGroup(t *testing.T) {
	t.Parallel()
	c := startAgent(t)
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	stream, err := c.Exec(ctx, sh(`sleep 1000 & echo $!; wait`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(resp.GetStdout())))
	if err != nil {
		t.Fatalf("pid %q: %v", resp.GetStdout(), err)
	}
	cancel()
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the command's child outlived the cancelled Exec")
		}
		time.Sleep(10 * time.Millisecond)
	}
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
