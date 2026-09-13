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

	"github.com/mirairoad/howl-go/core/observe"
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
	// Trace decides how much of a query reaches its span: the filter, the
	// fields an update set, the id. The zero value records the shape and the
	// identifiers and redacts everything else, which is what makes a trace
	// able to answer "which documents did that delete touch" without turning
	// into a copy of the data.
	//
	// Nothing is rendered unless a tracer is installed, so this costs an
	// untraced process nothing at all.
	Trace observe.Mode
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

	mode      observe.Mode // how much of a query reaches its span
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
		mode:     o.Trace,
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
func (s *Service[T, PT]) Get(ctx context.Context, id string, options ...Option) (doc T, err error) {
	var zero T
	if id == "" {
		return zero, ErrNotFound
	}
	o := applyOptions(options)
	ctx, o2 := s.begin(ctx, "get", o.session, "id", id)
	defer func() { s.end(o2, err, "get", "id", id) }()
	s.document(ctx, o2, id)
	ctx, cancel := s.deadline(ctx, 1)
	defer cancel()

	where := M{IDPath: id}
	if !o.deleted {
		where = active(where)
	}
	read := func(ctx context.Context) (json.RawMessage, error) {
		ctx, done := s.storage(ctx, "find_one")
		raw, err := s.backend.FindOne(ctx, where, OpOptions{Session: o.session})
		done(err)
		return raw, err
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
func (s *Service[T, PT]) GetMany(ctx context.Context, ids []string, options ...Option) (out map[string]T, err error) {
	out = make(map[string]T, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	o := applyOptions(options)
	ctx, o2 := s.begin(ctx, "get_many", o.session, "ids", len(ids))
	defer func() { s.end(o2, err, "get_many", "ids", len(ids)) }()
	if s.recording(ctx) {
		s.query(ctx, o2, M{IDPath: M{OpIn: anySlice(ids)}})
	}
	ctx, cancel := s.deadline(ctx, 1)
	defer cancel()

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
	sctx, done := s.storage(ctx, "find_many")
	rows, err := s.backend.FindMany(sctx, where, FindOptions{OpOptions: OpOptions{Session: o.session}})
	done(err)
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
	o2.count(int64(len(out)))
	return out, nil
}

// Find returns every document matching the query. The zero Query returns
// every active document, which on a large collection is a mistake worth
// making deliberately — pass a Limit.
func (s *Service[T, PT]) Find(ctx context.Context, q Query) (docs []T, err error) {
	ctx, o2 := s.begin(ctx, "find", q.Session)
	defer func() { s.end(o2, err, "find") }()
	if s.recording(ctx) {
		s.query(ctx, o2, q.trace())
	}
	ctx, cancel := s.deadline(ctx, 1)
	defer cancel()

	read := func(ctx context.Context) ([]json.RawMessage, error) {
		ctx, done := s.storage(ctx, "find_many")
		rows, err := s.backend.FindMany(ctx, q.filter(), q.findOptions())
		done(err)
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
		o2.count(int64(len(rows)))
		if err != nil {
			return nil, err
		}
		return decodeAll[T, PT](rows)
	}
	if blob, hit := s.hit(ctx, key); hit {
		var rows []json.RawMessage
		if json.Unmarshal(blob, &rows) == nil {
			o2.count(int64(len(rows)))
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
		o2.count(int64(len(rows)))
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
func (s *Service[T, PT]) One(ctx context.Context, q Query) (doc T, err error) {
	// Its own span, with the Find it delegates to nested inside: without one,
	// a One shows up in a trace as a find with a limit of 1 and the caller
	// cannot tell which of the two it wrote — and "not found" is One's answer,
	// not Find's.
	ctx, o2 := s.begin(ctx, "one", q.Session)
	defer func() { s.end(o2, err, "one") }()
	if s.recording(ctx) {
		s.query(ctx, o2, q.trace())
	}

	var zero T
	q.Limit = 1
	docs, err := s.Find(ctx, q)
	if err != nil {
		return zero, err
	}
	if len(docs) == 0 {
		return zero, ErrNotFound
	}
	o2.count(1)
	return docs[0], nil
}

// Count returns how many documents match. Limit, Skip, Sort and Project are
// ignored.
//
// It is cached on the same terms as [Service.Find] — a count is a find that
// returns a number, and a total next to a cached first page that is not itself
// cached is a query per request for the one value on the page that changes
// least.
func (s *Service[T, PT]) Count(ctx context.Context, q Query) (n int64, err error) {
	ctx, o2 := s.begin(ctx, "count", q.Session)
	defer func() { s.end(o2, err, "count") }()
	if s.recording(ctx) {
		s.query(ctx, o2, q.trace())
	}
	ctx, cancel := s.deadline(ctx, 1)
	defer cancel()

	read := func(ctx context.Context) (int64, error) {
		ctx, done := s.storage(ctx, "count")
		n, err := s.backend.Count(ctx, q.filter(), q.opOptions())
		done(err)
		return n, err
	}

	key := ""
	if s.caches(q.Session) && s.cacheFind {
		// Limit, Skip, Sort and Project do not change a count, so they are not
		// in its key: two counts differing only in the page they belong to
		// share one entry.
		key = s.key(ctx, "count", queryDigest(Query{Where: q.Where, Deleted: q.Deleted}))
	}
	if key == "" {
		n, err := read(ctx)
		o2.count(n)
		return n, err
	}
	if raw, hit := s.hit(ctx, key); hit {
		if n, err := strconv.ParseInt(string(raw), 10, 64); err == nil {
			o2.count(n)
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
		o2.count(n)
		s.store(ctx, key, strconv.AppendInt(nil, n, 10))
		return n, nil
	})
}

// ============================================================
// Writes
// ============================================================

// Create stamps the envelope, applies [Defaulter], validates, and inserts.
// The returned document is the stored one, id and all.
func (s *Service[T, PT]) Create(ctx context.Context, doc T, options ...Option) (created T, err error) {
	o := applyOptions(options)
	ctx, o2 := s.begin(ctx, "create", o.session)
	defer func() { s.end(o2, err, "create") }()
	ctx, cancel := s.deadline(ctx, 1)
	defer cancel()

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
	s.document(ctx, o2, p.envelope().ID)
	sctx, done := s.storage(ctx, "insert")
	err = s.backend.Insert(sctx, p.envelope().ID, raw, OpOptions{Session: o.session})
	done(err)
	if err != nil {
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

func (s *Service[T, PT]) patch(ctx context.Context, id string, o opts, produce func(T, json.RawMessage) (T, error)) (doc T, err error) {
	var zero T
	if id == "" {
		return zero, ErrNotFound
	}
	ctx, o2 := s.begin(ctx, "patch", o.session, "id", id)
	defer func() { s.end(o2, err, "patch", "id", id) }()
	s.document(ctx, o2, id)
	ctx, cancel := s.deadline(ctx, 1)
	defer cancel()

	where := M{IDPath: id}
	if !o.deleted {
		where = active(where)
	}

	for attempt := range patchAttempts {
		// The attempt is set as it goes, not at the end: a patch that is still
		// retrying when the deadline fires leaves the count behind it.
		o2.set("howl.db.attempts", attempt+1)
		sctx, done := s.storage(ctx, "find_one")
		raw, err := s.backend.FindOne(sctx, where, OpOptions{Session: o.session})
		done(err)
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

		// The paths the diff produced, not the caller's intent: a patch that
		// changed nothing writes nothing, and the span says which it was.
		s.changed(ctx, o2, set)
		wctx, wdone := s.storage(ctx, "update_paths")
		stored, err := s.backend.UpdatePaths(wctx, id, set, UpdateOptions{
			OpOptions:       OpOptions{Session: o.session},
			ExpectedVersion: envelope.Version,
			Unset:           unset,
		})
		wdone(err)
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
func (s *Service[T, PT]) Delete(ctx context.Context, id string, options ...Option) (err error) {
	if id == "" {
		return ErrNotFound
	}
	o := applyOptions(options)
	ctx, o2 := s.begin(ctx, "delete", o.session, "id", id, "hard", o.hard)
	defer func() { s.end(o2, err, "delete", "id", id, "hard", o.hard) }()
	s.document(ctx, o2, id)
	o2.set("howl.db.hard", o.hard)
	ctx, cancel := s.deadline(ctx, 1)
	defer cancel()

	if o.hard {
		sctx, done := s.storage(ctx, "delete_one")
		_, err := s.backend.DeleteOne(sctx, id, OpOptions{Session: o.session})
		done(err)
		if err != nil {
			return err
		}
		s.invalidate(ctx, o.session)
		return nil
	}

	// Read first so a second delete answers ErrNotFound instead of restamping
	// a document that is already gone.
	sctx, done := s.storage(ctx, "find_one")
	_, err = s.backend.FindOne(sctx, active(M{IDPath: id}), OpOptions{Session: o.session})
	done(err)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	sctx, done = s.storage(ctx, "update_paths")
	_, err = s.backend.UpdatePaths(sctx, id, map[string]any{
		DeletedAtPath: now,
		DeletedByPath: o.actor,
	}, UpdateOptions{OpOptions: OpOptions{Session: o.session}, NoBump: true})
	done(err)
	if err != nil {
		return err
	}
	s.invalidate(ctx, o.session)
	return nil
}

// Restore clears a soft delete. A document that is not deleted comes back
// unchanged, without a write.
func (s *Service[T, PT]) Restore(ctx context.Context, id string, options ...Option) (doc T, err error) {
	var zero T
	if id == "" {
		return zero, ErrNotFound
	}
	o := applyOptions(options)
	ctx, o2 := s.begin(ctx, "restore", o.session, "id", id)
	defer func() { s.end(o2, err, "restore", "id", id) }()
	s.document(ctx, o2, id)
	ctx, cancel := s.deadline(ctx, 1)
	defer cancel()

	sctx, done := s.storage(ctx, "find_one")
	raw, err := s.backend.FindOne(sctx, M{IDPath: id}, OpOptions{Session: o.session})
	done(err)
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
	sctx, done = s.storage(ctx, "update_paths")
	stored, err := s.backend.UpdatePaths(sctx, id, map[string]any{
		DeletedAtPath: nil,
		DeletedByPath: nil,
		UpdatedAtPath: time.Now().UnixMilli(),
		UpdatedByPath: o.actor,
	}, UpdateOptions{OpOptions: OpOptions{Session: o.session}})
	done(err)
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
func (s *Service[T, PT]) PatchWhere(ctx context.Context, where M, values Set, options ...Option) (n int64, err error) {
	if len(where) == 0 {
		return 0, fmt.Errorf("db: %s: PatchWhere refuses an empty filter", s.name)
	}
	if len(values) == 0 {
		return 0, nil
	}
	o := applyOptions(options)
	ctx, o2 := s.begin(ctx, "patch_where", o.session)
	defer func() { s.end(o2, err, "patch_where") }()
	s.query(ctx, o2, where)
	s.changed(ctx, o2, values)
	ctx, cancel := s.deadline(ctx, bulkTimeoutMult)
	defer cancel()

	paths := make(map[string]any, len(values)+2)
	for path, value := range values {
		if path == IDPath || path == VersionPath {
			continue
		}
		paths[path] = value
	}
	paths[UpdatedAtPath] = time.Now().UnixMilli()
	paths[UpdatedByPath] = o.actor

	n, err = s.bulk(ctx, active(where), paths, UpdateOptions{OpOptions: OpOptions{Session: o.session}})
	if err != nil {
		return 0, err
	}
	o2.count(n)
	s.invalidate(ctx, o.session)
	return n, nil
}

// DeleteWhere soft-deletes every active document matching where, and returns
// how many. An empty filter is refused; [Hard] is not supported here, because
// a bulk hard delete should be a statement you wrote yourself.
func (s *Service[T, PT]) DeleteWhere(ctx context.Context, where M, options ...Option) (n int64, err error) {
	if len(where) == 0 {
		return 0, fmt.Errorf("db: %s: DeleteWhere refuses an empty filter", s.name)
	}
	o := applyOptions(options)
	ctx, o2 := s.begin(ctx, "delete_where", o.session)
	defer func() { s.end(o2, err, "delete_where") }()
	s.query(ctx, o2, where)
	ctx, cancel := s.deadline(ctx, bulkTimeoutMult)
	defer cancel()

	n, err = s.bulk(ctx, active(where), map[string]any{
		DeletedAtPath: time.Now().UnixMilli(),
		DeletedByPath: o.actor,
	}, UpdateOptions{OpOptions: OpOptions{Session: o.session}, NoBump: true})
	if err != nil {
		return 0, err
	}
	o2.count(n)
	s.invalidate(ctx, o.session)
	return n, nil
}

// bulk runs a set-wide write through the backend's [BulkWriter] when it has
// one, and otherwise walks the matching ids. The fallback is sequential
// rather than concurrent: without knowing the backend's connection pool,
// fanning out is how a bulk over ten thousand rows becomes an outage
// somewhere else.
func (s *Service[T, PT]) bulk(ctx context.Context, where M, paths map[string]any, o UpdateOptions) (int64, error) {
	// Which of the two ran is the difference between one statement and one
	// per row, so it goes on the span: a set-wide write that is slow on a
	// backend without a BulkWriter is slow for a reason nothing else shows.
	if writer, ok := s.backend.(BulkWriter); ok {
		op(ctx).set("howl.db.bulk", "native")
		sctx, done := s.storage(ctx, "update_paths_where")
		n, err := writer.UpdatePathsWhere(sctx, where, paths, o)
		done(err)
		return n, err
	}
	op(ctx).set("howl.db.bulk", "fallback")
	sctx, done := s.storage(ctx, "find_many")
	rows, err := s.backend.FindMany(sctx, where, FindOptions{OpOptions: o.OpOptions})
	done(err)
	if err != nil {
		return 0, err
	}
	var n int64
	// One storage span for the whole walk, not one per row: a thousand
	// documents is a thousand statements, and a span each would bury the
	// trace it is meant to explain. The count says how many there were.
	sctx, done = s.storage(ctx, "update_paths")
	for _, raw := range rows {
		doc, err := decode[T, PT](raw)
		if err != nil {
			done(err)
			return n, err
		}
		if _, err := s.backend.UpdatePaths(sctx, PT(&doc).envelope().ID, paths, o); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // deleted between the read and the write
			}
			done(err)
			return n, err
		}
		n++
	}
	done(nil)
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

// opSpan is one operation, from the caller's point of view: the whole call,
// cache and decoding included. What it collects that a bare span cannot is the
// per-call counts — cache lookups and documents — which have to be tallied as
// the operation runs and set once at the end, because GetMany of fifty ids
// does fifty lookups and fifty span events is not a readable trace.
type opSpan struct {
	span         observe.Span
	start        time.Time
	hits, misses int
	rows         int64
	hasRows      bool
}

type opKey struct{}

// op returns the operation the context is inside, or nil at the top level —
// which is where a direct call from a test or a script starts.
func op(ctx context.Context) *opSpan {
	o, _ := ctx.Value(opKey{}).(*opSpan)
	return o
}

func (o *opSpan) set(key string, value any) {
	if o != nil {
		o.span.Set(key, value)
	}
}

// count records how many documents the operation answered with. It is an
// attribute, never a metric label: "8" and "8000" are the interesting
// difference, and neither belongs in a time series name.
func (o *opSpan) count(n int64) {
	if o != nil {
		o.rows, o.hasRows = n, true
	}
}

// query records what the operation was asked to find, and set records what it
// changed — the filter that chose the documents and the fields written to
// them. Both go through the service's Mode: field names and operators always,
// values only when they cannot be personal data. Neither is built at all
// unless something is listening.
func (s *Service[T, PT]) query(ctx context.Context, o *opSpan, v any) {
	if s.recording(ctx) {
		o.set("db.query.text", observe.Render(v, s.mode))
	}
}

// recording is asked before anything is built for a span. A Query has to be
// flattened into a map before it can be rendered, and doing that for a process
// with no tracer is an allocation per read that nothing ever reads.
func (s *Service[T, PT]) recording(ctx context.Context) bool {
	return s.mode != observe.Off && observe.Enabled(ctx)
}

func (s *Service[T, PT]) changed(ctx context.Context, o *opSpan, v any) {
	if s.recording(ctx) {
		o.set("howl.db.set", observe.Render(v, s.mode))
	}
}

// document records which document the operation was about. An id is the one
// value worth keeping by name: it is how the row is found again, in the
// database and in the next trace. A caller whose ids are email addresses gets
// "?" instead, by the same rule as everything else.
func (s *Service[T, PT]) document(ctx context.Context, o *opSpan, id string) {
	if s.recording(ctx) {
		o.set("howl.db.id", observe.Text(id, s.mode))
	}
}

// begin opens the span for one operation. The span carries the collection, the
// backend and the operation, so a trace shows the database as itself rather
// than as a gap under the handler; the debug log stays as the collector-less
// view of the same thing.
func (s *Service[T, PT]) begin(ctx context.Context, name string, session any, args ...any) (context.Context, *opSpan) {
	ctx, span := observe.Start(ctx, "db."+name+" "+s.name)
	span.Set(observe.Kind, "db")
	span.Set("db.system.name", s.backend.Prefix())
	span.Set("db.collection.name", s.name)
	span.Set("db.operation.name", name)
	// Inside a transaction the cache is not consulted in either direction, so
	// a hit rate that looks wrong for a collection written transactionally is
	// this, not a broken cache.
	if session != nil {
		span.Set("howl.db.session", true)
	}
	o := &opSpan{span: span, start: time.Now()}
	return context.WithValue(ctx, opKey{}, o), o
}

// end closes the operation, publishing the counts it gathered. The cache
// figures are attributes rather than events for the same reason they are
// counted at all: one operation, one line in the trace, however many lookups
// it took.
func (s *Service[T, PT]) end(o *opSpan, err error, name string, args ...any) {
	if o.hits > 0 {
		o.span.Set("howl.cache.hits", o.hits)
	}
	if o.misses > 0 {
		o.span.Set("howl.cache.misses", o.misses)
	}
	if o.hasRows {
		o.span.Set("howl.db.rows", o.rows)
	}
	o.span.End(err)
	s.trace(o.start, name, args...)
}

// storage wraps one call into the backend, so the operation's own cost —
// validating, decoding, building keys, waiting on another caller's query —
// is the difference between this span and its parent. Without it a slow
// unmarshal and a slow database look identical.
func (s *Service[T, PT]) storage(ctx context.Context, name string) (context.Context, func(error)) {
	ctx, span := observe.Start(ctx, s.backend.Prefix()+"."+name+" "+s.name)
	span.Set(observe.Kind, "db.storage")
	span.Set("db.system.name", s.backend.Prefix())
	span.Set("db.collection.name", s.name)
	span.Set("db.operation.name", name)
	return ctx, span.End
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
	o := op(ctx)
	if ok {
		s.stats.hits.Add(1)
		if o != nil {
			o.hits++
		}
	} else {
		s.stats.misses.Add(1)
		if o != nil {
			o.misses++
		}
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
