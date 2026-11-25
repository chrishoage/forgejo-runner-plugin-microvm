// SPDX-License-Identifier: GPL-3.0-or-later

// Package admission owns the plugin's memory budget and each job's
// admission to it (QUE-1, QUE-2, QUE-7): a job takes what it needs before
// its VMs boot, waiting in arrival order while it can't have it, and gives
// it back exactly once. It takes plain inputs, a job id and MiB, and imports
// no other package of the plugin's.
package admission

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"golang.org/x/sync/semaphore"
)

// Budget is the memory budget: a weighted semaphore of its cap in MiB, which
// each admitted job holds its need of until it is released, and a waiting
// line of the jobs that can't have theirs yet, in the order Admit recorded
// them. Only the line's head is ever admitted from it, and a job admitted at
// once only when the line is empty, so a job that would fit never overtakes
// an earlier one that doesn't fit yet, and a big job can't starve. With no
// cap, every job is admitted at once and nothing is held.
type Budget struct {
	capMiB uint64
	// sem holds the admitted jobs' MiB. It is only tried, under mu, never
	// waited on: its own queue is joined only inside Acquire, after Admit
	// has let go of mu, so it can't keep the order the line records.
	sem *semaphore.Weighted

	// mu guards the semaphore's tries, the line, what the log lines read,
	// and the holders, which make each job's release happen once.
	mu      sync.Mutex
	held    map[string]uint64
	usedMiB uint64
	line    []*waiter
}

// waiter is a job in the waiting line. admitted is closed once it holds
// its need; mu guards it.
type waiter struct {
	job      string
	needMiB  uint64
	admitted chan struct{}
}

// New returns a budget of capMiB, or one with no cap for 0.
func New(capMiB uint64) *Budget {
	b := &Budget{capMiB: capMiB, held: map[string]uint64{}}
	if capMiB > 0 {
		b.sem = semaphore.NewWeighted(int64(capMiB)) //nolint:gosec // G115: the config bounds max_memory_mib far below MaxInt64
	}
	return b
}

// Cap returns the budget's cap in MiB, 0 for none.
func (b *Budget) Cap() uint64 { return b.capMiB }

// Fits reports whether a job needing needMiB fits the cap on its own: with
// no cap, every job does. One that doesn't would wait forever.
func (b *Budget) Fits(needMiB uint64) bool { return b.sem == nil || needMiB <= b.capMiB }

// Wait is a snapshot of a job's wait as it begins, for its log lines: its
// need, what is free, the cap, and how many jobs wait ahead of it.
type Wait struct {
	NeedMiB, FreeMiB, CapMiB uint64
	Ahead                    int
}

// Admit admits the job, needing needMiB, to the budget: at once when it fits
// and no job waits, even when ctx has ended; otherwise it joins the end of
// the waiting line, which is its arrival, and waits until it is the line's
// head and fits. It calls waiting first, with a snapshot of the wait, in a
// goroutine of its own, while the job already holds its place in the line,
// and returns only once waiting has returned: waiting must return soon once
// ctx ends. A wait ends early only with ctx, and Admit then returns ctx's
// cause, holding nothing: ctx wins over an admission that came while
// waiting still ran, which is given back. A job that never fits, or that is
// already admitted or waiting, is refused at once.
func (b *Budget) Admit(ctx context.Context, job string, needMiB uint64, waiting func(Wait)) error {
	if b.sem == nil {
		return nil
	}
	if !b.Fits(needMiB) {
		return fmt.Errorf("job %s needs %d MiB, more than the memory budget's %d MiB", job, needMiB, b.capMiB)
	}
	b.mu.Lock()
	if _, ok := b.held[job]; ok || b.inLine(job) {
		b.mu.Unlock()
		return fmt.Errorf("job %s is already admitted or waiting", job)
	}
	if len(b.line) == 0 && b.take(job, needMiB) {
		b.mu.Unlock()
		return nil
	}
	if ctx.Err() != nil {
		b.mu.Unlock()
		return context.Cause(ctx)
	}
	w := &waiter{job: job, needMiB: needMiB, admitted: make(chan struct{})}
	snap := Wait{NeedMiB: needMiB, FreeMiB: b.capMiB - b.usedMiB, CapMiB: b.capMiB, Ahead: len(b.line)}
	b.line = append(b.line, w)
	b.mu.Unlock()

	// waiting may block on the job's stream; the job keeps its place
	// meanwhile, and may be admitted.
	told := make(chan struct{})
	go func() {
		defer close(told)
		if waiting != nil {
			waiting(snap)
		}
	}()
	select {
	case <-w.admitted:
	case <-ctx.Done():
	}
	<-told

	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-w.admitted:
		if ctx.Err() == nil {
			return nil
		}
		// ctx ended before Admit could return the admission, perhaps
		// while waiting still ran: the wait fails, so it holds nothing.
		b.release(job)
		return context.Cause(ctx)
	default:
	}
	i := slices.Index(b.line, w)
	b.line = slices.Delete(b.line, i, i+1)
	if i == 0 {
		// The jobs behind it move up, and the new head may fit.
		b.admitHeads()
	}
	return context.Cause(ctx)
}

// inLine reports whether job waits in the line. b.mu is held.
func (b *Budget) inLine(job string) bool {
	return slices.ContainsFunc(b.line, func(w *waiter) bool { return w.job == job })
}

// take admits job, needing needMiB, if it fits what is free, and reports
// whether it did. b.mu is held.
func (b *Budget) take(job string, needMiB uint64) bool {
	if !b.sem.TryAcquire(int64(needMiB)) { //nolint:gosec // G115: bounded by capMiB at Admit
		return false
	}
	b.held[job] = needMiB
	b.usedMiB += needMiB
	return true
}

// admitHeads admits the line's head while it fits, in order. b.mu is held.
func (b *Budget) admitHeads() {
	for len(b.line) > 0 && b.take(b.line[0].job, b.line[0].needMiB) {
		close(b.line[0].admitted)
		b.line = slices.Delete(b.line, 0, 1)
	}
}

// Release gives back what job holds, once, and admits the jobs at the head
// of the line that then fit: a job not admitted, or already released,
// changes nothing.
func (b *Budget) Release(job string) {
	if b.sem == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.release(job)
}

// release gives back what job holds, if anything, and admits the heads that
// then fit. b.mu is held.
func (b *Budget) release(job string) {
	need, ok := b.held[job]
	if !ok {
		return
	}
	delete(b.held, job)
	b.usedMiB -= need
	b.sem.Release(int64(need)) //nolint:gosec // G115: bounded by capMiB at Admit
	b.admitHeads()
}
