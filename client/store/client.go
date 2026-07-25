package store

import "sync"

// The browser's store instance.
//
// On the server a Store is per-process and read through the request context —
// two requests must never see each other's state. In the browser there is
// exactly one user and one tab, so a package-level instance is the right shape:
// any Mount, Unmount or click handler can reach it without threading it
// through a component tree.
//
// The server also links this variable; it simply never touches it, because
// server code always goes through the context.
var client = New()

// Client is the browser-side store. Call it from Mount, Unmount, or any event
// handler to read or mutate state.
func Client() *Store { return client }

// ---------------------------------------------------------------------------
// Subscriptions
//
// A mutation has to reach the DOM somehow. Rather than have every caller
// remember to re-render, a Store notifies its subscribers — which is precisely
// why Unmount exists: a page that subscribes on mount must unsubscribe when it
// leaves, or the callback fires against a DOM that is gone.
// ---------------------------------------------------------------------------

type subscriber struct {
	id int
	fn func()
}

type subs struct {
	mu    sync.Mutex
	next  int
	items []subscriber
}

var watchers subs

// Subscribe registers fn to run after every mutation. The returned function
// cancels it, and calling it twice is safe.
func Subscribe(fn func()) (cancel func()) {
	watchers.mu.Lock()
	watchers.next++
	id := watchers.next
	watchers.items = append(watchers.items, subscriber{id: id, fn: fn})
	watchers.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			watchers.mu.Lock()
			defer watchers.mu.Unlock()
			for i, s := range watchers.items {
				if s.id == id {
					watchers.items = append(watchers.items[:i], watchers.items[i+1:]...)
					return
				}
			}
		})
	}
}

// Notify runs every subscriber. Callbacks are copied out from under the lock so
// a subscriber may cancel itself, or subscribe again, without deadlocking.
func Notify() {
	watchers.mu.Lock()
	fns := make([]func(), 0, len(watchers.items))
	for _, s := range watchers.items {
		fns = append(fns, s.fn)
	}
	watchers.mu.Unlock()

	for _, fn := range fns {
		fn()
	}
}

// Watchers reports how many subscriptions are live — used by the demo page to
// make a leaked subscription visible instead of silent.
func Watchers() int {
	watchers.mu.Lock()
	defer watchers.mu.Unlock()
	return len(watchers.items)
}
