// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	// The runner's own generated client, not the plugin's copy of it: this
	// test binary links act/runner, which registers the same proto file,
	// and protobuf refuses one file registered twice.
	pluginv1alpha "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// probeDir is where the tests copy the probe in the guest.
const probeDir = "/opt/runner-microvm-probe"

// buildProbe builds the network probe, static, and returns its path.
func buildProbe(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "probe")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-o", bin, module+"/cmd/internal/probe")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	cmd.WaitDelay = 10 * time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v\n%s", err, out)
	}
	return bin
}

// vm is a job the test drives through the plugin's own protocol: Create,
// Start, CopyIn, Exec and Remove, as the runner calls them.
type vm struct {
	c  pluginv1alpha.BackendPluginClient
	id string
}

// bootVM creates and starts a job on p's plugin with the image e2e/image
// pins, and removes it on cleanup.
func bootVM(t *testing.T, p *plugin) *vm {
	t.Helper()
	conn, err := grpclib.NewClient("unix://"+p.sock, grpclib.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := pluginv1alpha.NewBackendPluginClient(conn)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	resp, err := c.Create(ctx, &pluginv1alpha.CreateRequest{Name: "network", LabelArg: readImage(t)})
	if err != nil {
		t.Fatal(err)
	}
	v := &vm{c: c, id: resp.GetEnvironmentId()}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := c.Remove(ctx, &pluginv1alpha.RemoveRequest{EnvironmentId: v.id}); err != nil {
			t.Errorf("Remove: %v", err)
		}
	})
	stream, err := c.Start(ctx, &pluginv1alpha.StartRequest{EnvironmentId: v.id})
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := stream.Recv(); errors.Is(err, io.EOF) {
			return v
		} else if err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
}

// copyIn copies the file at path into dir in the guest, executable, with
// CopyIn.
func (v *vm) copyIn(t *testing.T, path, dir string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: filepath.Base(path), Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(b))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	stream, err := v.c.CopyIn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	for first := true; first || len(data) > 0; first = false {
		n := min(len(data), 1<<20)
		chunk := &pluginv1alpha.CopyInChunk{Data: data[:n]}
		if first {
			chunk.EnvironmentId, chunk.DestPath = &v.id, &dir
		}
		if err := stream.Send(chunk); err != nil {
			t.Fatal(err)
		}
		data = data[n:]
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatalf("CopyIn: %v", err)
	}
}

// exec runs argv in the guest with Exec and returns its exit code and
// output, stdout then stderr.
func (v *vm) exec(t *testing.T, argv ...string) (int32, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	stream, err := v.c.Exec(ctx, &pluginv1alpha.ExecRequest{EnvironmentId: v.id, Command: argv, Workdir: "/"})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	for {
		out, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			t.Fatalf("Exec %v: no exit code", argv)
		}
		if err != nil {
			t.Fatalf("Exec %v: %v", argv, err)
		}
		switch o := out.GetOutput().(type) {
		case *pluginv1alpha.ExecOutput_Data:
			if o.Data.GetStream() == pluginv1alpha.DataChunk_STDERR {
				stderr.Write(o.Data.GetData())
			} else {
				stdout.Write(o.Data.GetData())
			}
		case *pluginv1alpha.ExecOutput_ExecComplete:
			return o.ExecComplete.GetExitCode(), stdout.String() + stderr.String()
		case *pluginv1alpha.ExecOutput_ExecFailed:
			t.Fatalf("Exec %v: %s", argv, o.ExecFailed.GetErrorMessage())
		}
	}
}

// hostAddr returns a non-loopback, non-link-local IPv4 address of the CI
// host.
func hostAddr(t *testing.T) netip.Addr {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() && !p.Addr().IsLoopback() && !p.Addr().IsLinkLocalUnicast() {
			return p.Addr()
		}
	}
	t.Fatal("the host has no non-loopback IPv4 address")
	return netip.Addr{}
}

// A static probe, copied into a booted VM with CopyIn and run with Exec,
// connects to a listener on the host's non-loopback address, which sees the
// connection come from the host (NET-5), and resolves a name through the
// gateway's DNS. The guest's eth0 is 198.18.0.2/24 with MTU 1500, no IPv6
// address, and a default route via 198.18.0.1, and lo is up.
func TestGuestNetwork(t *testing.T) {
	host := hostAddr(t)
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp4", netip.AddrPortFrom(host, 0).String())
	if err != nil {
		t.Fatal(err)
	}
	from := make(chan netip.Addr, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case from <- c.RemoteAddr().(*net.TCPAddr).AddrPort().Addr().Unmap():
			default:
			}
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); <-done })

	probe := buildProbe(t)
	v := bootVM(t, startPlugin(t, buildPlugin(t)))
	v.copyIn(t, probe, probeDir)
	bin := probeDir + "/probe"

	if code, out := v.exec(t, bin, "connect", ln.Addr().String()); code != 0 || !strings.Contains(out, "connected 198.18.0.2:") {
		t.Errorf("connect: exit %d: %s", code, out)
	}
	select {
	case got := <-from:
		if got != host {
			t.Errorf("the host's listener accepted from %s, want the host's %s", got, host)
		}
	case <-time.After(10 * time.Second):
		t.Error("the host's listener saw no connection")
	}

	if code, out := v.exec(t, bin, "resolve", "localhost", "198.18.0.1:53"); code != 0 || !strings.Contains(out, "127.0.0.1") {
		t.Errorf("resolve: exit %d: %s", code, out)
	}

	code, out := v.exec(t, bin, "netinfo")
	if code != 0 {
		t.Fatalf("netinfo: exit %d: %s", code, out)
	}
	for _, want := range []string{
		"link lo up ",
		// Every address of eth0's, IPv6 too: its only one is its IPv4 one.
		"link eth0 up mtu 1500 addrs 198.18.0.2/24\n",
		"default via 198.18.0.1 dev eth0\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("netinfo does not show %q:\n%s", want, out)
		}
	}
}
