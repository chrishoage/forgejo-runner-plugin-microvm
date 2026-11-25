// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"context"
	"regexp"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The memory budget on the built binary (QUE-1, QUE-2, QUE-6, QUE-8), with
// max_memory_mib set to one job's memory, so only one VM runs at a time: a
// second job's Start waits, and is cancelled, ending with its context's
// code; a third, which arrived behind it, waits until the first job is
// removed, then completes. The plugin's log has each wait's lines.
func TestMemoryBudget(t *testing.T) {
	t.Parallel()
	p := startPluginSetup(t, buildPlugin(t), pluginSetup{
		settings: "lib_dir: " + devLibDir(t) + "\nmax_memory_mib: 512\n",
		profiles: "profiles:\n  default:\n    cpus: 1\n    memory_mib: 512\n    scratch_gib: 1\n",
	})
	image := readImage(t)
	first := bootImage(t, p, "first", image)
	cancelled, third := createImage(t, p, "cancelled", image), createImage(t, p, "third", image)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cancelCtx, cancelStart := context.WithCancel(ctx)
	defer cancelStart()
	cancelledDone := make(chan error, 1)
	go func() { cancelledDone <- cancelled.start(cancelCtx) }()
	waitForLog(t, p, `msg="waiting for memory" .*\benv=`+cancelled.id+` need_mib=512 free_mib=0 cap_mib=512 ahead=0\b`)
	thirdDone := make(chan error, 1)
	go func() { thirdDone <- third.start(ctx) }()
	waitForLog(t, p, `msg="waiting for memory" .*\benv=`+third.id+` need_mib=512 free_mib=0 cap_mib=512 ahead=1\b`)

	cancelStart()
	select {
	case err := <-cancelledDone:
		if status.Code(err) != codes.Canceled {
			t.Errorf("the cancelled waiting Start = %v, want Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled waiting Start did not return within 10 s")
	}
	waitForLog(t, p, `msg="stopped waiting for memory" .*\benv=`+cancelled.id+` need_mib=512 took=\S+ err="context canceled"`)
	select {
	case err := <-thirdDone:
		t.Fatalf("the third Start ended while the first job still holds the memory: %v", err)
	default:
	}

	first.remove(t)
	select {
	case err := <-thirdDone:
		if err != nil {
			t.Fatalf("the third Start, once the first job was removed: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the third Start did not complete once the first job was removed")
	}
	waitForLog(t, p, `msg="got memory" .*\benv=`+third.id+` need_mib=512 took=`)
	if code, out := third.exec(t, "true"); code != 0 {
		t.Errorf("Exec in the admitted job exited %d: %s", code, out)
	}
	if p.logMatches(regexp.MustCompile(`msg="got memory" .*\benv=` + cancelled.id)) {
		t.Error("the cancelled Start was admitted")
	}
}

// waitForLog waits up to 30 s for a line of p's log to match pattern.
func waitForLog(t *testing.T, p *plugin, pattern string) {
	t.Helper()
	re := regexp.MustCompile(pattern)
	deadline := time.Now().Add(30 * time.Second)
	for !p.logMatches(re) {
		if time.Now().After(deadline) {
			t.Fatalf("no line of the plugin's log matches %s after 30 s", re)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
