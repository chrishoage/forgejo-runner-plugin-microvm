// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// testUser is the user in every test root's /etc/passwd with the test's own
// ids, its home /home/tester, and the test's own groups, so an Exec as it
// changes no ids and runs unprivileged.
const testUser = "tester"

// testRoot returns a job's root for the agent's Exec: a directory holding
// /etc/passwd and /etc/group, which name root and testUser.
func testRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	passwd := fmt.Sprintf("root:x:0:0:root:/root:/bin/sh\n%s:x:%d:%d::/home/%s:/bin/sh\n", testUser, os.Getuid(), os.Getgid(), testUser)
	var group strings.Builder
	for _, g := range groups {
		fmt.Fprintf(&group, "g%d:x:%d:%s\n", g, g, testUser)
	}
	writeUsers(t, root, passwd, group.String())
	return root
}

// writeUsers writes /etc/passwd and /etc/group in the job's root at root.
func writeUsers(t *testing.T, root, passwd, group string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"passwd": passwd, "group": group} {
		if err := os.WriteFile(filepath.Join(root, "etc", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// testServer returns the agent's service in a test root, with no image
// defaults.
func testServer(t *testing.T) *server {
	t.Helper()
	return &server{log: slog.New(slog.DiscardHandler), image: &agentv1.Image{}, root: testRoot(t)}
}

// startAgent serves the agent of testServer on a unix socket on the host
// and returns a client for it.
func startAgent(t *testing.T) agentv1.AgentServiceClient {
	t.Helper()
	return startServer(t, testServer(t))
}

// startServer serves s on a unix socket on the host and returns a client
// for it. The socket is in a short directory of its own, not t.TempDir: a
// unix socket's path holds at most 107 bytes, which a long test name under
// CI's temp directory passes.
func startServer(t *testing.T, s *server) agentv1.AgentServiceClient {
	t.Helper()
	dir, err := os.MkdirTemp("", "ag-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")
	lis, err := new(net.ListenConfig).Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		// Closing the listener ends serve with an error.
		_ = serve(lis, s)
	}()
	conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		_ = lis.Close()
		<-served
	})
	return agentv1.NewAgentServiceClient(conn)
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPing(t *testing.T) {
	t.Parallel()
	if _, err := startAgent(t).Ping(testContext(t), &agentv1.PingRequest{}); err != nil {
		t.Fatal(err)
	}
}
