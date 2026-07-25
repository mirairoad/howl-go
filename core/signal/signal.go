// Package signal is fine-grained reactivity: signals, computed values, effects
// and watchers, in the shape Vue's `ref`/`computed`/`watch` and Preact's
// signals popularised.
//
// The problem it replaces: a single "something changed" callback re-runs every
// subscriber for every mutation. With dependency tracking, a computation
// records exactly which signals it read while running, and only those signals
// can wake it again.
//
// Tracking is automatic. While an effect runs it is installed as the current
// computation; every Signal.Get() during that window registers the dependency
// both ways. Re-running an effect first detaches all of its old dependencies,
// so a branch that stops reading a signal stops being woken by it.
//
// Concurrency: the browser is single-threaded, which is the environment this is
// written for. The mutex keeps the structures safe if the same code is linked
// into the server, but `current` is process-wide — an effect must not be run
// from two goroutines at once. On the server nothing constructs signals.
package signal

import "sync"

var (
	mu      sync.Mutex
	current *effect // the computation being tracked, nil when not tracking
)

// dep is the signal side of the dependency edge, type-erased so an effect can
// hold signals of differing element types.
type dep interface{ removeSub(*effect) }

// ---------------------------------------------------------------------------
// Signal
// ---------------------------------------------------------------------------

type Signal[T any] struct {
	v    T
	eq   func(a, b T) bool // nil means "always treat a Set as a change"
	subs map[*effect]struct{}
}

// New creates a signal for any type. Every Set notifies, because an arbitrary T
// cannot be compared — use Of for comparable types to get change detection.
func New[T any](v T) *Signal[T] {
	return &Signal[T]{v: v, subs: map[*effect]struct{}{}}
}

// Of creates a signal that only notifies when the value actually differs.
func Of[T comparable](v T) *Signal[T] {
	s := New(v)
	s.eq = func(a, b T) bool { return a == b }
	return s
}

// WithEq creates a signal with a custom equality test — for slices, maps, or
// structs where == is unavailable or too strict.
func WithEq[T any](v T, eq func(a, b T) bool) *Signal[T] {
	s := New(v)
	s.eq = eq
	return s
}

// Get reads the value and, if a computation is running, subscribes it.
func (s *Signal[T]) Get() T {
	mu.Lock()
	if current != nil {
		if _, seen := s.subs[current]; !seen {
			s.subs[current] = struct{}{}
			current.deps = append(current.deps, s)
		}
	}
	v := s.v
	mu.Unlock()
	return v
}

// Peek reads without subscribing — for use inside an effect that must read a
// value without being woken by it.
func (s *Signal[T]) Peek() T {
	mu.Lock()
	defer mu.Unlock()
	return s.v
}

// Set writes the value and wakes dependents. Subscribers are copied out and the
// lock released before running them, since an effect will call Get again.
func (s *Signal[T]) Set(v T) {
	mu.Lock()
	if s.eq != nil && s.eq(s.v, v) {
		mu.Unlock()
		return
	}
	s.v = v
	woken := make([]*effect, 0, len(s.subs))
	for e := range s.subs {
		woken = append(woken, e)
	}
	mu.Unlock()

	for _, e := range woken {
		e.run()
	}
}

// Update applies fn to the current value. Convenience for read-modify-write.
func (s *Signal[T]) Update(fn func(T) T) { s.Set(fn(s.Peek())) }

func (s *Signal[T]) removeSub(e *effect) { delete(s.subs, e) } // caller holds mu

// ---------------------------------------------------------------------------
// Effect
// ---------------------------------------------------------------------------

type effect struct {
	fn      func()
	deps    []dep
	stopped bool
}

func (e *effect) run() {
	mu.Lock()
	if e.stopped {
		mu.Unlock()
		return
	}
	// Detach first: a run that no longer reads a signal must stop depending on
	// it, otherwise a conditional branch leaks a permanent subscription.
	for _, d := range e.deps {
		d.removeSub(e)
	}
	e.deps = e.deps[:0]
	prev := current
	current = e
	mu.Unlock()

	e.fn()

	mu.Lock()
	current = prev
	mu.Unlock()
}

// Effect runs fn immediately, then again whenever any signal it read changes.
// The returned function detaches it; calling it twice is safe.
//
// Watching several values is just reading several signals inside one effect —
// tracking is automatic, so there is no list of sources to declare.
func Effect(fn func()) (stop func()) {
	e := &effect{fn: fn}
	e.run()

	var once sync.Once
	return func() {
		once.Do(func() {
			mu.Lock()
			defer mu.Unlock()
			e.stopped = true
			for _, d := range e.deps {
				d.removeSub(e)
			}
			e.deps = nil
		})
	}
}

// Untrack runs fn without recording dependencies, so a callback can read
// signals it must not be woken by.
func Untrack(fn func()) {
	mu.Lock()
	prev := current
	current = nil
	mu.Unlock()

	fn()

	mu.Lock()
	current = prev
	mu.Unlock()
}

// ---------------------------------------------------------------------------
// Computed
// ---------------------------------------------------------------------------

// Computed is a value derived from other signals. It recomputes when its
// dependencies change and is itself a signal, so it composes.
type Computed[T any] struct {
	out  *Signal[T]
	stop func()
}

// Derive recomputes on every dependency change and always propagates.
func Derive[T any](fn func() T) *Computed[T] {
	var zero T
	c := &Computed[T]{out: New(zero)}
	c.stop = Effect(func() { c.out.Set(fn()) })
	return c
}

// DeriveEq is Derive for comparable types: if the recomputed value equals the
// previous one, nothing downstream is woken. This is what stops a change deep
// in a struct from invalidating every view that only reads one field of it.
func DeriveEq[T comparable](fn func() T) *Computed[T] {
	var zero T
	out := Of(zero)
	c := &Computed[T]{out: out}
	c.stop = Effect(func() { out.Set(fn()) })
	return c
}

func (c *Computed[T]) Get() T   { return c.out.Get() }
func (c *Computed[T]) Peek() T  { return c.out.Peek() }
func (c *Computed[T]) Dispose() { c.stop() }

// ---------------------------------------------------------------------------
// Watch
// ---------------------------------------------------------------------------

// Watch calls cb whenever the value produced by src changes, passing the new
// and previous values. Unlike Effect it does not fire on the initial run —
// matching Vue's `watch`, where the point is the transition, not the value.
//
//	stop := signal.Watch(
//	    func() string { return store.Article.Get().Title },
//	    func(now, before string) { … },
//	)
//
// cb runs untracked, so signals it reads do not silently become dependencies
// of the watcher.
func Watch[T comparable](src func() T, cb func(now, before T)) (stop func()) {
	first := true
	var prev T
	return Effect(func() {
		v := src()
		if first {
			first, prev = false, v
			return
		}
		if v == prev {
			return
		}
		before := prev
		prev = v
		Untrack(func() { cb(v, before) })
	})
}

// WatchImmediate is Watch that also fires once on registration, with the
// zero value as `before`.
func WatchImmediate[T comparable](src func() T, cb func(now, before T)) (stop func()) {
	first := true
	var prev T
	return Effect(func() {
		v := src()
		if !first && v == prev {
			return
		}
		before := prev
		first, prev = false, v
		Untrack(func() { cb(v, before) })
	})
}

// ---------------------------------------------------------------------------
// Multiple sources
// ---------------------------------------------------------------------------

// WatchAny watches several sources at once and calls cb when any of them
// changes. Sources may be of different types.
//
//	signal.WatchAny(func() { … },
//	    func() any { return store.Article.Get().Title },
//	    func() any { return store.TodoCount.Get() },
//	)
//
// Note this is NOT the same idea as React's useEffect(fn, [a, b, c]). React
// requires that list because JavaScript cannot observe reads — it has no way to
// know what the function looked at, so you declare it, and a wrong declaration
// is the classic stale-closure bug. Here reads ARE observed, so the idiomatic
// form needs no list at all:
//
//	signal.Effect(func() {
//	    title := store.Article.Get().Title   // both become dependencies
//	    n := store.TodoCount.Get()           // simply by being read
//	    …
//	})
//
// Prefer Effect. WatchAny exists for when you want the sources named
// explicitly, or want cb to skip the initial run.
func WatchAny(cb func(), srcs ...func() any) (stop func()) {
	first := true
	prev := make([]any, len(srcs))
	return Effect(func() {
		now := make([]any, len(srcs))
		for i, src := range srcs {
			now[i] = src()
		}
		if first {
			first = false
			copy(prev, now)
			return
		}
		dirty := false
		for i := range now {
			if differs(prev[i], now[i]) {
				dirty = true
				break
			}
		}
		copy(prev, now)
		if dirty {
			Untrack(cb)
		}
	})
}

// differs compares two dynamically-typed values. Comparing uncomparable types
// (slices, maps, funcs) panics in Go, so those are treated as always-changed
// rather than crashing a watcher.
func differs(a, b any) (d bool) {
	defer func() {
		if recover() != nil {
			d = true
		}
	}()
	return a != b
}
