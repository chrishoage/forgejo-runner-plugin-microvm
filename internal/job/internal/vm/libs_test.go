// SPDX-License-Identifier: GPL-3.0-or-later

package vm

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runLibs runs Libs with dir and returns its exit code, stdout and stderr.
func runLibs(dir string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := Libs(dir, &out, &errOut)
	return code, out.String(), errOut.String()
}

// From a directory that lacks the libraries, Libs fails naming the missing
// library and the directory, and loads nothing from the system instead.
func TestLibsFromAnEmptyDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	code, out, errOut := runLibs(dir)
	if code != 1 || out != "" {
		t.Errorf("exit %d, stdout %q; want 1 and nothing", code, out)
	}
	if want := "libkrunfw.so.5 in lib_dir " + dir + ": "; !strings.HasPrefix(errOut, want) {
		t.Errorf("stderr %q, want it to start %q", errOut, want)
	}
}

// libkrun missing from a directory that holds libkrunfw fails naming
// libkrun and the directory.
func TestLibsWithoutLibkrun(t *testing.T) {
	t.Parallel()
	// libkrunfw is the dev shell's; libkrun is looked for once it loads.
	dir := t.TempDir()
	if err := os.Symlink(devLib(t, "libkrunfw.so.5"), filepath.Join(dir, "libkrunfw.so.5")); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runLibs(dir)
	if want := "libkrun.so.1 in lib_dir " + dir + ": "; code != 1 || !strings.HasPrefix(errOut, want) {
		t.Errorf("exit %d, stderr %q; want 1 and %q", code, errOut, want)
	}
}

// A libkrun.so.1 without every function the VM child calls fails naming
// each one it lacks.
func TestLibsWithoutTheFunctions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"libkrunfw.so.5", "libkrun.so.1"} {
		if err := os.Symlink(devLib(t, "libkrunfw.so.5"), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	code, _, errOut := runLibs(dir)
	// The file in parentheses is the one the test process loaded first
	// under that identity, which another test may have loaded by another
	// path.
	want := ") lacks krun_add_disk, krun_add_net_unixgram, "
	if code != 1 || !strings.HasPrefix(errOut, "libkrun.so.1 (") || !strings.Contains(errOut, want) ||
		!strings.Contains(errOut, ", krun_start_enter: ") {
		t.Errorf("exit %d, stderr %q; want 1 and every function, from %q", code, errOut, want)
	}
}

// A libkrunfw.so.5 in the directory that isn't the libkrunfw libkrun loads
// fails: libkrun would have taken another from the system.
func TestLibsWithAnotherLibkrunfw(t *testing.T) {
	t.Parallel()
	// libkrun under both names: its own libkrunfw then comes from its run
	// path, not the directory.
	dir := t.TempDir()
	for _, name := range []string{"libkrunfw.so.5", "libkrun.so.1"} {
		if err := os.Symlink(devLib(t, "libkrun.so.1"), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	code, _, errOut := runLibs(dir)
	if want := "libkrunfw.so.5 in lib_dir " + dir + " is not the libkrunfw.so.5 that libkrun.so.1 loads"; code != 1 || !strings.HasPrefix(errOut, want) {
		t.Errorf("exit %d, stderr %q; want 1 and %q", code, errOut, want)
	}
}

// A relative directory is refused: the VM child changes its working
// directory before it loads the libraries.
func TestLibsRefusesARelativeDirectory(t *testing.T) {
	t.Parallel()
	code, out, errOut := runLibs("lib")
	if code != 1 || out != "" || !strings.Contains(errOut, "lib_dir lib is not an absolute path") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// devLib is the dev shell's library name, from the directory only the tests
// and `just check-libs` read.
func devLib(t *testing.T, name string) string {
	t.Helper()
	dir := os.Getenv("RUNNER_MICROVM_LIBDIR")
	if dir == "" {
		t.Fatal("RUNNER_MICROVM_LIBDIR is not set: run the tests inside the dev shell")
	}
	return filepath.Join(dir, name)
}
