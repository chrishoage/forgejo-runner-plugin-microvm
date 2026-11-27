// SPDX-License-Identifier: GPL-3.0-or-later

package plugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/joblaunch"
	"github.com/chrishoage/forgejo-runner-plugin-microvm/internal/testbin"
)

// fakeJobBinary writes a job binary that runs script, and returns its path.
func fakeJobBinary(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner-microvm-job")
	testbin.Write(t, path, "#!/bin/sh\n"+script+"\n")
	return path
}

// checkLibs runs the job binary's libs with lib_dir as its argument, or
// none when it is unset, and returns the files it names.
func TestCheckLibsReturnsTheFiles(t *testing.T) {
	t.Parallel()
	// The fake names files after its arguments.
	bin := fakeJobBinary(t, `[ "$1" = libs ] || exit 99
echo "libkrun.so.1=${2:-sys}/libkrun.so.1 args=$#"
echo "libkrunfw.so.5=${2:-sys}/libkrunfw.so.5"`)
	for dir, want := range map[string]libFiles{
		"":         {libkrun: "sys/libkrun.so.1 args=1", libkrunfw: "sys/libkrunfw.so.5"},
		"/opt/lib": {libkrun: "/opt/lib/libkrun.so.1 args=2", libkrunfw: "/opt/lib/libkrunfw.so.5"},
	} {
		got, err := checkLibs(t.Context(), bin, dir)
		if err != nil || got != want {
			t.Errorf("lib_dir %q: %+v, %v; want %+v", dir, got, err, want)
		}
	}
}

// A failing libs stops startup with its error, saying where the libraries
// were to load from.
func TestCheckLibsFails(t *testing.T) {
	t.Parallel()
	const msg = "libkrunfw.so.5 in lib_dir /opt/lib: /opt/lib/libkrunfw.so.5: cannot open shared object file"
	bin := fakeJobBinary(t, "echo 'libkrun.so.1=/x' \necho '"+msg+"' >&2\nexit 1")
	for dir, where := range map[string]string{"": "by soname", "/opt/lib": "from lib_dir /opt/lib"} {
		_, err := checkLibs(t.Context(), bin, dir)
		if err == nil || !strings.Contains(err.Error(), msg) || !strings.Contains(err.Error(), "load libkrun and libkrunfw "+where) {
			t.Errorf("lib_dir %q: error %v, want one saying %q and holding %q", dir, err, where, msg)
		}
	}
}

// A libs that exits 0 without naming both files fails: what it printed is
// in the error.
func TestCheckLibsNeedsBothFiles(t *testing.T) {
	t.Parallel()
	for _, script := range []string{
		"",
		"echo libkrun.so.1=/lib/libkrun.so.1",
		"echo libkrunfw.so.5=/lib/libkrunfw.so.5",
		"printf 'libkrun.so.1=/lib/libkrun.so.1\\nlibkrunfw.so.5='",
		"echo /lib/libkrun.so.1 /lib/libkrunfw.so.5",
	} {
		_, err := checkLibs(t.Context(), fakeJobBinary(t, script), "")
		if err == nil || !strings.Contains(err.Error(), "did not name both files") {
			t.Errorf("%q: error %v, want one saying libs did not name both files", script, err)
		}
	}
}

// A libs that hangs past its timeout is killed with its whole process
// group, so what it started dies too, and checkLibs returns promptly.
func TestCheckLibsTimeoutKillsItsGroup(t *testing.T) {
	t.Parallel()
	pids := filepath.Join(t.TempDir(), "pids")
	// A child of its own, holding the output pipes, then the fake itself.
	bin := fakeJobBinary(t, "sleep 1000 &\necho $$ $! >"+pids+"\necho 'loading' >&2\nexec sleep 1000")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := checkLibs(ctx, bin, "")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "loading") {
		t.Errorf("error %v, want the context's deadline and what libs printed", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("checkLibs took %s to return", took)
	}
	for _, pid := range readPids(t, pids) {
		waitGone(t, pid)
	}
}

// A signal from term during libs ends it, with its process group, and
// checkLibsUntilSignal returns the signal and no error: the plugin stops
// before serving, not with libs killed by the signal.
func TestCheckLibsUntilSignal(t *testing.T) {
	t.Parallel()
	pids := filepath.Join(t.TempDir(), "pids")
	bin := fakeJobBinary(t, "sleep 1000 &\necho $$ $! >"+pids+".tmp\nmv "+pids+".tmp "+pids+"\nexec sleep 1000")
	term := make(chan os.Signal, 2)
	type result struct {
		sig os.Signal
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, sig, err := checkLibsUntilSignal(testContext(t), term, bin, "")
		done <- result{sig, err}
	}()
	got := readPids(t, pids)
	term <- syscall.SIGINT
	select {
	case r := <-done:
		if r.sig != syscall.SIGINT || r.err != nil {
			t.Errorf("returned signal %v, error %v; want SIGINT and no error", r.sig, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("checkLibsUntilSignal did not return after the signal")
	}
	for _, pid := range got {
		waitGone(t, pid)
	}
	if len(term) != 0 {
		t.Error("the signal is still queued")
	}
}

// With no signal, checkLibsUntilSignal returns what checkLibs does.
func TestCheckLibsUntilSignalPassesTheResult(t *testing.T) {
	t.Parallel()
	ok := fakeJobBinary(t, "echo libkrun.so.1=/l/libkrun.so.1\necho libkrunfw.so.5=/l/libkrunfw.so.5")
	libs, sig, err := checkLibsUntilSignal(t.Context(), make(chan os.Signal), ok, "")
	if want := (libFiles{libkrun: "/l/libkrun.so.1", libkrunfw: "/l/libkrunfw.so.5"}); libs != want || sig != nil || err != nil {
		t.Errorf("%+v, %v, %v; want %+v", libs, sig, err, want)
	}
	_, sig, err = checkLibsUntilSignal(t.Context(), nil, fakeJobBinary(t, "echo nope >&2; exit 1"), "")
	if sig != nil || err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("a failing libs: %v, %v; want its error", sig, err)
	}
}

// readPids waits for the file at path, which a fake writes, and returns the
// pids it holds.
func readPids(t *testing.T, path string) []int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			var pids []int
			for f := range strings.FieldsSeq(string(b)) {
				pid, err := strconv.Atoi(f)
				if err != nil {
					t.Fatalf("%s: %q", path, b)
				}
				pids = append(pids, pid)
			}
			if len(pids) > 0 {
				return pids
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake never wrote %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitGone waits until pid has exited: it is gone, or a zombie nobody has
// reaped yet.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return
		}
		// The state follows the parenthesised command name.
		if i := strings.LastIndexByte(string(b), ')'); i >= 0 && strings.HasPrefix(string(b[i+1:]), " Z") {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d outlived the job binary's run", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// What the job binary prints is kept only up to a bound.
func TestCappedBuffer(t *testing.T) {
	t.Parallel()
	b := &cappedBuffer{max: 5}
	for _, s := range []string{"abc", "defg", "hij"} {
		if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if got := b.String(); got != "abcde" {
		t.Errorf("kept %q, want %q", got, "abcde")
	}
}

// fakeMkfsOnPath puts a fake mkfs.ext4 first on PATH, so run's startup gets
// past its lookup. A test that calls it can't be parallel.
func fakeMkfsOnPath(t *testing.T) {
	t.Helper()
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "mkfs.ext4"), nil, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// fakePayload is a payloadFunc whose job binary is a shell script that
// answers version and runs libs for libs.
func fakePayload(libs string) payloadFunc {
	return func() ([]byte, []byte, error) {
		job := "#!/bin/sh\ncase \"$1\" in\nversion) echo fake ;;\nlibs)\n" + libs + "\n;;\n*) exit 99 ;;\nesac\n"
		return []byte(job), []byte("fake agent"), nil
	}
}

// socketPaths are the listen socket's path and the temporary name it is
// bound under.
func socketPaths(dirs Dirs) []string {
	return []string{dirs.Listen, filepath.Join(filepath.Dir(dirs.Listen), "."+filepath.Base(dirs.Listen)+".new")}
}

// A libs that fails stops the plugin's startup with its error, before the
// listen socket is bound: the fake fails differently if it finds the socket.
//
// It is not parallel: it sets PATH.
func TestStartupStopsWithTheLibsError(t *testing.T) {
	fakeMkfsOnPath(t)
	dirs := testDirs(filepath.Join(t.TempDir(), "plugin"))
	socks := socketPaths(dirs)
	const msg = "libkrun.so.1: cannot open shared object file"
	libs := fmt.Sprintf("if [ -e '%s' ] || [ -e '%s' ]; then echo 'the socket is bound' >&2; else echo '%s' >&2; fi\nexit 1",
		socks[0], socks[1], msg)
	// Bounded, so a startup that skips the check and serves fails the test.
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	err := run(ctx, discardLog(), configOf(dirs), nil, nil, fakePayload(libs), newImageCache, joblaunch.New)
	if err == nil || !strings.Contains(err.Error(), msg) || !strings.Contains(err.Error(), "load libkrun and libkrunfw by soname") {
		t.Fatalf("run returned %v, want libs's error %q", err, msg)
	}
	for _, p := range socks {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the failed startup left %s: %v", p, err)
		}
	}
}

// A SIGTERM during libs ends it and stops the plugin before it serves, with
// no error, as at any other point of startup.
//
// It is not parallel: it sets PATH.
func TestSignalDuringLibsStopsBeforeServing(t *testing.T) {
	fakeMkfsOnPath(t)
	dirs := testDirs(filepath.Join(t.TempDir(), "plugin"))
	pids := filepath.Join(t.TempDir(), "pids")
	libs := "echo $$ >'" + pids + ".tmp'\nmv '" + pids + ".tmp' '" + pids + "'\nexec sleep 1000"
	var logs logBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	term := make(chan os.Signal, 2)
	done := make(chan error, 1)
	go func() {
		done <- run(testContext(t), log, configOf(dirs), nil, term, fakePayload(libs), newImageCache, joblaunch.New)
	}()
	pid := readPids(t, pids)
	term <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v, want no error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop after the signal")
	}
	if !strings.Contains(logs.String(), "stopped before serving") {
		t.Errorf("the log does not say the plugin stopped before serving:\n%s", logs.String())
	}
	waitGone(t, pid[0])
	for _, p := range socketPaths(dirs) {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the stopped startup left %s: %v", p, err)
		}
	}
}
