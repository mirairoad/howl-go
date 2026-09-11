// Package cache is the storage behind every cache in howl-go: endpoint
// responses (core/api), page responses (core/app) and document reads (db).
//
// One interface, so one adapter serves all three. An application that runs
// Redis writes Get, Set and Del once and hands the same value to api.Config,
// app.Config and db.Cache; nothing else about caching is backend-specific.
//
// It imports nothing but the standard library, so a package anywhere in the
// tree — including db, which imports nothing else from core — can depend on it.
package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Store is where cached bytes live. Get returns false on a miss; Set is
// best-effort and never reports failure, because a cache that fails a write has
// cost the caller nothing but a slower next read.
//
// A Store never decides what is safe to cache. The callers do, and their rules
// are written next to them: a response that sets a cookie is never stored, and
// a page request carrying credentials never reads one.
type Store interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration)
	Del(ctx context.Context, keys ...string)
}

// NewLRU returns an in-process store holding at most max entries, evicting the
// least recently used. max <= 0 means 1000.
//
// It is correct for one process and wrong the moment there are two: each
// replica keeps its own copy, so an entry one of them drops is still served by
// the others until its TTL runs out. Put a shared store in front for anything
// replicated — see Try.
func NewLRU(max int) Store {
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

// Try reads primary first and falls back to fallback on a miss — howl (TS)'s
// tryCache(redisCache, memoryCache). Writes and deletes go to both, so the
// fallback is warm on the day the primary goes away.
//
// timeout bounds each call to primary; zero means no bound. With one, a Redis
// that has stopped answering costs every request that long and no longer —
// without one, a store the network has swallowed holds each request until the
// caller's own context gives up, which for a page is the user giving up.
func Try(primary, fallback Store, timeout time.Duration) Store {
	return try{primary: primary, fallback: fallback, timeout: timeout}
}

type try struct {
	primary, fallback Store
	timeout           time.Duration
}

func (t try) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	if t.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, t.timeout)
}

func (t try) Get(ctx context.Context, key string) ([]byte, bool) {
	pctx, cancel := t.bounded(ctx)
	v, ok := t.primary.Get(pctx, key)
	cancel()
	if ok {
		return v, true
	}
	return t.fallback.Get(ctx, key)
}

func (t try) Set(ctx context.Context, key string, value []byte, ttl time.Duration) {
	pctx, cancel := t.bounded(ctx)
	t.primary.Set(pctx, key, value, ttl)
	cancel()
	t.fallback.Set(ctx, key, value, ttl)
}

func (t try) Del(ctx context.Context, keys ...string) {
	pctx, cancel := t.bounded(ctx)
	t.primary.Del(pctx, keys...)
	cancel()
	t.fallback.Del(ctx, keys...)
}
