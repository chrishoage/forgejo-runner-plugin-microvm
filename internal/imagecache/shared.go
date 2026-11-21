// SPDX-License-Identifier: GPL-3.0-or-later

package imagecache

import (
	"context"
	"fmt"
	"sync"

	"golang.org/x/sync/singleflight"
)

// sharedCalls runs one call per key for every caller that asks for that key
// while it runs: one pull per layer, one lookup per reference
// (docs/architecture.md, Image cache). singleflight runs the call and hands
// its result to every caller; sharedCalls adds what a shared pull needs on
// top:
//
//   - The call runs in a context of its own, not any one caller's, so a
//     cancelled caller can't fail the others.
//   - It counts the callers holding it. A cancelled caller returns its own
//     context's error at once, and the call is cancelled only when its last
//     caller leaves; a later caller then starts a call of its own.
//   - A result is held until every caller that got it releases it, then
//     cleaned up: a pulled layer's temp file is deleted once every Acquire
//     that waited on it has stored it or failed.
type sharedCalls[T any] struct {
	g singleflight.Group
	// running counts calls whose fn hasn't returned, the abandoned ones
	// included.
	running *sync.WaitGroup
	// cleanup releases a successful result nobody holds any more; nil for
	// none.
	cleanup func(T)

	mu    sync.Mutex
	calls map[string]*sharedCall[T]
	// joined, when set, is called with a key and the number of callers
	// holding its call each time a caller joins: a test's view of it. Nil
	// outside tests.
	joined func(key string, refs int)
}

// sharedCall is one call of fn for a key.
type sharedCall[T any] struct {
	ctx    context.Context
	cancel context.CancelFunc
	// refs counts the callers waiting on the call or holding its result.
	refs int
	// done is set once fn has returned; val and err are its result.
	done bool
	val  T
	err  error
}

func newSharedCalls[T any](running *sync.WaitGroup, cleanup func(T)) *sharedCalls[T] {
	return &sharedCalls[T]{running: running, cleanup: cleanup, calls: map[string]*sharedCall[T]{}}
}

// do returns key's result: the result of the call running for key, or of a
// new one running fn. It returns once the call ends or ctx is done, then with
// ctx's error. The caller calls release once it no longer needs the result,
// whatever do returned.
func (s *sharedCalls[T]) do(ctx context.Context, key string, fn func(context.Context) (T, error)) (val T, release func(), err error) {
	s.mu.Lock()
	sc := s.calls[key]
	if sc == nil {
		// Not the caller's context: the call ends only with its last
		// caller.
		cctx, cancel := context.WithCancel(context.Background()) //nolint:gosec // G118: leave cancels it, with the last caller
		sc = &sharedCall[T]{ctx: cctx, cancel: cancel}
		s.calls[key] = sc
		s.running.Add(1)
	}
	sc.refs++
	refs, hook := sc.refs, s.joined
	// singleflight joins the call running for key; only a new sharedCall's
	// fn runs. A call that has returned is no longer singleflight's, and
	// its result is read here instead.
	var ch <-chan singleflight.Result
	if !sc.done {
		ch = s.g.DoChan(key, func() (any, error) {
			defer s.running.Done()
			v, err := protect(sc.ctx, fn)
			s.finish(sc, v, err)
			return v, err
		})
	}
	val, err = sc.val, sc.err
	s.mu.Unlock()
	if hook != nil {
		hook(key, refs)
	}
	var once sync.Once
	release = func() { once.Do(func() { s.leave(key, sc) }) }
	if ch == nil {
		return val, release, err
	}
	select {
	case r := <-ch:
		v, _ := r.Val.(T)
		return v, release, r.Err
	case <-ctx.Done():
		release()
		var zero T
		return zero, func() {}, ctx.Err()
	}
}

// finish records fn's result, and cleans it up when every caller has left
// already.
func (s *sharedCalls[T]) finish(sc *sharedCall[T], v T, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc.done, sc.val, sc.err = true, v, err
	if sc.refs == 0 {
		s.drop(sc)
	}
}

// leave lets go of key's call for one caller. The last one cancels the call
// if it still runs, or cleans its result up if it has one.
func (s *sharedCalls[T]) leave(key string, sc *sharedCall[T]) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sc.refs--; sc.refs > 0 {
		return
	}
	// A later caller starts a call of its own, even while this one still
	// unwinds: Forget keeps singleflight from handing it this call.
	delete(s.calls, key)
	s.g.Forget(key)
	sc.cancel()
	if sc.done {
		s.drop(sc)
	}
}

// drop cleans up the result of a call nobody holds.
func (s *sharedCalls[T]) drop(sc *sharedCall[T]) {
	if sc.err == nil && s.cleanup != nil {
		s.cleanup(sc.val)
	}
}

// protect runs fn, turning a panic into an error: singleflight re-panics in
// a goroutine of its own, which would end the plugin and every job (IMG-11).
func protect[T any](ctx context.Context, fn func(context.Context) (T, error)) (v T, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panicked: %v", p)
		}
	}()
	return fn(ctx)
}
