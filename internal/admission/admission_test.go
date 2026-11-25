// SPDX-License-Identifier: GPL-3.0-or-later

package admission

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
)

// queue admits jobs on a budget in the background and records the order
// they are admitted in, and each one's wait.
type queue struct {
	t *testing.T
	b *Budget

	mu       sync.Mutex
	admitted []string
	waits    map[string]Wait
	errs     map[string]error
	cancels  map[string]context.CancelFunc
	wg       sync.WaitGroup
}

func newQueue(t *testing.T, capMiB uint64) *queue {
	return &queue{t: t, b: New(capMiB), waits: map[string]Wait{}, errs: map[string]error{}, cancels: map[string]context.CancelFunc{}}
}

// admit starts admitting job, needing need MiB, and returns once it is
// admitted or waiting: every goroutine in the bubble is blocked then.
func (q *queue) admit(job string, need uint64) {
	ctx, cancel := context.WithCancel(q.t.Context())
	q.mu.Lock()
	q.cancels[job] = cancel
	q.mu.Unlock()
	q.wg.Go(func() {
		err := q.b.Admit(ctx, job, need, func(w Wait) {
			q.mu.Lock()
			q.waits[job] = w
			q.mu.Unlock()
		})
		q.mu.Lock()
		defer q.mu.Unlock()
		if err != nil {
			q.errs[job] = err
			return
		}
		q.admitted = append(q.admitted, job)
	})
	synctest.Wait()
}

// release releases job and lets whatever it frees be admitted.
func (q *queue) release(job string) {
	q.b.Release(job)
	synctest.Wait()
}

// cancel ends job's wait.
func (q *queue) cancel(job string) {
	q.mu.Lock()
	cancel := q.cancels[job]
	q.mu.Unlock()
	cancel()
	synctest.Wait()
}

// want fails the test unless exactly jobs were admitted, in that order.
func (q *queue) want(jobs ...string) {
	q.t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	if !slices.Equal(q.admitted, jobs) {
		q.t.Errorf("admitted %v, want %v", q.admitted, jobs)
	}
}

// end cancels every wait left and waits for each Admit to return.
func (q *queue) end() {
	q.mu.Lock()
	for _, cancel := range q.cancels {
		cancel()
	}
	q.mu.Unlock()
	q.wg.Wait()
}

// Jobs that don't fit wait, and are admitted in arrival order as memory
// frees; each wait's snapshot names the job's need, what is free, the cap
// and how many jobs are ahead.
func TestJobsThatDoNotFitWaitInArrivalOrder(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := newQueue(t, 4096)
		defer q.end()
		q.admit("a", 3072)
		q.admit("b", 3072)
		q.admit("c", 2048)
		q.admit("d", 3072)
		q.want("a")

		q.mu.Lock()
		if _, ok := q.waits["a"]; ok {
			t.Error("a job admitted at once reported a wait")
		}
		wantWaits := map[string]Wait{
			"b": {NeedMiB: 3072, FreeMiB: 1024, CapMiB: 4096, Ahead: 0},
			"c": {NeedMiB: 2048, FreeMiB: 1024, CapMiB: 4096, Ahead: 1},
			"d": {NeedMiB: 3072, FreeMiB: 1024, CapMiB: 4096, Ahead: 2},
		}
		for job, want := range wantWaits {
			if got := q.waits[job]; got != want {
				t.Errorf("%s's wait = %+v, want %+v", job, got, want)
			}
		}
		q.mu.Unlock()

		// Each release frees room for the next job only.
		q.release("a")
		q.want("a", "b")
		q.release("b")
		q.want("a", "b", "c")
		q.release("c")
		q.want("a", "b", "c", "d")
	})
}

// A job that would fit never overtakes an earlier one that doesn't fit yet.
func TestAJobThatFitsDoesNotOvertake(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := newQueue(t, 4096)
		defer q.end()
		q.admit("a", 2048)
		q.admit("big", 4096)
		// 2048 MiB are free, but big is first.
		q.admit("small", 1024)
		q.want("a")

		q.release("a")
		q.want("a", "big")
		q.release("big")
		q.want("a", "big", "small")
	})
}

// A cancelled wait leaves the queue with its context's error, holding
// nothing, and the jobs behind it move up.
func TestACancelledWaitLeavesTheQueue(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := newQueue(t, 4096)
		defer q.end()
		q.admit("a", 2048)
		q.admit("big", 4096)
		q.admit("small", 1024)
		q.admit("last", 2048)
		q.want("a")

		q.cancel("big")
		q.want("a", "small")
		q.mu.Lock()
		if err := q.errs["big"]; !errors.Is(err, context.Canceled) {
			t.Errorf("the cancelled wait returned %v, want context.Canceled", err)
		}
		q.mu.Unlock()
		// big holds nothing: once a goes, last fits beside small.
		q.release("big")
		q.release("a")
		q.want("a", "small", "last")
	})
}

// A wait ends with its context's cause, so the caller can tell what ended
// it.
func TestAWaitEndsWithItsContextsCause(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := New(1024)
		if err := b.Admit(t.Context(), "a", 1024, nil); err != nil {
			t.Fatal(err)
		}
		cause := errors.New("draining")
		ctx, cancel := context.WithCancelCause(t.Context())
		done := make(chan error, 1)
		go func() { done <- b.Admit(ctx, "b", 1024, func(Wait) {}) }()
		synctest.Wait()
		cancel(cause)
		if err := <-done; !errors.Is(err, cause) {
			t.Errorf("Admit = %v, want the context's cause", err)
		}
	})
}

// A job admitted while its waiting callback still runs, whose context then
// ends before the callback returns, fails its wait with the context's cause
// and holds nothing: the admission is given back, to the next in line.
func TestAnEndedWaitGivesBackAnAdmissionWhileWaitingRuns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := newQueue(t, 1024)
		defer q.end()
		q.admit("a", 1024)
		cause := errors.New("draining")
		ctx, cancel := context.WithCancelCause(t.Context())
		// The callback blocks as a send on a full stream does, until the
		// wait's context ends.
		done := make(chan error, 1)
		go func() {
			done <- q.b.Admit(ctx, "b", 1024, func(Wait) { <-ctx.Done() })
		}()
		synctest.Wait()
		q.admit("c", 1024)
		q.release("a")
		select {
		case err := <-done:
			t.Fatalf("Admit = %v while its waiting callback still runs", err)
		default:
		}
		cancel(cause)
		if err := <-done; !errors.Is(err, cause) {
			t.Errorf("Admit = %v, want the context's cause", err)
		}
		synctest.Wait()
		// b gave back what it was admitted to, so c, next in line, has it.
		q.want("a", "c")
		q.release("c")
	})
}

// A job whose context has already ended is still admitted when it fits at
// once: only a wait ends with the context.
func TestAJobThatFitsIsAdmittedWhateverItsContext(t *testing.T) {
	t.Parallel()
	b := New(1024)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := b.Admit(ctx, "a", 1024, func(Wait) { t.Error("a job that fits waited") }); err != nil {
		t.Fatalf("Admit = %v, want it admitted", err)
	}
	if err := b.Admit(ctx, "b", 1, func(Wait) {}); !errors.Is(err, context.Canceled) {
		t.Errorf("Admit of a job that must wait, with its context ended = %v, want context.Canceled", err)
	}
}

// Release is exactly once: a second release of a job, or a release of a
// job never admitted, changes nothing.
func TestReleaseIsExactlyOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := newQueue(t, 2048)
		defer q.end()
		q.admit("a", 2048)
		q.release("a")
		q.release("a")
		q.release("never")
		q.admit("b", 2048)
		q.release("a")
		q.admit("c", 1024)
		q.want("a", "b")

		q.release("b")
		q.want("a", "b", "c")
	})
}

// With no cap, nothing waits, and releasing changes nothing.
func TestNoCapAdmitsEverything(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := newQueue(t, 0)
		defer q.end()
		for _, job := range []string{"a", "b", "c"} {
			q.admit(job, 1<<20)
		}
		q.release("a")
		q.want("a", "b", "c")
		q.mu.Lock()
		if len(q.waits) != 0 {
			t.Errorf("waits %v with no cap", q.waits)
		}
		q.mu.Unlock()
	})
}

// Fits says whether a job fits the cap on its own; with no cap, every job
// does. A job that never fits is refused at once rather than left waiting
// forever.
func TestFits(t *testing.T) {
	t.Parallel()
	b := New(4096)
	if !b.Fits(4096) || b.Fits(4097) {
		t.Errorf("Fits(4096), Fits(4097) = %v, %v; want true, false", b.Fits(4096), b.Fits(4097))
	}
	if !New(0).Fits(1 << 40) {
		t.Error("with no cap, a job does not fit")
	}
	if err := b.Admit(t.Context(), "huge", 4097, func(Wait) { t.Error("a job that never fits waited") }); err == nil {
		t.Error("Admit of a job that never fits succeeded")
	}
}

// A job admitted twice is the caller's bug, and is refused, holding
// nothing more.
func TestAJobIsAdmittedOnce(t *testing.T) {
	t.Parallel()
	b := New(4096)
	if err := b.Admit(t.Context(), "a", 1024, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.Admit(t.Context(), "a", 1024, nil); err == nil {
		t.Error("a second Admit of one job succeeded")
	}
	if err := b.Admit(t.Context(), "b", 3072, func(Wait) { t.Error("b waited: a holds more than its need") }); err != nil {
		t.Fatal(err)
	}
}

// A job arrives when Admit records its wait: one that would fit and comes
// after it, even before the first is told it waits, never overtakes it,
// and waiters are admitted in the order their waits were recorded.
func TestArrivalIsWhenTheWaitIsRecorded(t *testing.T) {
	t.Parallel()
	// The race this guards against is in the scheduler's hands: many runs
	// make it all but certain to show.
	for range 200 {
		synctest.Test(t, func(t *testing.T) {
			q := newQueue(t, 2048)
			q.admit("a", 1024)
			ctx, cancel := context.WithCancel(t.Context())
			admit := func(job string, need uint64, waiting func(Wait)) {
				q.wg.Go(func() {
					if err := q.b.Admit(ctx, job, need, waiting); err != nil {
						return
					}
					q.mu.Lock()
					q.admitted = append(q.admitted, job)
					q.mu.Unlock()
				})
			}
			// A wait is recorded before its waiting runs, so small,
			// admitted from big's, arrives after big, and next, from
			// small's, after small.
			admit("big", 2048, func(Wait) {
				admit("small", 1024, func(Wait) { admit("next", 2048, nil) })
			})
			synctest.Wait()
			q.want("a")
			for _, job := range []string{"a", "big", "small"} {
				q.release(job)
			}
			q.want("a", "big", "small", "next")
			cancel()
			for _, job := range []string{"a", "big", "small", "next"} {
				q.b.Release(job)
			}
			q.end()
		})
	}
}
