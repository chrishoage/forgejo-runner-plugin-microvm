// SPDX-License-Identifier: GPL-3.0-or-later

package jobnet

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"runtime/pprof"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// counting is a slog handler that counts its records.
type counting struct{ n *atomic.Int64 }

func (c counting) Enabled(context.Context, slog.Level) bool  { return true }
func (c counting) Handle(context.Context, slog.Record) error { c.n.Add(1); return nil }
func (c counting) WithAttrs([]slog.Attr) slog.Handler        { return c }
func (c counting) WithGroup(string) slog.Handler             { return c }

// Once a job network runs, the library's logger writes only its errors,
// to the job's log and at most libraryLogBurst at once, so a guest that
// makes it log a line per packet can't fill the job log. Not parallel: the
// library's logger is process-wide, and every New routes it.
func TestLibraryLogIsBounded(t *testing.T) {
	var n atomic.Int64
	nw, err := New(testConfig(), slog.New(counting{&n}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := nw.Close(); err != nil {
			t.Error(err)
		}
	})
	if logrus.IsLevelEnabled(logrus.WarnLevel) {
		t.Error("the library's warnings are on")
	}
	for range 1000 {
		logrus.Warn("a warning per packet")
		logrus.Error("an error per packet")
	}
	if got := n.Load(); got < 1 || got > libraryLogBurst+1 {
		t.Errorf("the job log took %d of 1000 lines, want 1 to %d", got, libraryLogBurst+1)
	}
}

// A job network closes without a line in the job log while the switch is
// reading a VM's port: closing the port ends that read with an error the
// library logs, and that is the job's normal teardown. Not parallel: the
// library's logger is process-wide.
func TestCloseLogsNoError(t *testing.T) {
	var n atomic.Int64
	nw, err := New(testConfig(), slog.New(counting{&n}))
	if err != nil {
		t.Fatal(err)
	}
	vmEnd, err := nw.Attach()
	if err != nil {
		_ = nw.Close()
		t.Fatal(err)
	}
	defer func() { _ = vmEnd.Close() }()
	waitPortRead(t)
	if err := nw.Close(); err != nil {
		t.Fatal(err)
	}
	if got := n.Load(); got != 0 {
		t.Errorf("closing the job network logged %d lines", got)
	}
}

// portRead matches, in a goroutine dump, a goroutine blocked in the
// switch's read of a port.
var portRead = regexp.MustCompile(`(?s)goroutine \d+ \[IO wait[^\]]*\]:\n(?:[^\n]+\n)*?[^\n]*tap\.\(\*Switch\)\.rxNonStream`)

// waitPortRead waits until a goroutine of the switch's is blocked reading a
// port, so a Close then ends that read rather than the loop around it.
func waitPortRead(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var b bytes.Buffer
		if err := pprof.Lookup("goroutine").WriteTo(&b, 2); err != nil {
			t.Fatal(err)
		}
		if portRead.Match(b.Bytes()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the switch is not reading the port:\n%s", b.String())
		}
		time.Sleep(time.Millisecond)
	}
}
