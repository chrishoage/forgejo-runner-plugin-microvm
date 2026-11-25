// SPDX-License-Identifier: GPL-3.0-or-later

//go:build kvm

package cmd_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1alpha "code.forgejo.org/forgejo/runner/v13/act/plugin/proto/v1alpha"
)

// startResult is how a Start in the background ended, with the progress
// lines it sent.
type startResult struct {
	lines []string
	err   error
}

// startInBackground runs Start on v with ctx in a goroutine, and returns
// the channel its result arrives on.
func (v *vm) startInBackground(ctx context.Context) <-chan startResult {
	done := make(chan startResult, 1)
	go func() {
		var r startResult
		defer func() { done <- r }()
		stream, err := v.c.Start(ctx, &pluginv1alpha.StartRequest{EnvironmentId: v.id})
		if err != nil {
			r.err = err
			return
		}
		for {
			out, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				r.err = err
				return
			}
			if d := out.GetData(); d != nil {
				r.lines = append(r.lines, string(d.GetData()))
			}
		}
	}()
	return done
}

// within waits up to d for a Start's result, failing t without one.
func within(t *testing.T, done <-chan startResult, d time.Duration, what string) startResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
		return startResult{}
	}
}

// notYet fails t if the Start whose result done carries has ended.
func notYet(t *testing.T, done <-chan startResult, what string) {
	t.Helper()
	select {
	case r := <-done:
		t.Fatalf("%s ended early: %v", what, r.err)
	default:
	}
}

// A second job on a read-write disk the first holds waits, and a third
// waits behind it; cancelling the second's Start ends it with Canceled and
// frees its place, so the third gets the disk once the first job is
// removed, and reads what the first job wrote, which Remove flushed. The
// third's setup log and the plugin's log name the disk and its holder
// (QUE-3, QUE-6, QUE-8, STO-9). One VM runs at a time.
func TestDiskQueueWriters(t *testing.T) {
	t.Parallel()
	img, host := baseImage(t)
	disk := ext4Disk(t, "persist")
	p := startPluginWith(t, buildPlugin(t), fmt.Sprintf(`profiles:
  rw:
    cpus: 1
    memory_mib: 512
    disks: [{source: %s, target: /data, read_write: true}]
`, disk))
	ref := pushImage(t, host, "disk-queue-writers", img) + ";profile=rw"
	first := bootImage(t, p, "first", ref)
	// No sync: Remove's flush must carry it to the host file.
	if code, out := as(t, first, "0", "echo from-first > /data/flushed"); code != 0 {
		t.Fatalf("the first job's step exited %d:\n%s", code, out)
	}
	cancelled, third := createImage(t, p, "cancelled", ref), createImage(t, p, "third", ref)
	quoted := regexp.QuoteMeta(disk)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cancelCtx, cancelStart := context.WithCancel(ctx)
	defer cancelStart()
	cancelledDone := cancelled.startInBackground(cancelCtx)
	waitForLog(t, p, `msg="waiting for disk" .*\benv=`+cancelled.id+` disk=`+quoted+` mode=rw holders=`+first.id+` ahead=0\b`)
	thirdDone := third.startInBackground(ctx)
	waitForLog(t, p, `msg="waiting for disk" .*\benv=`+third.id+` disk=`+quoted+` mode=rw holders=`+first.id+` ahead=1\b`)

	cancelStart()
	if r := within(t, cancelledDone, 10*time.Second, "the cancelled waiting Start"); status.Code(r.err) != codes.Canceled {
		t.Errorf("the cancelled waiting Start = %v, want Canceled", r.err)
	}
	waitForLog(t, p, `msg="stopped waiting for disk" .*\benv=`+cancelled.id+` disk=`+quoted+` mode=rw took=\S+ err="context canceled"`)
	notYet(t, thirdDone, "the third Start, while the first job holds the disk,")

	first.remove(t)
	r := within(t, thirdDone, 5*time.Minute, "the third Start, once the first job was removed,")
	if r.err != nil {
		t.Fatalf("the third Start, once the first job was removed: %v", r.err)
	}
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`^\[runner-microvm \d+\.\d\ds\] waiting for disk ` + quoted + ` \(held by ` + first.id + `; 1 ahead\)\n$`),
		regexp.MustCompile(`^\[runner-microvm \d+\.\d\ds\] got disk ` + quoted + ` after \d+\.\d\ds\n$`),
	} {
		if !slices.ContainsFunc(r.lines, want.MatchString) {
			t.Errorf("the third Start's setup log has no line matching %s:\n%s", want, strings.Join(r.lines, ""))
		}
	}
	waitForLog(t, p, `msg="got disk" .*\benv=`+third.id+` disk=`+quoted+` mode=rw took=`)
	if code, out := as(t, third, "0", `test "$(cat /data/flushed)" = from-first`); code != 0 {
		t.Errorf("the third job does not see the first job's data: exited %d:\n%s", code, out)
	}
	if p.logMatches(regexp.MustCompile(`msg="got disk" .*\benv=` + cancelled.id)) {
		t.Error("the cancelled Start got the disk")
	}
}

// Two jobs reading one read-only disk run at the same time, and a writer
// of that disk waits for both, its wait naming both as holders (QUE-3).
// At most the two readers' VMs run at once.
func TestDiskQueueReaders(t *testing.T) {
	t.Parallel()
	img, host := baseImage(t)
	disk := ext4Disk(t, "shared")
	p := startPluginWith(t, buildPlugin(t), fmt.Sprintf(`profiles:
  ro:
    cpus: 1
    memory_mib: 512
    disks: [{source: %[1]s, target: /data}]
  rw:
    cpus: 1
    memory_mib: 512
    disks: [{source: %[1]s, target: /data, read_write: true}]
`, disk))
	ref := pushImage(t, host, "disk-queue-readers", img)
	a := bootImage(t, p, "ro-a", ref+";profile=ro")
	b := bootImage(t, p, "ro-b", ref+";profile=ro")
	for _, v := range []*vm{a, b} {
		if code, out := as(t, v, "0", `test "$(cat /data/marker)" = shared`); code != 0 {
			t.Errorf("a read-only job's step exited %d:\n%s", code, out)
		}
	}
	if p.logMatches(regexp.MustCompile(`msg="waiting for disk"`)) {
		t.Error("a reader waited beside another reader")
	}

	writer := createImage(t, p, "rw", ref+";profile=rw")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	done := writer.startInBackground(ctx)
	holders := []string{a.id, b.id}
	slices.Sort(holders)
	waitForLog(t, p, `msg="waiting for disk" .*\benv=`+writer.id+` disk=`+regexp.QuoteMeta(disk)+` mode=rw holders=`+strings.Join(holders, ",")+` ahead=0\b`)
	a.remove(t)
	notYet(t, done, "the writer's Start, while a reader holds the disk,")
	b.remove(t)
	if r := within(t, done, 5*time.Minute, "the writer's Start, once both readers were removed,"); r.err != nil {
		t.Fatalf("the writer's Start, once both readers were removed: %v", r.err)
	}
	if code, out := as(t, writer, "0", "echo written > /data/new && sync"); code != 0 {
		t.Errorf("the writer's step exited %d:\n%s", code, out)
	}
}
