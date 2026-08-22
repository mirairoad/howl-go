package db

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Cache configures query-result caching. The zero value disables it: a TTL of
// zero means nothing is cached, which is the right default for a store whose
// documents are edited through the same process that reads them.
type Cache struct {
	// TTL is how long an entry lives. Zero disables caching.
	TTL time.Duration
	// MaxSize caps the built-in in-process cache, in entries. Default 1000.
	MaxSize int
	// Adapter is the cache implementation. Nil uses an in-process LRU, which
	// is correct for a single process and wrong the moment there are two —
	// invalidation is per-process, so a second replica keeps serving its own
	// stale copy. Supply a shared adapter (Redis) for anything replicated.
	Adapter CacheAdapter
	// SkipGet and SkipFind exclude one shape of read from the cache. Find
	// results churn the most, and a collection written more than it is listed
	// is often better off caching only by id.
	SkipGet  bool
	SkipFind bool
}

// CacheAdapter is the storage a cache uses. Get returns false on a miss; Set
// is best-effort and never reports failure, because a cache that fails a
// write has cost the caller nothing but a slower next read.
type CacheAdapter interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration)
	Del(ctx context.Context, keys ...string)
}

// Versioner is the optional capability that makes invalidation work across
// processes. Every cache key embeds a collection version; a write bumps it,
// and every key built before the bump is unreachable at once — no pattern
// scan, no key enumeration.
//
// An adapter that does not implement it gets [Service]'s in-process counter,
// which invalidates this process and no other.
type Versioner interface {
	Version(ctx context.Context, collection string) (int64, error)
	Bump(ctx context.Context, collection string) (int64, error)
}

// Prefixed is implemented by a namespaced adapter. [NewService] checks the
// namespace against the backend's, because keys are written with the
// backend's prefix and cleared with the adapter's: a mismatch makes every
// clear a no-op, silently, forever.
type Prefixed interface{ Prefix() string }

// NewLRU returns an in-process cache holding at most max entries, evicting
// least-recently-used. It is what [Cache] uses when no adapter is set.
func NewLRU(max int) CacheAdapter {
	if max <= 0 {
		max = 1000
	}
	return &lru{max: max, index: make(map[string]*list.Element, max), order: list.New()}
}

type lru struct {
	mu    sync.Mutex
	max   int
	index map[string]*list.Element
	order *list.List
}

type entry struct {
	key     string
	value   []byte
	expires time.Time
}

func (c *lru) Get(_ context.Context, key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*entry)
	if time.Now().After(e.expires) {
		c.drop(el)
		return nil, false
	}
	c.order.MoveToFront(el)
	return e.value, true
}

func (c *lru) Set(_ context.Context, key string, value []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	expires := time.Now().Add(ttl)
	if el, ok := c.index[key]; ok {
		e := el.Value.(*entry)
		e.value, e.expires = value, expires
		c.order.MoveToFront(el)
		return
	}
	c.index[key] = c.order.PushFront(&entry{key: key, value: value, expires: expires})
	for c.order.Len() > c.max {
		c.drop(c.order.Back())
	}
}

func (c *lru) Del(_ context.Context, keys ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range keys {
		if el, ok := c.index[k]; ok {
			c.drop(el)
		}
	}
}

func (c *lru) drop(el *list.Element) {
	if el == nil {
		return
	}
	c.order.Remove(el)
	delete(c.index, el.Value.(*entry).key)
}
