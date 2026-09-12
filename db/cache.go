package db

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/mirairoad/howl-go/core/cache"
)

// Cache configures query-result caching. The zero value disables it: a TTL of
// zero means nothing is cached, which is the right default for a store whose
// documents are edited through the same process that reads them.
type Cache struct {
	// TTL is how long an entry lives. Zero disables caching.
	TTL time.Duration
	// MaxSize caps the built-in in-process cache, in entries. Default 1000.
	MaxSize int
	// MaxEntryBytes is the largest single result stored. Default 8 MiB, the
	// same cap mw.Cache puts on a response; a larger result is still returned,
	// just not kept. MaxSize counts entries, so without this one unbounded
	// Find — no Limit, on a table that grew — is one entry holding a table.
	MaxEntryBytes int
	// Adapter is the cache implementation. Nil uses an in-process LRU, which
	// is correct for a single process and wrong the moment there are two —
	// invalidation is per-process, so a second replica keeps serving its own
	// stale copy. Supply a shared adapter (Redis) for anything replicated.
	Adapter CacheAdapter
	// SkipGet and SkipFind exclude one shape of read from the cache. Find
	// results churn the most, and a collection written more than it is listed
	// is often better off caching only by id. SkipFind covers Count, which is
	// a find that returns a number.
	SkipGet  bool
	SkipFind bool
}

// CacheAdapter is the storage a cache uses — core/cache's Store, so one
// adapter serves documents, endpoint responses and pages alike. Get returns
// false on a miss; Set is best-effort and never reports failure, because a
// cache that fails a write has cost the caller nothing but a slower next read.
type CacheAdapter = cache.Store

// Versioner is the optional capability that makes invalidation work across
// processes. Every cache key embeds a collection version; a write bumps it,
// and every key built before the bump is unreachable at once — no pattern
// scan, no key enumeration.
//
// An adapter that does not implement it gets [Service]'s in-process counter,
// which invalidates this process and no other. That counter is also why two
// services over one collection — the same table read through two document
// types — must share a Versioner if they share an adapter: each service owns
// its own counter, so a write through one leaves the other's keys reachable.
//
// Version is on the path of every cached read. A Version that returns an error
// does not fail the read: the read runs uncached, because a key built on a
// guessed version reads some other window's entries. A Bump that fails is
// logged and falls back to the local counter, which retires this process's
// keys and leaves every other replica serving stale documents until the TTL
// runs out — the one failure this design cannot paper over.
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
func NewLRU(max int) CacheAdapter { return cache.NewLRU(max) }

// CacheStats counts what the cache has done since the service was built. A
// document read has no response header to carry a hit or a miss the way
// mw.Cache does, so this is the only way to tell a cache that is working from
// one that answers nothing: a service with traffic and no Hits has a TTL
// shorter than the gap between reads, or a write on every request.
type CacheStats struct {
	// Hits and Misses count cached reads answered from the store and from the
	// backend. Every read counts once: a miss that waits on another caller's
	// query is still a miss, though the two of them cost one query.
	Hits, Misses int64
	// Bypassed counts reads that could not use the cache because the shared
	// version was unreadable — an adapter [Versioner] returning errors.
	Bypassed int64
	// TooLarge counts results that were returned but not stored, being larger
	// than [Cache.MaxEntryBytes].
	TooLarge int64
}

// counters is CacheStats while it is being written to.
type counters struct {
	hits, misses, bypassed, tooLarge atomic.Int64
}

func (c *counters) snapshot() CacheStats {
	return CacheStats{
		Hits:     c.hits.Load(),
		Misses:   c.misses.Load(),
		Bypassed: c.bypassed.Load(),
		TooLarge: c.tooLarge.Load(),
	}
}
