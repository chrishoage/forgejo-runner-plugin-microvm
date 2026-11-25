// SPDX-License-Identifier: GPL-3.0-or-later

package admission

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// queue admits jobs on an admitter in the background and records the order
// they are admitted in, each one's waits, and each wait's end.
type queue struct {
	t *testing.T
	b *Admitter

	mu       sync.Mutex
	admitted []string
	waits    map[string][]Wait
	ended    map[string][]error
	errs     map[string]error
	cancels  map[string]context.CancelFunc
	wg       sync.WaitGroup
}

func newQueue(t *testing.T, capMiB uint64) *queue {
	return &queue{t: t, b: New(capMiB), waits: map[string][]Wait{}, ended: map[string][]error{}, errs: map[string]error{}, cancels: map[string]context.CancelFunc{}}
}

// admit starts admitting job, needing need MiB, and returns once it is
// admitted or waiting: every goroutine in the bubble is blocked then.
func (q *queue) admit(job string, need uint64) { q.admitDisks(job, need) }

// admitDisks starts admitting job, needing disks and need MiB, as admit
// does.
func (q *queue) admitDisks(job string, need uint64, disks ...Disk) {
	ctx, cancel := context.WithCancel(q.t.Context())
	q.mu.Lock()
	q.cancels[job] = cancel
	q.mu.Unlock()
	q.wg.Go(func() {
		err := q.b.Admit(ctx, job, disks, need, Observer{
			Waiting: func(w Wait) {
				q.mu.Lock()
				q.waits[job] = append(q.waits[job], w)
				q.mu.Unlock()
			},
			Waited: func(_ Wait, _ time.Duration, err error) {
				q.mu.Lock()
				q.ended[job] = append(q.ended[job], err)
				q.mu.Unlock()
			},
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

// wantAll fails the test unless exactly jobs were admitted, in any order:
// jobs admitted at one release run in no order of their own.
func (q *queue) wantAll(jobs ...string) {
	q.t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	got, want := slices.Sorted(slices.Values(q.admitted)), slices.Sorted(slices.Values(jobs))
	if !slices.Equal(got, want) {
		q.t.Errorf("admitted %v, want %v in any order", q.admitted, jobs)
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
			if got := q.waits[job]; len(got) != 1 || !reflect.DeepEqual(got[0], want) {
				t.Errorf("%s's waits = %+v, want %+v", job, got, want)
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
		if err := b.Admit(t.Context(), "a", nil, 1024, Observer{}); err != nil {
			t.Fatal(err)
		}
		cause := errors.New("draining")
		ctx, cancel := context.WithCancelCause(t.Context())
		done := make(chan error, 1)
		go func() { done <- b.Admit(ctx, "b", nil, 1024, Observer{Waiting: func(Wait) {}}) }()
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
			done <- q.b.Admit(ctx, "b", nil, 1024, Observer{Waiting: func(Wait) { <-ctx.Done() }})
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
	if err := b.Admit(ctx, "a", nil, 1024, Observer{Waiting: func(Wait) { t.Error("a job that fits waited") }}); err != nil {
		t.Fatalf("Admit = %v, want it admitted", err)
	}
	if err := b.Admit(ctx, "b", nil, 1, Observer{Waiting: func(Wait) {}}); !errors.Is(err, context.Canceled) {
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
	if err := b.Admit(t.Context(), "huge", nil, 4097, Observer{Waiting: func(Wait) { t.Error("a job that never fits waited") }}); err == nil {
		t.Error("Admit of a job that never fits succeeded")
	}
}

// A job admitted twice is the caller's bug, and is refused, holding
// nothing more.
func TestAJobIsAdmittedOnce(t *testing.T) {
	t.Parallel()
	b := New(4096)
	if err := b.Admit(t.Context(), "a", nil, 1024, Observer{}); err != nil {
		t.Fatal(err)
	}
	if err := b.Admit(t.Context(), "a", nil, 1024, Observer{}); err == nil {
		t.Error("a second Admit of one job succeeded")
	}
	if err := b.Admit(t.Context(), "b", nil, 3072, Observer{Waiting: func(Wait) { t.Error("b waited: a holds more than its need") }}); err != nil {
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
					if err := q.b.Admit(ctx, job, nil, need, Observer{Waiting: waiting}); err != nil {
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

// ro and rw are the disk keyed k, read-only or read-write, at /disk<k>.
func ro(k uint64) Disk { return Disk{Key: DiskKey{Dev: 1, Ino: k}, Path: fmt.Sprintf("/disk%d", k)} }

func rw(k uint64) Disk {
	d := ro(k)
	d.ReadWrite = true
	return d
}

// waitsOf returns job's waits so far.
func (q *queue) waitsOf(job string) []Wait {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.waits[job])
}

// Writers to one disk are admitted one at a time, in arrival order, and
// each disk wait names the disk, its holders and how many are ahead.
func TestWritersTakeADiskOneAtATime(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := newQueue(t, 0)
		defer q.end()
		for _, job := range []string{"a", "b", "c", "d"} {
			q.admitDisks(job, 0, rw(1))
		}
		q.want("a")
		d := rw(1)
		for job, ahead := range map[string]int{"b": 0, "c": 1, "d": 2} {
			want := []Wait{{Disk: &d, Holders: []string{"a"}, Ahead: ahead}}
			if got := q.waitsOf(job); !reflect.DeepEqual(got, want) {
				t.Errorf("%s's waits = %+v, want %+v", job, got, want)
			}
		}
		for i, job := range []string{"a", "b", "c"} {
			q.release(job)
			q.want([]string{"a", "b", "c", "d"}[:i+2]...)
		}
		q.release("d")
	})
}

// Readers share a disk; a writer waits for every earlier reader; a reader
// behind a waiting writer waits for it, though the disk is only read, and
// the readers behind the writer are admitted together once it leaves.
func TestReadersShareADiskAndWritersWaitForThem(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := newQueue(t, 0)
		defer q.end()
		q.admitDisks("r1", 0, ro(1))
		q.admitDisks("r2", 0, ro(1))
		q.want("r1", "r2")
		q.admitDisks("w", 0, rw(1))
		q.admitDisks("r3", 0, ro(1))
		q.admitDisks("r4", 0, ro(1))
		q.want("r1", "r2")
		d := ro(1)
		if got, want := q.waitsOf("r3"), []Wait{{Disk: &d, Holders: []string{"r1", "r2"}, Ahead: 1}}; !reflect.DeepEqual(got, want) {
			t.Errorf("r3's waits = %+v, want %+v", got, want)
		}

		q.release("r1")
		q.want("r1", "r2")
		q.release("r2")
		q.want("r1", "r2", "w")
		q.release("w")
		q.wantAll("r1", "r2", "w", "r3", "r4")
		q.release("r3")
		q.release("r4")
	})
}

// Jobs needing disks {A, B} and {B, A}, many at once, all finish: each
// takes its disks in key order, whatever order its profile lists them in
// (QUE-5). A deadlock would block every goroutine, which synctest reports.
func TestOppositeDiskOrdersNeverDeadlock(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := New(0)
		var wg sync.WaitGroup
		for i := range 200 {
			disks := []Disk{rw(1), rw(2)}
			if i%2 == 1 {
				disks = []Disk{rw(2), rw(1)}
			}
			job := fmt.Sprint(i)
			wg.Go(func() {
				if err := b.Admit(t.Context(), job, disks, 0, Observer{}); err != nil {
					t.Errorf("%s: %v", job, err)
					return
				}
				time.Sleep(time.Second)
				b.Release(job)
			})
		}
		wg.Wait()
		if len(b.disks) != 0 || len(b.jobs) != 0 {
			t.Errorf("after every job is released, %d disk queues and %d jobs are left", len(b.disks), len(b.jobs))
		}
	})
}

// A job waiting for a disk holds no memory: a later job that needs all
// the memory left is admitted at once. A job holding its disks and waiting
// for memory is admitted when memory frees.
func TestADiskWaitHoldsNoMemory(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := newQueue(t, 1024)
		defer q.end()
		q.admitDisks("a", 512, rw(1))
		q.admitDisks("b", 512, rw(1))
		q.admit("c", 512)
		q.want("a", "c")
		if w := q.waitsOf("b"); len(w) != 1 || w[0].Disk == nil {
			t.Errorf("b's waits = %+v, want one for its disk", w)
		}

		// a frees its disk and its memory: b takes both.
		q.release("a")
		q.want("a", "c", "b")
		// e takes its disk, then waits for memory, holding the disk.
		q.admitDisks("e", 1024, rw(3))
		q.admitDisks("f", 0, rw(3))
		q.want("a", "c", "b")
		w := q.waitsOf("e")
		if len(w) != 1 || w[0].Disk != nil || w[0].NeedMiB != 1024 || w[0].FreeMiB != 0 || w[0].Ahead != 0 {
			t.Errorf("e's waits = %+v, want one for 1024 MiB with none free", w)
		}
		if w := q.waitsOf("f"); len(w) != 1 || !reflect.DeepEqual(w[0].Holders, []string{"e"}) {
			t.Errorf("f's waits = %+v, want one for the disk e holds", w)
		}
		q.release("c")
		q.want("a", "c", "b")
		q.release("b")
		q.want("a", "c", "b", "e")
		q.release("e")
		q.want("a", "c", "b", "e", "f")
		q.release("f")
	})
}

// A cancelled wait gives back the disks it already took and leaves the
// queue it waits in: a job behind it moves up, and the disk it took is
// free at once. Its wait ends with its context's error.
func TestACancelledWaitGivesBackItsDisks(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := newQueue(t, 0)
		defer q.end()
		q.admitDisks("x", 0, rw(2))
		q.admitDisks("y", 0, rw(2), rw(1))
		q.admitDisks("z", 0, rw(2))
		q.want("x")
		q.cancel("y")
		q.mu.Lock()
		if err := q.errs["y"]; !errors.Is(err, context.Canceled) {
			t.Errorf("the cancelled wait returned %v, want context.Canceled", err)
		}
		if ended := q.ended["y"]; len(ended) != 1 || !errors.Is(ended[0], context.Canceled) {
			t.Errorf("y's waits ended with %v, want one context.Canceled", ended)
		}
		q.mu.Unlock()
		// y took disk 1 before it waited for disk 2: it gave it back.
		q.admitDisks("v", 0, rw(1))
		q.want("x", "v")
		q.release("x")
		q.want("x", "v", "z")
		for _, job := range []string{"y", "v", "z"} {
			q.release(job)
		}
		if len(q.b.disks) != 0 || len(q.b.jobs) != 0 {
			t.Errorf("%d disk queues and %d jobs left", len(q.b.disks), len(q.b.jobs))
		}
	})
}

// A wait for memory that is cancelled gives back the disks the job holds.
func TestACancelledMemoryWaitGivesBackItsDisks(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := newQueue(t, 1024)
		defer q.end()
		q.admit("a", 1024)
		q.admitDisks("b", 1024, rw(1), ro(2))
		q.want("a")
		q.cancel("b")
		q.admitDisks("c", 0, rw(1), rw(2))
		q.want("a", "c")
		q.release("a")
		q.release("c")
	})
}

// Release is exactly once for a job's disks and memory together: a second
// release frees nothing more, and a release while the job's Admit still
// waits frees nothing.
func TestReleaseIsExactlyOnceForDisksAndMemory(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := newQueue(t, 1024)
		defer q.end()
		q.admitDisks("a", 1024, rw(1), ro(2))
		q.admitDisks("b", 512, ro(2))
		// b holds disk 2 and waits for memory; its release does nothing.
		q.release("b")
		q.admitDisks("w", 0, rw(2))
		q.want("a")

		q.release("a")
		q.want("a", "b")
		q.release("a")
		q.admitDisks("c", 1024, rw(1))
		// a's second release freed no memory: c waits for b's.
		q.want("a", "b")
		q.release("b")
		q.wantAll("a", "b", "w", "c")
		q.release("b")
		q.release("w")
		q.release("c")
		if len(q.b.disks) != 0 || len(q.b.jobs) != 0 {
			t.Errorf("%d disk queues and %d jobs left", len(q.b.disks), len(q.b.jobs))
		}
	})
}

// A disk listed twice is one entry, at its strongest mode: read-only and
// read-write together are a write.
func TestADiskListedTwiceIsOneEntry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		q := newQueue(t, 0)
		defer q.end()
		q.admitDisks("a", 0, ro(1), ro(1))
		q.admitDisks("b", 0, ro(1), rw(1))
		q.want("a")
		if w := q.waitsOf("b"); len(w) != 1 || !w[0].Disk.ReadWrite {
			t.Errorf("b's waits = %+v, want one, writing", w)
		}
		q.release("a")
		q.want("a", "b")
		q.release("b")
	})
}
