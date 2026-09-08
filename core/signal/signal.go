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
// Release is automatic too, inside a Scope. The wasm runtime opens one around
// a page's Mount and disposes it when the page leaves, so an effect created in
// Mount needs no bookkeeping — there is no stop func to keep and no Unmount to
// call it from. Outside a scope the stop func is still returned, for the
// registrations that should live as long as the process.
//
// Concurrency: the browser is single-threaded, which is the environment this is
// written for. The mutex keeps the structures safe if the same code is linked
// into the server, but `current` and `scope` are process-wide — an effect must
// not be run from two goroutines at once. On the server nothing constructs
// signals.
package signal

import (
	"sort"
	"sync"
)

var (
	mu      sync.Mutex
	current *effect // the computation being tracked, nil when not tracking
	owner   *scope  // the scope collecting cleanups, nil when none is open
	nextID  uint64  // creation order, which is also the flush order

	// Writes do not run effects directly; they queue them, and the queue is
	// drained once the outermost write or Batch returns. That is what makes
	// two Sets in one handler cost one repaint, and it is what keeps a
	// diamond — an effect reading both a signal and a value derived from it —
	// from running twice per change.
	depth   int       // >0 while a Batch or a flush is running
	pending []*effect // queued, unordered; sorted by id at flush
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

// Set writes the value and wakes dependents. Inside a Batch, or inside another
// effect's run, the dependents are queued and run when the outermost write
// completes; a bare Set is a batch of one.
func (s *Signal[T]) Set(v T) {
	mu.Lock()
	if s.eq != nil && s.eq(s.v, v) {
		mu.Unlock()
		return
	}
	s.v = v
	for e := range s.subs {
		if !e.queued && !e.stopped {
			e.queued = true
			pending = append(pending, e)
		}
	}
	mu.Unlock()
	flush()
}

// Update applies fn to the current value. Convenience for read-modify-write.
func (s *Signal[T]) Update(fn func(T) T) { s.Set(fn(s.Peek())) }

func (s *Signal[T]) removeSub(e *effect) { delete(s.subs, e) } // caller holds mu

// ---------------------------------------------------------------------------
// Batch
// ---------------------------------------------------------------------------

// Batch runs fn with effects deferred: however many signals fn writes, each
// dependent runs once, after fn returns. A handler that sets three signals
// without it repaints three times.
func Batch(fn func()) {
	mu.Lock()
	depth++
	mu.Unlock()
	defer func() {
		mu.Lock()
		depth--
		mu.Unlock()
		flush()
	}()
	fn()
}

// flush drains the queue, lowest id first. A computed value is necessarily
// created before anything that reads it, so creation order is a topological
// order: the computed recomputes, re-queues its readers (already queued, so a
// no-op), and each reader runs once with every input current.
//
// Each run happens at depth 1, so a Set made inside an effect queues rather
// than recursing — the loop picks it up on the next iteration.
func flush() {
	for {
		mu.Lock()
		if depth > 0 || len(pending) == 0 {
			mu.Unlock()
			return
		}
		sort.Slice(pending, func(i, j int) bool { return pending[i].id < pending[j].id })
		e := pending[0]
		pending = pending[1:]
		e.queued = false
		depth++
		mu.Unlock()

		e.run()

		mu.Lock()
		depth--
		mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// Effect
// ---------------------------------------------------------------------------

type effect struct {
	id      uint64
	fn      func()
	deps    []dep
	stopped bool
	queued  bool
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
//
// Inside a Scope — which is where a page's Mount runs — the effect is released
// with the scope, and the returned stop func can be ignored. Outside one it is
// the only way to detach; calling it twice is safe.
//
// Watching several values is just reading several signals inside one effect —
// tracking is automatic, so there is no list of sources to declare.
func Effect(fn func()) (stop func()) {
	mu.Lock()
	nextID++
	e := &effect{id: nextID, fn: fn}
	mu.Unlock()
	e.run()

	var once sync.Once
	stop = func() {
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
	OnCleanup(stop)
	return stop
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
// Scope
// ---------------------------------------------------------------------------

// scope collects everything registered while it is open, so one dispose
// releases all of it. It is the reason a page has no release slice.
type scope struct {
	cleanups []func()
	disposed bool
}

// Scope runs fn and returns the func that releases everything fn registered:
// every Effect, Watch and Derive, and every dom listener, in reverse order.
// Scopes nest; an inner one disposed by hand is simply not disposed again.
//
// The wasm runtime wraps a page's Mount in one and disposes it when the page
// is swapped out — see dom.Mount. Application code only needs Scope for a
// lifetime that is not a page's.
func Scope(fn func()) (dispose func()) {
	s := &scope{}
	mu.Lock()
	prev := owner
	owner = s
	mu.Unlock()

	fn()

	mu.Lock()
	owner = prev
	mu.Unlock()

	return func() {
		mu.Lock()
		if s.disposed {
			mu.Unlock()
			return
		}
		s.disposed = true
		cleanups := s.cleanups
		s.cleanups = nil
		mu.Unlock()
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
}

// OnCleanup registers fn with the open scope, to run when it is disposed. With
// no scope open it does nothing — the registration then lives as long as the
// process, which is right for an app-shell control and wrong for a page.
func OnCleanup(fn func()) {
	mu.Lock()
	defer mu.Unlock()
	if owner != nil && !owner.disposed {
		owner.cleanups = append(owner.cleanups, fn)
	}
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
//	signal.Watch(
//	    func() string { return store.Article.Get().Title },
//	    func(now, before string) { … },
//	)
//
// cb runs untracked, so signals it reads do not silently become dependencies
// of the watcher. To watch several values, read them all in one Effect.
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
