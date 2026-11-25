// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"time"

	pluginv1alpha "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
)

// VM-11's target: with the image cached, Start takes about 2 s or less.
// The check fails only above 3 s, so it catches a real slowdown without
// failing on a loaded host's noise; the logged median shows drift toward
// the target (docs/architecture.md). Changing the bound is a decision.
const (
	startBound  = 3 * time.Second
	timedStarts = 5
)

// TestStartTime is VM-11's check, with the plugin run as the test's user:
// startTimes.
//
// It runs apart from the other cmd tests' VMs, not in parallel with them,
// so the times are of one Start, not of whatever else is booting beside it.
// Other packages' test binaries, which go test ./... runs alongside cmd's,
// may still share the CPU: the first suspect if it flakes.
func TestStartTime(t *testing.T) {
	startTimes(t, startPlugin(t, buildPlugin(t)))
}

// TestStartTimeAsRoot is TestStartTime with serve run as root under
// `sudo -n`, where libkrun's virtiofs server, which serves the boot root
// and layer share on Start's path, switches credentials for each request.
// Without passwordless sudo it skips, saying so, unless CI is set.
func TestStartTimeAsRoot(t *testing.T) {
	needRoot(t)
	p := startRootPluginWith(t, buildPlugin(t), testProfiles)
	p.wantRoot(t)
	startTimes(t, p)
}

// startTimes warms p's image cache with one untimed Create and Start of the
// image e2e/image pins, then times timedStarts Create, Start, Remove
// cycles on it, each from sending Start to receiving StartComplete. It logs
// every time and their median, and fails when the median is above
// startBound. The first Start pulls and converts the image, which VM-11
// excludes; the median shrugs off one slow boot on a shared runner.
func startTimes(t *testing.T, p *plugin) {
	t.Helper()
	image := readImage(t)
	bootImage(t, p, "start-time-warm-up", image).remove(t)
	times := make([]time.Duration, 0, timedStarts)
	for i := range timedStarts {
		v := createImage(t, p, fmt.Sprintf("start-time-%d", i), image)
		took := v.timedStart(t)
		v.remove(t)
		t.Logf("Start %d of %d: %v", i+1, timedStarts, took)
		times = append(times, took)
	}
	slices.Sort(times)
	median := times[len(times)/2]
	t.Logf("median of %d Starts: %v, bound %v", timedStarts, median, startBound)
	if median > startBound {
		t.Errorf("the median Start took %v, above VM-11's %v", median, startBound)
	}
}

// timedStart runs Start on v to its end and returns the time from sending
// Start to receiving StartComplete, failing t if Start fails or ends
// without one.
func (v *vm) timedStart(t *testing.T) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	sent := time.Now()
	stream, err := v.c.Start(ctx, &pluginv1alpha.StartRequest{EnvironmentId: v.id})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var took time.Duration
	for {
		out, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if out.GetStartComplete() != nil && took == 0 {
			took = time.Since(sent)
		}
	}
	if took == 0 {
		t.Fatal("Start ended without StartComplete")
	}
	return took
}
