package db

import (
	"context"
	"sync"
)

// flight collapses concurrent misses on one cache key into one query. The
// cache protects the backend from repeat reads; without this, nothing protects
// it from the burst that fills a cold key — a popular document dropped at
// 200 requests/second is 200 identical SELECTs, all of them writing the same
// entry.
//
// It only guards reads that are cached. An uncached service queries per call
// by construction, and a caller who asked for no cache did not ask to be
// queued behind another caller's deadline, which is what sharing a query is.
type flight struct {
	mu    sync.Mutex
	calls map[string]*call
}

type call struct {
	done chan struct{}
	val  any
	err  error
}

// share runs load for the first caller on key and hands its result to everyone
// who arrived while it ran.
//
// The leader's ctx is the one load runs under, so the query carries the
// deadline of whichever caller got there first. A follower whose own ctx ends
// first stops waiting and answers with that; it does not cancel the leader,
// whose result the next reader still wants in the cache.
func share[V any](f *flight, ctx context.Context, key string, load func(context.Context) (V, error)) (V, error) {
	var zero V

	f.mu.Lock()
	if c, ok := f.calls[key]; ok {
		f.mu.Unlock()
		select {
		case <-c.done:
		case <-ctx.Done():
			return zero, ctx.Err()
		}
		if v, ok := c.val.(V); ok || c.err != nil {
			return v, c.err
		}
		// The key's kind segment determines V, so this is unreachable — and if
		// that ever stops being true it costs a duplicated query rather than a
		// panic or a zero value pretending to be a document.
		return load(ctx)
	}
	c := &call{done: make(chan struct{})}
	if f.calls == nil {
		f.calls = make(map[string]*call)
	}
	f.calls[key] = c
	f.mu.Unlock()

	v, err := load(ctx)

	c.val, c.err = v, err
	f.mu.Lock()
	delete(f.calls, key)
	f.mu.Unlock()
	// After the delete: a follower that read c from the map is already
	// committed to waiting, and one that missed it starts its own flight
	// against a cache the leader has by now filled.
	close(c.done)
	return v, err
}
