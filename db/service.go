package db

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"sync/atomic"
	"time"
)

// Options configure a [Service]. Only Collection is required.
type Options struct {
	// Collection is the table or collection name. It is also the cache-key
	// segment and the log label.
	Collection string
	// Cache configures query-result caching. The zero value disables it.
	Cache Cache
	// Timeout bounds every operation. Default 30s; bulk writes get ten times
	// it, because a set-wide UPDATE over a large table legitimately runs
	// longer than a single-row write.
	Timeout time.Duration
	// Log receives the debug records. Defaults to slog.Default().
	Log *slog.Logger
	// Debug logs every operation with its duration. Off in production: it is
	// one record per query.
	Debug bool
}

// The default operation deadline, and the multiplier applied to it for the
// set-wide writes.
const (
	defaultTimeout  = 30 * time.Second
	bulkTimeoutMult = 10
	// The largest single cached result, when [Cache.MaxEntryBytes] does not
	// say. The same 8 MiB mw.Cache allows a response.
	defaultMaxEntryBytes = 8 << 20
	// A patch reads, then writes under an optimistic lock. Two attempts
	// absorb the ordinary case of one concurrent writer; past that, retrying
	// is just a slower way to lose to sustained contention on the same
	// document, and [ErrConflict] is the honest answer.
	patchAttempts = 3
)

// Service is a collection: the whole contract, over any [Backend].
//
// Construct it through a backend's constructor (pg.New) rather than directly
// — the backend has to exist first, and its options travel with it.
type Service[T any, PT Document[T]] struct {
	backend  Backend
	name     string
	timeout  time.Duration
	log      *slog.Logger
	debug    bool
	declared map[string]bool

	cache     CacheAdapter
	ttl       time.Duration
	maxEntry  int
	cacheGet  bool
	cacheFind bool
	versioner Versioner
	local     atomic.Int64
	stats     counters
	flight    flight
}

// NewService wires a backend into the contract. Backends call it; an
// application calls the backend's own constructor.
func NewService[T any, PT Document[T]](backend Backend, o Options) (*Service[T, PT], error) {
	if o.Collection == "" {
		return nil, errors.New("db: Options.Collection is required")
	}
	s := &Service[T, PT]{
		backend:  backend,
		name:     o.Collection,
		timeout:  cmp.Or(o.Timeout, defaultTimeout),
		log:      cmp.Or(o.Log, slog.Default()),
		debug:    o.Debug,
		declared: declaredFields(reflect.TypeFor[T]()),
	}

	if o.Cache.TTL > 0 {
		s.ttl = o.Cache.TTL
		s.maxEntry = cmp.Or(o.Cache.MaxEntryBytes, defaultMaxEntryBytes)
		s.cacheGet = !o.Cache.SkipGet
		s.cacheFind = !o.Cache.SkipFind
		s.cache = o.Cache.Adapter
		if s.cache == nil {
			s.cache = NewLRU(o.Cache.MaxSize)
		}
		// Keys are written with the backend's prefix and cleared with the
		// adapter's. A mismatch is not a degraded cache, it is a cache that
		// never invalidates — so it is a construction error, not a warning.
		if p, ok := s.cache.(Prefixed); ok && p.Prefix() != backend.Prefix() {
			return nil, fmt.Errorf("db: %s: cache adapter prefix %q does not match backend prefix %q",
				o.Collection, p.Prefix(), backend.Prefix())
		}
		s.versioner, _ = s.cache.(Versioner)
	}
	return s, nil
}

// Collection is the name this service operates on.
func (s *Service[T, PT]) Collection() string { return s.name }

// Backend is the storage this service runs on. Reach for it to feature-detect
// a capability; a backend's own escape hatch is the better door to raw
// queries.
func (s *Service[T, PT]) Backend() Backend { return s.backend }

// ============================================================
// Reads
// ============================================================

// Get returns the document with this id, or [ErrNotFound]. A soft-deleted
// document is not found unless [Deleted] is passed.
func (s *Service[T, PT]) Get(ctx context.Context, id string, options ...Option) (T, error) {
	var zero T
	if id == "" {
		return zero, ErrNotFound
	}
	o := applyOptions(options)
	ctx, cancel := s.deadline(ctx, 1)
	defer cancel()
	defer s.trace(time.Now(), "get", "id", id)

	where := M{IDPath: id}
	if !o.deleted {
		where = active(where)
	}
	read := func(ctx context.Context) (json.RawMessage, error) {
		return s.backend.FindOne(ctx, where, OpOptions{Session: o.session})
	}

	key := ""
	if s.caches(o.session) && s.cacheGet && !o.deleted {
		key = s.key(ctx, "get", id)
	}
	if key == "" {
		raw, err := read(ctx)
		if err != nil {
			return zero, err
		}
		return decode[T, PT](raw)
	}
	if raw, hit := s.hit(ctx, key); hit {
		return decode[T, PT](raw)
	}
	raw, err := share(&s.flight, ctx, key, func(ctx context.Context) (json.RawMessage, error) {
		raw, err := read(ctx)
		if err != nil {
			return nil, err
		}
		s.store(ctx, key, raw)
		return raw, nil
	})
	if err != nil {
		return zero, err
	}
	return decode[T, PT](raw)
}

// GetMany returns the documents with these ids, keyed by id. Ids that do not
// exist are absent from the map rather than an error — the caller asked about
// a set, and a set can come back smaller.
func (s *Service[T, PT]) GetMany(ctx context.Context, ids []string, options ...Option) (map[string]T, error) {
	out := make(map[string]T, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	o := applyOptions(options)
	ctx, cancel := s.deadline(ctx, 1)
	defer cancel()
	defer s.trace(time.Now(), "get_many", "ids", len(ids))

	// The version is read once for the whole batch. s.key per id reads it per
	// id, which is one network round trip per id the moment the versioner is
	// not in this process.
	space := ""
	if s.caches(o.session) && s.cacheGet && !o.deleted {
		space, _ = s.keyspace(ctx)
	}
	misses := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, seen := out[id]; seen || id == "" {
			continue
		}
		if space != "" {
			if raw, hit := s.hit(ctx, space+"get:"+id); hit {
				doc, err := decode[T, PT](raw)
				if err != nil {
					return nil, err
				}
				out[id] = doc
				continue
			}
		}
		misses = append(misses, id)
	}
	if len(misses) == 0 {
		return out, nil
	}

	where := M{IDPath: M{OpIn: anySlice(misses)}}
	if !o.deleted {
		where = active(where)
	}
	rows, err := s.backend.FindMany(ctx, where, FindOptions{OpOptions: OpOptions{Session: o.session}})
	if err != nil {
		return nil, err
	}
	for _, raw := range rows {
		doc, err := decode[T, PT](raw)
		if err != nil {
			return nil, err
		}
		id := PT(&doc).envelope().ID
		out[id] = doc
		if space != "" {
			s.store(ctx, space+"get:"+id, raw)
		}
	}
	return out, nil
}

// Find returns every document matching the query. The zero Query returns
// every active document, which on a large collection is a mistake worth
// making deliberately — pass a Limit.
func (s *Service[T, PT]) Find(ctx context.Context, q Query) ([]T, error) {
	ctx, cancel := s.deadline(ctx, 1)
	defer cancel()
	defer s.trace(time.Now(), "find", "collection", s.name)

	read := func(ctx context.Context) ([]json.RawMessage, error) {
		rows, err := s.backend.FindMany(ctx, q.filter(), q.findOptions())
		if err != nil {
			return nil, err
		}
		if len(q.Project) > 0 {
			for i, raw := range rows {
				rows[i] = prune(raw, q.Project)
			}
		}
		return rows, nil
	}

	key := ""
	if s.caches(q.Session) && s.cacheFind {
		key = s.key(ctx, "find", queryDigest(q))
	}
	if key == "" {
		rows, err := read(ctx)
		if err != nil {
			return nil, err
		}
		return decodeAll[T, PT](rows)
	}
	if blob, hit := s.hit(ctx, key); hit {
		var rows []json.RawMessage
		if json.Unmarshal(blob, &rows) == nil {
			return decodeAll[T, PT](rows)
		}
		// An entry that will not decode is worse than no entry: it would be
		// re-read, and fail, until its TTL ran out. Drop it and query.
		s.cache.Del(ctx, key)
		s.stats.hits.Add(-1)
		s.stats.misses.Add(1)
	}
	rows, err := share(&s.flight, ctx, key, func(ctx context.Context) ([]json.RawMessage, error) {
		rows, err := read(ctx)
		if err != nil {
			return nil, err
		}
		// The result set is cached under a key that includes the projection,
		// but a projected document is never written to its by-id key: half a
		// document must not be served to a later Get.
		if blob, err := json.Marshal(rows); err == nil {
			s.store(ctx, key, blob)
		}
		return rows, nil
	})
	if err != nil {
		return nil, err
	}
	return decodeAll[T, PT](rows)
}

// One returns the first document matching the query, or [ErrNotFound]. It is
// where a domain lookup lands: One(ctx, db.Query{Where: db.Eq("email", e)}).
func (s *Service[T, PT]) One(ctx context.Context, q Query) (T, error) {
	var zero T
	q.Limit = 1
	docs, err := s.Find(ctx, q)
	if err != nil {
		return zero, err
	}
	if len(docs) == 0 {
		return zero, ErrNotFound
	}
	return docs[0], nil
}

// Count returns how many documents match. Limit, Skip, Sort and Project are
// ignored.
//
// It is cached on the same terms as [Service.Find] — a count is a find that
// returns a number, and a total next to a cached first page that is not itself
// cached is a query per request for the one value on the page that changes
// least.
func (s *Service[T, PT]) Count(ctx context.Context, q Query) (int64, error) {
	ctx, cancel := s.deadline(ctx, 1)
	defer cancel()
	defer s.trace(time.Now(), "count", "collection", s.name)

	read := func(ctx context.Context) (int64, error) {
		return s.backend.Count(ctx, q.filter(), q.opOptions())
	}

	key := ""
	if s.caches(q.Session) && s.cacheFind {
		// Limit, Skip, Sort and Project do not change a count, so they are not
		// in its key: two counts differing only in the page they belong to
		// share one entry.
		key = s.key(ctx, "count", queryDigest(Query{Where: q.Where, Deleted: q.Deleted}))
	}
	if key == "" {
		return read(ctx)
	}
	if raw, hit := s.hit(ctx, key); hit {
		if n, err := strconv.ParseInt(string(raw), 10, 64); err == nil {
			return n, nil
		}
		// As in Find: an entry that will not parse would fail every read until
		// its TTL ran out.
		s.cache.Del(ctx, key)
		s.stats.hits.Add(-1)
		s.stats.misses.Add(1)
	}
	return share(&s.flight, ctx, key, func(ctx context.Context) (int64, error) {
		n, err := read(ctx)
		if err != nil {
			return 0, err
		}
		s.store(ctx, key, strconv.AppendInt(nil, n, 10))
		return n, nil
	})
}

// ============================================================
// Writes
// ============================================================

// Create stamps the envelope, applies [Defaulter], validates, and inserts.
// The returned document is the stored one, id and all.
func (s *Service[T, PT]) Create(ctx context.Context, doc T, options ...Option) (T, error) {
	o := applyOptions(options)
	ctx, cancel := s.deadline(ctx, 1)
	defer cancel()
	defer s.trace(time.Now(), "create", "collection", s.name)

	p := PT(&doc)
	now := time.Now().UnixMilli()
	*p.envelope() = Doc{
		ID:      s.backend.NewID(),
		Version: 1,
		Meta:    Meta{CreatedAt: now, CreatedBy: o.actor, UpdatedAt: now, UpdatedBy: o.actor},
	}
	if d, ok := any(p).(Defaulter); ok {
		d.Defaults()
	}
	if err := s.validate(p); err != nil {
		return doc, err
	}

	raw, err := json.Marshal(p)
	if err != nil {
		return doc, fmt.Errorf("db: %s: encoding document: %w", s.name, err)
	}
	if err := s.backend.Insert(ctx, p.envelope().ID, raw, OpOptions{Session: o.session}); err != nil {
		return doc, err
	}
	s.invalidate(ctx, o.session)
	return doc, nil
}

// Patch reads the document, hands it to mutate, validates the result and
// writes back the fields that actually changed, under an optimistic lock on
// the version it read.
//
//	u, err := users.Patch(ctx, id, func(u *User) { u.Name = "Ada L." }, db.By(actor))
//
// A closure rather than a partial struct because Go has no partial struct,
// and because this is the shape the operation already has: the service must
// read before it writes in order to validate the whole document, so the
// caller may as well be handed the value it read. Field names are checked by
// the compiler, and a lost lock is retried from the fresh document rather
// than re-applying a stale delta.
//
// mutate may be called more than once. Keep it a pure edit of the value it is
// given — no I/O, no side effects.
func (s *Service[T, PT]) Patch(ctx context.Context, id string, mutate func(PT), options ...Option) (T, error) {
	return s.patch(ctx, id, applyOptions(options), func(current T, _ json.RawMessage) (T, error) {
		mutate(PT(&current))
		return current, nil
	})
}

// PatchFields is Patch for the case where the field names are data: dotted
// paths from a form, a script, an admin tool. It reads, deep-sets the paths,
// then validates the result exactly as Patch does — the names are dynamic,
// the guarantees are not.
func (s *Service[T, PT]) PatchFields(ctx context.Context, id string, values Set, options ...Option) (T, error) {
	return s.patch(ctx, id, applyOptions(options), func(_ T, raw json.RawMessage) (T, error) {
		var updated T
		merged, err := applySet(raw, values)
		if err != nil {
			return updated, fmt.Errorf("db: %s: applying fields: %w", s.name, err)
		}
		return decode[T, PT](merged)
	})
}

func (s *Service[T, PT]) patch(ctx context.Context, id string, o opts, produce func(T, json.RawMessage) (T, error)) (T, error) {
	var zero T
	if id == "" {
		return zero, ErrNotFound
	}
	ctx, cancel := s.deadline(ctx, 1)
	defer cancel()
	defer s.trace(time.Now(), "patch", "id", id)

	where := M{IDPath: id}
	if !o.deleted {
		where = active(where)
	}

	for attempt := range patchAttempts {
		raw, err := s.backend.FindOne(ctx, where, OpOptions{Session: o.session})
		if err != nil {
			return zero, err
		}
		current, err := decode[T, PT](raw)
		if err != nil {
			return zero, err
		}
		updated, err := produce(current, raw)
		if err != nil {
			return zero, err
		}

		// The envelope is the store's, whatever the caller did to it: taking
		// it from the document just read is also what makes the version below
		// the one the lock is taken on.
		envelope := *PT(&current).envelope()
		envelope.Meta.UpdatedAt = time.Now().UnixMilli()
		envelope.Meta.UpdatedBy = o.actor
		p := PT(&updated)
		*p.envelope() = envelope
		if err := s.validate(p); err != nil {
			return zero, err
		}

		next, err := json.Marshal(p)
		if err != nil {
			return zero, fmt.Errorf("db: %s: encoding document: %w", s.name, err)
		}
		set, unset, err := diff(raw, next, s.declared)
		if err != nil {
			return zero, err
		}

		stored, err := s.backend.UpdatePaths(ctx, id, set, UpdateOptions{
			OpOptions:       OpOptions{Session: o.session},
			ExpectedVersion: envelope.Version,
			Unset:           unset,
		})
		if errors.Is(err, ErrNotFound) {
			// Either the row went away or its version moved under us. Both
			// arrive as "no row matched"; the next attempt re-reads and finds
			// out which, and a genuine disappearance surfaces as ErrNotFound
			// from that read.
			if attempt < patchAttempts-1 {
				continue
			}
			return zero, ErrConflict
		}
		if err != nil {
			return zero, err
		}
		s.invalidate(ctx, o.session)
		return decode[T, PT](stored)
	}
	return zero, ErrConflict
}

// Delete soft-deletes: the document stays, stamped with the time and the
// actor, and drops out of every read that does not ask for [Deleted]. Pass
// [Hard] to remove the row instead.
//
// Deleting an already-deleted document is [ErrNotFound] — the same answer the
// reads give, so a caller does not have to know the difference.
func (s *Service[T, PT]) Delete(ctx context.Context, id string, options ...Option) error {
	if id == "" {
		return ErrNotFound
	}
	o := applyOptions(options)
	ctx, cancel := s.deadline(ctx, 1)
	defer cancel()
	defer s.trace(time.Now(), "delete", "id", id, "hard", o.hard)

	if o.hard {
		if _, err := s.backend.DeleteOne(ctx, id, OpOptions{Session: o.session}); err != nil {
			return err
		}
		s.invalidate(ctx, o.session)
		return nil
	}

	// Read first so a second delete answers ErrNotFound instead of restamping
	// a document that is already gone.
	if _, err := s.backend.FindOne(ctx, active(M{IDPath: id}), OpOptions{Session: o.session}); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	_, err := s.backend.UpdatePaths(ctx, id, map[string]any{
		DeletedAtPath: now,
		DeletedByPath: o.actor,
	}, UpdateOptions{OpOptions: OpOptions{Session: o.session}, NoBump: true})
	if err != nil {
		return err
	}
	s.invalidate(ctx, o.session)
	return nil
}

// Restore clears a soft delete. A document that is not deleted comes back
// unchanged, without a write.
func (s *Service[T, PT]) Restore(ctx context.Context, id string, options ...Option) (T, error) {
	var zero T
	if id == "" {
		return zero, ErrNotFound
	}
	o := applyOptions(options)
	ctx, cancel := s.deadline(ctx, 1)
	defer cancel()
	defer s.trace(time.Now(), "restore", "id", id)

	raw, err := s.backend.FindOne(ctx, M{IDPath: id}, OpOptions{Session: o.session})
	if err != nil {
		return zero, err
	}
	current, err := decode[T, PT](raw)
	if err != nil {
		return zero, err
	}
	if !PT(&current).envelope().Meta.IsDeleted() {
		return current, nil
	}

	// Null rather than removed: the envelope's shape is part of the contract,
	// and the generated column that indexes it reads a JSON null as SQL NULL
	// either way.
	stored, err := s.backend.UpdatePaths(ctx, id, map[string]any{
		DeletedAtPath: nil,
		DeletedByPath: nil,
		UpdatedAtPath: time.Now().UnixMilli(),
		UpdatedByPath: o.actor,
	}, UpdateOptions{OpOptions: OpOptions{Session: o.session}})
	if err != nil {
		return zero, err
	}
	s.invalidate(ctx, o.session)
	return decode[T, PT](stored)
}

// ============================================================
// Set-wide writes
// ============================================================

// PatchWhere applies the same paths to every active document matching where,
// in one statement when the backend can, and returns how many it changed.
//
// Unlike [Service.Patch] it does no per-document read, no merge and no
// validation: the same fixed values are written to every match. That is the
// point — it is for a flag flip or a foreign-key reassignment across a set.
// For anything that has to be checked per document, loop over [Service.Patch].
//
// An empty filter is refused. "Every document in the collection" is a
// decision, and it should not be reachable by forgetting an argument.
func (s *Service[T, PT]) PatchWhere(ctx context.Context, where M, values Set, options ...Option) (int64, error) {
	if len(where) == 0 {
		return 0, fmt.Errorf("db: %s: PatchWhere refuses an empty filter", s.name)
	}
	if len(values) == 0 {
		return 0, nil
	}
	o := applyOptions(options)
	ctx, cancel := s.deadline(ctx, bulkTimeoutMult)
	defer cancel()
	defer s.trace(time.Now(), "patch_where", "collection", s.name)

	paths := make(map[string]any, len(values)+2)
	for path, value := range values {
		if path == IDPath || path == VersionPath {
			continue
		}
		paths[path] = value
	}
	paths[UpdatedAtPath] = time.Now().UnixMilli()
	paths[UpdatedByPath] = o.actor

	n, err := s.bulk(ctx, active(where), paths, UpdateOptions{OpOptions: OpOptions{Session: o.session}})
	if err != nil {
		return 0, err
	}
	s.invalidate(ctx, o.session)
	return n, nil
}

// DeleteWhere soft-deletes every active document matching where, and returns
// how many. An empty filter is refused; [Hard] is not supported here, because
// a bulk hard delete should be a statement you wrote yourself.
func (s *Service[T, PT]) DeleteWhere(ctx context.Context, where M, options ...Option) (int64, error) {
	if len(where) == 0 {
		return 0, fmt.Errorf("db: %s: DeleteWhere refuses an empty filter", s.name)
	}
	o := applyOptions(options)
	ctx, cancel := s.deadline(ctx, bulkTimeoutMult)
	defer cancel()
	defer s.trace(time.Now(), "delete_where", "collection", s.name)

	n, err := s.bulk(ctx, active(where), map[string]any{
		DeletedAtPath: time.Now().UnixMilli(),
		DeletedByPath: o.actor,
	}, UpdateOptions{OpOptions: OpOptions{Session: o.session}, NoBump: true})
	if err != nil {
		return 0, err
	}
	s.invalidate(ctx, o.session)
	return n, nil
}

// bulk runs a set-wide write through the backend's [BulkWriter] when it has
// one, and otherwise walks the matching ids. The fallback is sequential
// rather than concurrent: without knowing the backend's connection pool,
// fanning out is how a bulk over ten thousand rows becomes an outage
// somewhere else.
func (s *Service[T, PT]) bulk(ctx context.Context, where M, paths map[string]any, o UpdateOptions) (int64, error) {
	if writer, ok := s.backend.(BulkWriter); ok {
		return writer.UpdatePathsWhere(ctx, where, paths, o)
	}
	rows, err := s.backend.FindMany(ctx, where, FindOptions{OpOptions: o.OpOptions})
	if err != nil {
		return 0, err
	}
	var n int64
	for _, raw := range rows {
		doc, err := decode[T, PT](raw)
		if err != nil {
			return n, err
		}
		if _, err := s.backend.UpdatePaths(ctx, PT(&doc).envelope().ID, paths, o); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // deleted between the read and the write
			}
			return n, err
		}
		n++
	}
	return n, nil
}

// ============================================================
// Plumbing
// ============================================================

func (s *Service[T, PT]) validate(p PT) error {
	if v, ok := any(p).(Validator); ok {
		if err := v.Validate(); err != nil {
			return invalid(s.name, err)
		}
	}
	return nil
}

func decode[T any, PT Document[T]](raw json.RawMessage) (T, error) {
	var doc T
	if err := json.Unmarshal(raw, PT(&doc)); err != nil {
		return doc, fmt.Errorf("db: decoding document: %w", err)
	}
	return doc, nil
}

func decodeAll[T any, PT Document[T]](rows []json.RawMessage) ([]T, error) {
	out := make([]T, 0, len(rows))
	for _, raw := range rows {
		doc, err := decode[T, PT](raw)
		if err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, nil
}

func (s *Service[T, PT]) deadline(ctx context.Context, mult int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.timeout*time.Duration(mult))
}

func (s *Service[T, PT]) trace(start time.Time, op string, args ...any) {
	if !s.debug {
		return
	}
	s.log.Debug("db "+op, append([]any{"collection", s.name, "took", time.Since(start)}, args...)...)
}

// caches reports whether this operation may touch the cache. An operation
// inside a transaction never does, in either direction: its reads are not
// visible to anyone else yet, and its writes may still roll back.
func (s *Service[T, PT]) caches(session any) bool { return s.cache != nil && session == nil }

// key builds <keyspace><kind>:<suffix>, or "" when the cache cannot be used
// for this read. Bumping a version makes every key built before it
// unreachable, with no pattern scan and no key enumeration.
func (s *Service[T, PT]) key(ctx context.Context, kind, suffix string) string {
	space, ok := s.keyspace(ctx)
	if !ok {
		return ""
	}
	return space + kind + ":" + suffix
}

// keyspace is <prefix>:<collection>:v<shared>.<local>: — the part of a key
// every entry for this collection shares, and the reason invalidation is O(1).
// Callers that build more than one key hold it rather than calling [key] per
// key: with an adapter [Versioner] the shared half costs a round trip.
//
// Both counters are always present, rather than one standing in for the other
// when a version read fails. They number differently — a shared version is
// every replica's writes, a local one is this process's — so a key that
// silently switched from one to the other would read the entries of whichever
// earlier window happened to land on the same number.
//
// ok is false when the shared version could not be read. The read then runs
// uncached: a key built on a guessed version is a key pointing at somebody
// else's documents.
func (s *Service[T, PT]) keyspace(ctx context.Context) (string, bool) {
	shared := int64(0)
	if s.versioner != nil {
		v, err := s.versioner.Version(ctx, s.name)
		if err != nil {
			// Debug, not Warn: this is on the path of every cached read, so an
			// adapter that is down would write one line per query. The count
			// is in CacheStats().Bypassed, which is where an operator looks.
			s.stats.bypassed.Add(1)
			s.log.Debug("db cache version unreadable; serving uncached",
				"collection", s.name, "error", err)
			return "", false
		}
		shared = v
	}
	return s.backend.Prefix() + ":" + s.name +
		":v" + strconv.FormatInt(shared, 10) +
		"." + strconv.FormatInt(s.local.Load(), 10) + ":", true
}

// hit reads an entry and counts what happened, because a document read has no
// response header to say so the way mw.Cache does.
func (s *Service[T, PT]) hit(ctx context.Context, key string) ([]byte, bool) {
	raw, ok := s.cache.Get(ctx, key)
	if ok {
		s.stats.hits.Add(1)
	} else {
		s.stats.misses.Add(1)
	}
	return raw, ok
}

// store keeps a result unless it is too big to be worth a cache. The caller
// has already been served either way: a cap that dropped the answer would be a
// correctness bug, where a cap that drops the entry is only a slower next read.
func (s *Service[T, PT]) store(ctx context.Context, key string, value []byte) {
	if len(value) > s.maxEntry {
		s.stats.tooLarge.Add(1)
		return
	}
	s.cache.Set(ctx, key, value, s.ttl)
}

// CacheStats reports what the cache has done since this service was built. The
// zero value comes back from a service with no cache configured.
func (s *Service[T, PT]) CacheStats() CacheStats { return s.stats.snapshot() }

// invalidate retires every cache key for this collection. Nothing is deleted:
// the entries become unreachable the moment the version moves, and the LRU
// reclaims them in its own time.
func (s *Service[T, PT]) invalidate(ctx context.Context, session any) {
	if s.cache == nil || session != nil {
		return
	}
	if s.versioner != nil {
		if _, err := s.versioner.Bump(ctx, s.name); err == nil {
			return
		}
		// Warn, unlike a failed version read: the write has already happened,
		// so every other replica is now serving documents it should not, and
		// the local counter below fixes only this one.
		s.log.Warn("db cache version bump failed; falling back to the local counter",
			"collection", s.name)
	}
	s.local.Add(1)
}

// queryDigest identifies a find. json.Marshal sorts map keys, so two equal
// filters written in a different order produce the same key.
func queryDigest(q Query) string {
	blob, err := json.Marshal(struct {
		Where   M        `json:"w"`
		Sort    Sort     `json:"s"`
		Limit   int      `json:"l"`
		Skip    int      `json:"k"`
		Project []string `json:"p"`
		Deleted bool     `json:"d"`
	}{q.Where, q.Sort, q.Limit, q.Skip, q.Project, q.Deleted})
	if err != nil {
		return "unhashable"
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:12])
}
