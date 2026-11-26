// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
	agentv1 "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/runnermicrovm/agent/v1"
)

// loggedServer returns a client of a server on cache and launcher whose
// log, at level, the test reads.
func loggedServer(t *testing.T, cache *fakeCache, launcher *fakeLauncher, level slog.Level) (pluginv1alpha.BackendPluginClient, *logBuffer) {
	t.Helper()
	srv := newTestServer(t, cache, launcher)
	log := &logBuffer{}
	srv.log = slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: level}))
	return pluginv1alpha.NewBackendPluginClient(serve(t, srv)), log
}

// linesOf returns the lines of log whose message is msg.
func linesOf(log, msg string) []string {
	var out []string
	m := "msg=" + msg + " "
	if strings.Contains(msg, " ") {
		m = `msg="` + msg + `" `
	}
	for line := range strings.Lines(log) {
		if strings.Contains(line, m) {
			out = append(out, line)
		}
	}
	return out
}

// wantLine fails t unless log has exactly one line of msg, and it matches
// each of want, returning it.
func wantLine(t *testing.T, log, msg string, want ...string) string {
	t.Helper()
	lines := linesOf(log, msg)
	if len(lines) != 1 {
		t.Errorf("%d lines of %q, want one:\n%s", len(lines), msg, log)
		return ""
	}
	for _, w := range want {
		if !regexp.MustCompile(w).MatchString(lines[0]) {
			t.Errorf("the %q line does not match %s:\n%s", msg, w, lines[0])
		}
	}
	return lines[0]
}

// At info, Create, Start done and Remove each log one line carrying env=,
// with what the event list names, and the calls in between log nothing.
func TestJobEventsCarryEnv(t *testing.T) {
	t.Parallel()
	c, log := loggedServer(t, &fakeCache{}, &fakeLauncher{}, slog.LevelInfo)
	resp, err := c.Create(testContext(t), &pluginv1alpha.CreateRequest{Name: "build", LabelArg: testImage + ";profile=default"})
	if err != nil {
		t.Fatal(err)
	}
	id := resp.GetEnvironmentId()
	if _, err := start(testContext(t), c, id); err != nil {
		t.Fatal(err)
	}
	if _, err := execAll(t, c, &pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Remove(testContext(t), &pluginv1alpha.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatal(err)
	}
	env := regexp.QuoteMeta("env=" + id + " ")
	got := log.String()
	wantLine(t, got, "created", `^time=\S+ level=INFO `, env, ` name=build `, regexp.QuoteMeta(` label="`+testImage+`;profile=default" `),
		regexp.QuoteMeta(" image="+testImage+" "), ` profile=default `)
	wantLine(t, got, "started", `^time=\S+ level=INFO `, env, ` took=\d`, ` image=\S+ digest=sha256:0 layers=2 layers_pulled=1 profile=default`)
	wantLine(t, got, "removed", `^time=\S+ level=INFO `, env, ` err=<nil> since_create=\d`)
	if strings.Contains(got, "level=DEBUG") || strings.Contains(got, "msg=exec") {
		t.Errorf("a debug line at info:\n%s", got)
	}
}

// A failed Start logs one error line carrying env= and its cause.
func TestStartFailedCarriesEnv(t *testing.T) {
	t.Parallel()
	c, log := loggedServer(t, &fakeCache{}, &fakeLauncher{does: networkFails}, slog.LevelInfo)
	id := create(t, c)
	if _, err := start(testContext(t), c, id); err == nil {
		t.Fatal("Start succeeded")
	}
	wantLine(t, log.String(), "start failed", `^time=\S+ level=ERROR `, regexp.QuoteMeta("env="+id+" "), `err=".*cannot reserve gateway IP`)
	if lines := linesOf(log.String(), "started"); len(lines) != 0 {
		t.Errorf("a failed Start logged started:\n%s", log.String())
	}
}

// A failed Start's line keeps a service VM's error's summary, the VM's
// name and exit code, and leaves out its log and console tails, which the
// runner gets: a service's console holds its own output, produced under its
// env, and the plugin's log masks nothing.
func TestStartFailedLogsNoVMOutput(t *testing.T) {
	t.Parallel()
	c, log := loggedServer(t, &fakeCache{}, &fakeLauncher{does: lastExitsDuringBoot}, slog.LevelInfo)
	id := createWithServices(t, c, "build", svc("cache", "redis:8"))
	const marker = "console line 25"
	_, err := start(testContext(t), c, id)
	if !strings.Contains(status.Convert(err).Message(), marker) {
		t.Errorf("Start = %v; want the service's console tail, %q", err, marker)
	}
	wantLine(t, log.String(), "start failed", regexp.QuoteMeta(`err="VM cache exited during boot (1)"`))
	if strings.Contains(log.String(), marker) || strings.Contains(log.String(), "log line 10") {
		t.Errorf("the plugin's log has the VM's output:\n%s", log.String())
	}
}

// A failed Start's line keeps the job's container's log and console tails:
// its console holds only the kernel's and the agent's output, which LOG-2
// keeps free of secrets, and they say why it failed to boot.
func TestStartFailedLogsTheJobsContainerOutput(t *testing.T) {
	t.Parallel()
	c, log := loggedServer(t, &fakeCache{}, &fakeLauncher{does: exitsDuringBoot}, slog.LevelInfo)
	id := create(t, c)
	if _, err := start(testContext(t), c, id); err == nil {
		t.Fatal("Start succeeded")
	}
	wantLine(t, log.String(), "start failed", regexp.QuoteMeta(`err="VM job exited during boot (1)\nVM log:\n`), "log line 10", "console line 25")
}

// At debug, each Exec, CopyIn and CopyOut logs one line carrying env=,
// argv[0] for an Exec, never another argument or any environment, and the
// path for a copy, each with its exit code or bytes copied and its time.
func TestCallsLogAtDebug(t *testing.T) {
	t.Parallel()
	c, log := loggedServer(t, &fakeCache{}, &fakeLauncher{}, slog.LevelDebug)
	id := create(t, c)
	if _, err := start(testContext(t), c, id); err != nil {
		t.Fatal(err)
	}
	if _, err := execAll(t, c, &pluginv1alpha.ExecRequest{
		EnvironmentId: id, Command: []string{"sh", "--the-flag", "echo the-argument"},
		Env: map[string]string{"A": "the-env-value", "PATH": "/the/path"}, Workdir: "/the-workdir",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := execAll(t, c, &pluginv1alpha.ExecRequest{EnvironmentId: id, Command: []string{"start-error"}}); err != nil {
		t.Fatal(err)
	}
	in, err := c.CopyIn(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Send(&pluginv1alpha.CopyInChunk{EnvironmentId: &id, DestPath: new("/dest"), Data: []byte("o")}); err != nil {
		t.Fatal(err)
	}
	if err := in.Send(&pluginv1alpha.CopyInChunk{Data: []byte("k")}); err != nil {
		t.Fatal(err)
	}
	if _, err := in.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}
	out, err := c.CopyOut(testContext(t), &pluginv1alpha.CopyOutRequest{EnvironmentId: id, SrcPath: "/src/dir"})
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := out.Recv(); err != nil {
			break
		}
	}
	got := log.String()
	env := regexp.QuoteMeta("env=" + id + " ")
	execs := linesOf(got, "exec")
	if len(execs) != 2 {
		t.Fatalf("%d exec lines, want 2:\n%s", len(execs), got)
	}
	for i, want := range []string{`argv0=sh exit_code=3 took=\d`, `argv0=start-error start_failed=true took=\d`} {
		if !regexp.MustCompile(`^time=\S+ level=DEBUG msg=exec ` + env + want).MatchString(execs[i]) {
			t.Errorf("exec line %d does not match %s:\n%s", i, want, execs[i])
		}
	}
	for _, secret := range []string{"the-argument", "--the-flag", "the-env-value", "/the/path", "the-workdir"} {
		if strings.Contains(got, secret) {
			t.Errorf("the log holds %q:\n%s", secret, got)
		}
	}
	wantLine(t, got, "copy in", `^time=\S+ level=DEBUG `, env, ` path=/dest bytes=2 took=\d`)
	wantLine(t, got, "copy out", `^time=\S+ level=DEBUG `, env, ` path=/src/dir bytes=8 took=\d`)
}

// stopsCopyInAgent fails a CopyIn once it has read the first message, and
// reads no more.
type stopsCopyInAgent struct{ fakeAgent }

func (*stopsCopyInAgent) CopyIn(stream agentv1.AgentService_CopyInServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	return status.Error(codes.Internal, "the agent stopped")
}

// A CopyIn's debug line counts only the bytes sent to the agent: a chunk
// whose send failed mid-copy is not among them.
func TestCopyInCountsOnlyWhatItSent(t *testing.T) {
	t.Parallel()
	c, log := loggedServer(t, &fakeCache{}, &fakeLauncher{agent: &stopsCopyInAgent{}}, slog.LevelDebug)
	id := create(t, c)
	if _, err := start(testContext(t), c, id); err != nil {
		t.Fatal(err)
	}
	in, err := c.CopyIn(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Send(&pluginv1alpha.CopyInChunk{EnvironmentId: &id, DestPath: new("/dest"), Data: []byte("a")}); err != nil {
		t.Fatal(err)
	}
	// Far more than the stream's flow-control window: the agent, which
	// reads no more, ends its stream before the plugin can send it all.
	// The runner's send may fail too, once the plugin has given up.
	_ = in.Send(&pluginv1alpha.CopyInChunk{Data: make([]byte, 16<<20)})
	if _, err := in.CloseAndRecv(); err == nil {
		t.Fatal("CopyIn succeeded with the agent stopped")
	}
	wantLine(t, log.String(), "copy in", `^time=\S+ level=DEBUG `, regexp.QuoteMeta("env="+id+" "), ` path=/dest bytes=1 `)
}
