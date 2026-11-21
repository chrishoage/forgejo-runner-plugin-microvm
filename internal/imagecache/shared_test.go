// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// newShared returns a sharedCalls whose cleanups are counted.
func newShared(cleaned *atomic.Int32) *sharedCalls[string] {
	return newSharedCalls(&sync.WaitGroup{}, func(string) { cleaned.Add(1) })
}

// Callers asking for one key while its call runs share that call, and get
// its result.
func TestSharedCallRunsOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var cleaned, runs atomic.Int32
		s := newShared(&cleaned)
		gate := make(chan struct{})
		fn := func(context.Context) (string, error) {
			runs.Add(1)
			<-gate
			return "v", nil
		}
		var g sync.WaitGroup
		for range 5 {
			g.Go(func() {
				v, release, err := s.do(t.Context(), "k", fn)
				defer release()
				if err != nil || v != "v" {
					t.Errorf("do: %q, %v", v, err)
				}
			})
		}
		synctest.Wait()
		close(gate)
		g.Wait()
		if n := runs.Load(); n != 1 {
			t.Errorf("fn ran %d times, want 1", n)
		}
		if n := cleaned.Load(); n != 1 {
			t.Errorf("%d cleanups, want 1 after the last release", n)
		}
		if len(s.calls) != 0 {
			t.Errorf("%d calls kept after every caller left", len(s.calls))
		}
	})
}

// A caller that is cancelled returns its own context's error at once; the
// call runs on for the others, and is cancelled only when the last leaves.
func TestSharedCallCancelledByLastWaiter(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var cleaned atomic.Int32
		s := newShared(&cleaned)
		var callCtx context.Context
		fn := func(ctx context.Context) (string, error) {
			callCtx = ctx
			<-ctx.Done()
			return "", ctx.Err()
		}
		ctx1, cancel1 := context.WithCancel(t.Context())
		ctx2, cancel2 := context.WithCancel(t.Context())
		errs := make(chan error, 2)
		for _, ctx := range []context.Context{ctx1, ctx2} {
			go func() {
				_, release, err := s.do(ctx, "k", fn)
				release()
				errs <- err
			}()
		}
		synctest.Wait()
		cancel1()
		synctest.Wait()
		if err := <-errs; !errors.Is(err, context.Canceled) {
			t.Errorf("the cancelled caller: %v, want context.Canceled", err)
		}
		if err := callCtx.Err(); err != nil {
			t.Fatalf("the call was cancelled with a caller still waiting: %v", err)
		}
		cancel2()
		synctest.Wait()
		if err := <-errs; !errors.Is(err, context.Canceled) {
			t.Errorf("the last caller: %v, want context.Canceled", err)
		}
		if callCtx.Err() == nil {
			t.Error("the call runs on with no caller waiting")
		}
		if n := cleaned.Load(); n != 0 {
			t.Errorf("%d cleanups of a failed call's result", n)
		}
	})
}

// A call that succeeds after its last caller left has its result cleaned
// up, and a caller that comes while an abandoned call still unwinds starts a
// call of its own instead of getting the abandoned one's result.
func TestSharedAbandonedCall(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var cleaned, runs atomic.Int32
		s := newShared(&cleaned)
		gate := make(chan struct{})
		// The first run ignores its cancel and succeeds late.
		fn := func(context.Context) (string, error) {
			if runs.Add(1) == 1 {
				<-gate
				return "old", nil
			}
			return "new", nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, release, err := s.do(ctx, "k", fn)
			release()
			done <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("do: %v, want context.Canceled", err)
		}
		v, release, err := s.do(t.Context(), "k", fn)
		if err != nil || v != "new" {
			t.Errorf("the caller after the abandoned call got %q, %v; want a call of its own", v, err)
		}
		release()
		close(gate)
		synctest.Wait()
		if n := cleaned.Load(); n != 2 {
			t.Errorf("%d cleanups, want 2: the abandoned result and the released one", n)
		}
		s.running.Wait()
	})
}

// A call that panics fails its callers with an error naming the panic, and
// leaves the process running.
func TestSharedCallPanics(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var cleaned atomic.Int32
		s := newShared(&cleaned)
		_, release, err := s.do(t.Context(), "k", func(context.Context) (string, error) { panic("boom") })
		release()
		if err == nil || !strings.Contains(err.Error(), "panicked: boom") {
			t.Errorf("do: %v, want the panic as an error", err)
		}
	})
}
