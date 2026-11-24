// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"log/slog"
	"regexp"
	"strings"
	"testing"

	pluginv1alpha "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/gen/plugin/v1alpha"
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
