// SPDX-License-Identifier: GPL-3.0-or-later

// Package admission owns the plugin's memory budget, its disk queues and
// each job's admission to them (QUE-1 to QUE-5, QUE-7): a job takes its
// disks and its memory before its VMs boot, waiting in arrival order while
// it can't have them, and gives them back exactly once. It takes plain
// inputs, a job id, disks as their keys, paths and modes, and MiB, and
// imports no other package of the plugin's.
package admission

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

// diskSize is the size of a disk queue's semaphore: a reader takes 1 and a
// writer all of it, so readers share a disk and a writer has it alone. It
// bounds readers only in theory.
const diskSize = math.MaxInt32

// Admitter is the memory budget and the disk queues, and the admission of
// each job to them. A job takes its disks one at a time in sorted key
// order, holding each while it waits for the next, then its memory last: the
// total order rules out deadlock (QUE-5), and memory last means a job
// waiting for a disk holds none of it (QUE-4).
type Admitter struct {
	capMiB uint64

	// mu guards every line, the disk queues' table and the jobs.
	mu sync.Mutex
	// memory is the memory budget, nil with no cap.
	memory *line
	// disks are the disk queues, by key; a queue nobody holds or waits in
	// is dropped, so the table holds only disks in use.
	disks map[DiskKey]*line
	// jobs are the jobs admitted or being admitted, by id, and what each
	// holds, which makes each job's release happen once.
	jobs map[string]*holding
}

// holding is what a job holds: the lines it took, in order. admitting is
// set while its Admit runs, which owns it until then.
type holding struct {
	admitting bool
	lines     []*line
}

// New returns an admitter whose memory budget is capMiB, or has no cap for
// 0.
func New(capMiB uint64) *Admitter {
	a := &Admitter{capMiB: capMiB, disks: map[DiskKey]*line{}, jobs: map[string]*holding{}}
	if capMiB > 0 {
		a.memory = newLine(int64(capMiB), nil) //nolint:gosec // G115: the config bounds max_memory_mib far below MaxInt64
	}
	return a
}

// Cap returns the memory budget's cap in MiB, 0 for none.
func (a *Admitter) Cap() uint64 { return a.capMiB }

// Fits reports whether a job needing needMiB fits the memory budget's cap on
// its own: with no cap, every job does. One that doesn't would wait forever.
func (a *Admitter) Fits(needMiB uint64) bool { return a.memory == nil || needMiB <= a.capMiB }

// DiskKey names a disk by its source file's device and inode, so two paths
// to one file share its queue, as the flock they'd meet does.
type DiskKey struct{ Dev, Ino uint64 }

// Disk is one disk a job needs: its key, its path for the log, and whether
// the job writes it.
type Disk struct {
	Key       DiskKey
	Path      string
	ReadWrite bool
}

// Wait is a snapshot of one of a job's waits as it begins, for its log
// lines. A disk wait names the disk, the jobs holding it, sorted, and how
// many jobs wait ahead of it; a memory wait, whose Disk is nil, the job's
// need, what is free, the cap and how many jobs wait ahead.
type Wait struct {
	Disk                     *Disk
	Holders                  []string
	NeedMiB, FreeMiB, CapMiB uint64
	Ahead                    int
}

// Observer is told of each of a job's waits. Waiting runs as a wait begins,
// in a goroutine of its own, while the job already holds its place in the
// line, and must return soon once the wait's context ends. Waited runs once
// Waiting has returned and the wait has ended, with its length, and nil when
// the job got what it waited for, or the error that ended the wait. Either
// may be nil.
type Observer struct {
	Waiting func(Wait)
	Waited  func(w Wait, took time.Duration, err error)
}

// Admit admits job to its disks, then to needMiB of the memory budget. It
// takes each disk, and then the memory, at once when it is free for the job
// and no job waits for it, even when ctx has ended; otherwise the job joins
// the end of that line, which is its arrival there, and waits until it is
// at the line's head and what it needs is free. A disk listed twice is one
// entry, at its strongest mode. A wait ends early only with ctx, and Admit
// then returns ctx's cause, having given back everything it took: ctx wins
// over an admission that came while Waiting still ran. A job that never
// fits the memory budget, or that is already admitted or being admitted,
// is refused at once.
func (a *Admitter) Admit(ctx context.Context, job string, disks []Disk, needMiB uint64, obs Observer) error {
	if !a.Fits(needMiB) {
		return fmt.Errorf("job %s needs %d MiB, more than the memory budget's %d MiB", job, needMiB, a.capMiB)
	}
	a.mu.Lock()
	if _, ok := a.jobs[job]; ok {
		a.mu.Unlock()
		return fmt.Errorf("job %s is already admitted or waiting", job)
	}
	h := &holding{admitting: true}
	a.jobs[job] = h
	a.mu.Unlock()

	for _, d := range diskOrder(disks) {
		weight := int64(1)
		if d.ReadWrite {
			weight = diskSize
		}
		get := func() *line {
			l, ok := a.disks[d.Key]
			if !ok {
				k := d.Key
				l = newLine(diskSize, &k)
				a.disks[d.Key] = l
			}
			return l
		}
		snap := func(l *line, ahead int) Wait {
			return Wait{Disk: &d, Holders: l.holders(), Ahead: ahead}
		}
		if err := a.take(ctx, job, h, get, weight, snap, obs); err != nil {
			a.giveBack(job)
			return err
		}
	}
	if a.memory != nil {
		get := func() *line { return a.memory }
		snap := func(l *line, ahead int) Wait {
			return Wait{NeedMiB: needMiB, FreeMiB: uint64(l.size - l.used), CapMiB: a.capMiB, Ahead: ahead} //nolint:gosec // G115: used never exceeds size
		}
		if err := a.take(ctx, job, h, get, int64(needMiB), snap, obs); err != nil { //nolint:gosec // G115: bounded by capMiB above
			a.giveBack(job)
			return err
		}
	}
	a.mu.Lock()
	h.admitting = false
	a.mu.Unlock()
	return nil
}

// diskOrder returns disks with each key once, at its strongest mode and
// its first path, sorted by key.
func diskOrder(disks []Disk) []Disk {
	var ds []Disk
	for _, d := range disks {
		if i := slices.IndexFunc(ds, func(e Disk) bool { return e.Key == d.Key }); i >= 0 {
			ds[i].ReadWrite = ds[i].ReadWrite || d.ReadWrite
			continue
		}
		ds = append(ds, d)
	}
	slices.SortFunc(ds, func(x, y Disk) int {
		return cmp.Or(cmp.Compare(x.Key.Dev, y.Key.Dev), cmp.Compare(x.Key.Ino, y.Key.Ino))
	})
	return ds
}

// take has job, being admitted with h, take weight of the line get returns
// under a.mu, at once or after a wait, as Admit describes; snap describes
// the wait, under a.mu, with how many wait ahead. Once it is taken, h
// records it, also when ctx ended meanwhile and take fails, so giving h
// back releases it.
func (a *Admitter) take(ctx context.Context, job string, h *holding, get func() *line, weight int64,
	snap func(l *line, ahead int) Wait, obs Observer,
) error {
	a.mu.Lock()
	l := get()
	if len(l.waiters) == 0 && l.take(job, weight) {
		h.lines = append(h.lines, l)
		a.mu.Unlock()
		return nil
	}
	if ctx.Err() != nil {
		a.dropIfIdle(l)
		a.mu.Unlock()
		return context.Cause(ctx)
	}
	w := &waiter{job: job, weight: weight, admitted: make(chan struct{})}
	s := snap(l, len(l.waiters))
	l.waiters = append(l.waiters, w)
	a.mu.Unlock()

	begin := time.Now()
	// Waiting may block on the job's stream; the job keeps its place
	// meanwhile, and may be admitted.
	told := make(chan struct{})
	go func() {
		defer close(told)
		if obs.Waiting != nil {
			obs.Waiting(s)
		}
	}()
	select {
	case <-w.admitted:
	case <-ctx.Done():
	}
	<-told

	a.mu.Lock()
	var err error
	select {
	case <-w.admitted:
		h.lines = append(h.lines, l)
		// ctx may have ended before the admission could be returned,
		// perhaps while Waiting still ran: the wait fails then, and
		// Admit gives back what it took.
		if ctx.Err() != nil {
			err = context.Cause(ctx)
		}
	default:
		i := slices.Index(l.waiters, w)
		l.waiters = slices.Delete(l.waiters, i, i+1)
		if i == 0 {
			// The jobs behind it move up, and the new head may fit.
			l.admitHeads()
		}
		a.dropIfIdle(l)
		err = context.Cause(ctx)
	}
	a.mu.Unlock()
	if obs.Waited != nil {
		obs.Waited(s, time.Since(begin), err)
	}
	return err
}

// Release gives back everything job holds, once, and admits the jobs at
// the head of each line that then fit: a job not admitted, already
// released, or whose Admit still runs, changes nothing.
func (a *Admitter) Release(job string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if h, ok := a.jobs[job]; ok && !h.admitting {
		a.release(job, h)
	}
}

// giveBack releases what job, whose Admit failed, took. a.mu is not held.
func (a *Admitter) giveBack(job string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.release(job, a.jobs[job])
}

// release gives back every line job holds by h, and forgets job. a.mu is
// held.
func (a *Admitter) release(job string, h *holding) {
	for _, l := range h.lines {
		l.release(job)
		a.dropIfIdle(l)
	}
	delete(a.jobs, job)
}

// dropIfIdle drops l from the disk queues' table when it is a disk queue
// nobody holds or waits in: the next job to want the disk makes it anew.
// a.mu is held.
func (a *Admitter) dropIfIdle(l *line) {
	if l.disk != nil && len(l.held) == 0 && len(l.waiters) == 0 {
		delete(a.disks, *l.disk)
	}
}

// line is a weighted semaphore of size and a waiting line of the jobs that
// can't take their weight of it yet, in the order they joined it. Only the
// line's head is ever admitted from it, and a job takes its weight at once
// only when the line is empty, so a job that would fit never overtakes an
// earlier one that doesn't fit yet, and a heavy job, such as a writer, can't
// starve. The semaphore is only ever tried, never waited on: its own queue
// is joined only inside Acquire, after the caller has let go of the lock
// guarding the line, so it can't keep the order the line records. The
// Admitter's mu guards every line.
type line struct {
	size int64
	// disk is the disk queue's key, nil for the memory budget.
	disk *DiskKey
	sem  *semaphore.Weighted
	used int64
	// held is what each job holding the line holds.
	held    map[string]int64
	waiters []*waiter
}

// waiter is a job in a line. admitted is closed once it holds its weight.
type waiter struct {
	job      string
	weight   int64
	admitted chan struct{}
}

func newLine(size int64, disk *DiskKey) *line {
	return &line{size: size, disk: disk, sem: semaphore.NewWeighted(size), held: map[string]int64{}}
}

// take gives job weight of l if it fits what is free, and reports whether
// it did.
func (l *line) take(job string, weight int64) bool {
	if !l.sem.TryAcquire(weight) {
		return false
	}
	l.held[job] = weight
	l.used += weight
	return true
}

// admitHeads admits l's head while it fits, in order.
func (l *line) admitHeads() {
	for len(l.waiters) > 0 && l.take(l.waiters[0].job, l.waiters[0].weight) {
		close(l.waiters[0].admitted)
		l.waiters = slices.Delete(l.waiters, 0, 1)
	}
}

// release gives back what job holds of l, if anything, and admits the heads
// that then fit.
func (l *line) release(job string) {
	weight, ok := l.held[job]
	if !ok {
		return
	}
	delete(l.held, job)
	l.used -= weight
	l.sem.Release(weight)
	l.admitHeads()
}

// holders returns the jobs holding l, sorted.
func (l *line) holders() []string {
	hs := make([]string, 0, len(l.held))
	for job := range l.held {
		hs = append(hs, job)
	}
	slices.Sort(hs)
	return hs
}
