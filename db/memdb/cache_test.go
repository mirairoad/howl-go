package memdb_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirairoad/howl-go/core/cache"
	"github.com/mirairoad/howl-go/db"
	"github.com/mirairoad/howl-go/db/conformance"
	"github.com/mirairoad/howl-go/db/memdb"
)

// versioned is a shared adapter: one store, and the collection counters that
// make invalidation reach every service using it. It is what a Redis adapter
// implements with INCR, and it exists here because [db.Versioner] is otherwise
// a capability nothing in the tree implements — the cross-process invalidation
// path would be described and never run.
type versioned struct {
	cache.Store
	mu      sync.Mutex
	version map[string]int64
	broken  bool // every Version and Bump fails, as an adapter that is down does
}

func newVersioned() *versioned {
	return &versioned{Store: cache.NewLRU(256), version: map[string]int64{}}
}

var errAdapterDown = errors.New("adapter down")

func (v *versioned) Version(_ context.Context, collection string) (int64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.broken {
		return 0, errAdapterDown
	}
	return v.version[collection], nil
}

func (v *versioned) Bump(_ context.Context, collection string) (int64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.broken {
		return 0, errAdapterDown
	}
	v.version[collection]++
	return v.version[collection], nil
}

// The conformance suite a third time, over an adapter that carries the version
// itself. Every case passed uncached and passed on the in-process counter, so a
// failure here is the shared-version path and nothing else: Version on the read
// side, Bump on every write.
func TestConformanceSharedVersionedCache(t *testing.T) {
	conformance.Run(t, func(t *testing.T) conformance.Service {
		// A store per case, not one for the suite: the collection name and the
		// version are the same in every case, so a shared one would let a
		// find from the previous case answer this one's.
		s, err := db.NewService[conformance.Doc](memdb.New(), db.Options{
			Collection: "conformance",
			Cache:      db.Cache{TTL: time.Minute, Adapter: newVersioned()},
		})
		if err != nil {
			t.Fatalf("new service: %v", err)
		}
		return s
	})
}

type doc struct {
	db.Doc
	Name string `json:"name"`
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// counting is a backend that reports how often it was actually asked.
type counting struct {
	db.Backend
	ones, manys, counts atomic.Int64
	gate                chan struct{} // FindOne waits for it when non-nil
}

func (c *counting) FindOne(ctx context.Context, where db.M, o db.OpOptions) (json.RawMessage, error) {
	c.ones.Add(1)
	if c.gate != nil {
		<-c.gate
	}
	return c.Backend.FindOne(ctx, where, o)
}

func (c *counting) FindMany(ctx context.Context, where db.M, o db.FindOptions) ([]json.RawMessage, error) {
	c.manys.Add(1)
	return c.Backend.FindMany(ctx, where, o)
}

func (c *counting) Count(ctx context.Context, where db.M, o db.OpOptions) (int64, error) {
	c.counts.Add(1)
	return c.Backend.Count(ctx, where, o)
}

func service(t *testing.T, backend db.Backend, c db.Cache) *db.Service[doc, *doc] {
	t.Helper()
	s, err := db.NewService[doc](backend, db.Options{Collection: "docs", Cache: c, Log: quiet()})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return s
}

func TestCountIsCached(t *testing.T) {
	ctx := context.Background()
	backend := &counting{Backend: memdb.New()}
	cached := service(t, backend, db.Cache{TTL: time.Minute})
	// A second service on the same data, with no cache: its writes are how the
	// collection changes without the cached service knowing.
	other := service(t, backend, db.Cache{})

	for _, name := range []string{"a", "b"} {
		if _, err := other.Create(ctx, doc{Name: name}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if n, err := cached.Count(ctx, db.Query{}); err != nil || n != 2 {
		t.Fatalf("Count = %d, %v; want 2", n, err)
	}
	if _, err := other.Create(ctx, doc{Name: "c"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if n, err := cached.Count(ctx, db.Query{}); err != nil || n != 2 {
		t.Fatalf("Count = %d, %v; want the cached 2", n, err)
	}
	if got := backend.counts.Load(); got != 1 {
		t.Fatalf("backend counted %d times, want 1", got)
	}

	// A write through the cached service retires the entry.
	if _, err := cached.Create(ctx, doc{Name: "d"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if n, err := cached.Count(ctx, db.Query{}); err != nil || n != 4 {
		t.Fatalf("Count after a write = %d, %v; want 4", n, err)
	}

	// Sort, Limit and Skip are not part of a count, so they do not split the
	// entry: this one is answered by the one just filled.
	if n, err := cached.Count(ctx, db.Query{Sort: db.Sort{{Field: "name"}}, Limit: 1, Skip: 2}); err != nil || n != 4 {
		t.Fatalf("Count with paging = %d, %v; want the cached 4", n, err)
	}
	if got := backend.counts.Load(); got != 2 {
		t.Fatalf("backend counted %d times, want 2", got)
	}
	// Two counts each side of the write: the first of each pair filled the
	// entry, the second read it.
	if s := cached.CacheStats(); s.Hits != 2 || s.Misses != 2 {
		t.Fatalf("stats = %+v, want 2 hits and 2 misses", s)
	}
}

// A cold key under load is the case the cache does not help with: every caller
// misses, and every caller queries.
func TestConcurrentMissesShareOneQuery(t *testing.T) {
	ctx := context.Background()
	backend := &counting{Backend: memdb.New(), gate: make(chan struct{})}
	s := service(t, backend, db.Cache{TTL: time.Minute})

	created, err := service(t, backend, db.Cache{}).Create(ctx, doc{Name: "hot"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	backend.ones.Store(0)

	const readers = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, readers)
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := s.Get(ctx, created.ID)
			if err == nil && got.Name != "hot" {
				err = errors.New("wrong document: " + got.Name)
			}
			errs <- err
		}()
	}
	close(start)
	// Every reader is now either in the backend or waiting on the leader. The
	// gate holds the one query open long enough for the rest to arrive.
	time.Sleep(100 * time.Millisecond)
	close(backend.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("get: %v", err)
		}
	}
	if got := backend.ones.Load(); got != 1 {
		t.Fatalf("%d readers on a cold key cost %d queries, want 1", readers, got)
	}
}

func TestOversizeResultIsServedButNotStored(t *testing.T) {
	ctx := context.Background()
	backend := &counting{Backend: memdb.New()}
	s := service(t, backend, db.Cache{TTL: time.Minute, MaxEntryBytes: 16})

	if _, err := s.Create(ctx, doc{Name: "a name longer than sixteen bytes"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for range 2 {
		docs, err := s.Find(ctx, db.Query{})
		if err != nil || len(docs) != 1 {
			t.Fatalf("Find = %d docs, %v; want 1", len(docs), err)
		}
	}
	if got := backend.manys.Load(); got != 2 {
		t.Fatalf("backend queried %d times, want 2 — the oversize entry was stored", got)
	}
	if st := s.CacheStats(); st.TooLarge != 2 || st.Hits != 0 {
		t.Fatalf("stats = %+v, want 2 too-large and no hits", st)
	}
}

// Two services over one collection, sharing one adapter. Without a Versioner
// each owns a private counter, so a write through one leaves the other's
// entries reachable — the documented hazard, pinned here so it stays a
// deliberate limit rather than a surprise.
func TestSharedAdapterNeedsAVersioner(t *testing.T) {
	ctx := context.Background()

	stale := func(adapter db.CacheAdapter) string {
		backend := memdb.New()
		reader := service(t, backend, db.Cache{TTL: time.Minute, Adapter: adapter})
		writer := service(t, backend, db.Cache{TTL: time.Minute, Adapter: adapter})

		created, err := writer.Create(ctx, doc{Name: "before"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := reader.Get(ctx, created.ID); err != nil { // fills the entry
			t.Fatalf("get: %v", err)
		}
		if _, err := writer.PatchFields(ctx, created.ID, db.Set{"name": "after"}); err != nil {
			t.Fatalf("patch: %v", err)
		}
		got, err := reader.Get(ctx, created.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return got.Name
	}

	if name := stale(cache.NewLRU(64)); name != "before" {
		t.Fatalf("a plain shared adapter served %q — the local-counter limit has changed and the docs with it", name)
	}
	if name := stale(newVersioned()); name != "after" {
		t.Fatalf("a versioned adapter served the stale %q", name)
	}
}

// An adapter whose version cannot be read is an adapter that cannot be used:
// the alternative is a key built on a guessed version, which reads whatever
// another window left at that number.
func TestUnreadableVersionServesUncached(t *testing.T) {
	ctx := context.Background()
	backend := &counting{Backend: memdb.New()}
	adapter := newVersioned()
	s := service(t, backend, db.Cache{TTL: time.Minute, Adapter: adapter})

	created, err := s.Create(ctx, doc{Name: "a"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	adapter.broken = true

	for range 2 {
		if got, err := s.Get(ctx, created.ID); err != nil || got.Name != "a" {
			t.Fatalf("Get = %q, %v; want a", got.Name, err)
		}
	}
	if got := backend.ones.Load(); got != 2 {
		t.Fatalf("backend queried %d times, want 2 — a broken adapter was trusted", got)
	}
	st := s.CacheStats()
	if st.Bypassed != 2 || st.Hits != 0 || st.Misses != 0 {
		t.Fatalf("stats = %+v, want 2 bypassed and nothing else", st)
	}
}
