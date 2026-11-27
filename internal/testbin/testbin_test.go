// SPDX-License-Identifier: GPL-3.0-or-later

package testbin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// What Write writes runs at once, with the content given and mode 0755.
func TestWriteWritesAnExecutable(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hello")
	Write(t, path, "#!/bin/sh\necho hi \"$1\"\n")
	fi, err := os.Stat(path)
	if err != nil || fi.Mode() != 0o755 {
		t.Fatalf("stat %s: %v, %v; want mode 0755", path, fi, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "there").Output()
	if err != nil || string(out) != "hi there\n" {
		t.Fatalf("ran %s: %q, %v; want %q", path, out, err, "hi there\n")
	}
}

// fatalTB records a Fatal instead of failing the test.
type fatalTB struct {
	*testing.T
	msg string
}

func (f *fatalTB) Fatal(args ...any) { f.msg = fmt.Sprint(args...); runtime.Goexit() }

func (f *fatalTB) Fatalf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// A file Write can't make fails the test, naming it.
func TestWriteFailsTheTest(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "no", "dir", "hello")
	tb := &fatalTB{T: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Write(tb, path, "#!/bin/sh\n")
	}()
	<-done
	if !strings.Contains(tb.msg, path) {
		t.Fatalf("Write into a missing directory: %q, want a failure naming %s", tb.msg, path)
	}
}
