package db

// Option is a per-operation knob for the operations addressed by id, where a
// config struct would cost more than it explains — Get(ctx, id) has to stay
// two arguments. The operations that take a whole query take [Query] instead,
// which is the shape core/app and core/api use for anything with more than a
// couple of fields.
type Option func(*opts)

type opts struct {
	actor   string
	deleted bool
	hard    bool
	session any
}

// The identity every write is stamped with when the caller does not say. It
// matches the TypeScript service's default so a shared table reads the same
// from both.
const systemActor = "system"

func applyOptions(list []Option) opts {
	o := opts{actor: systemActor}
	for _, fn := range list {
		fn(&o)
	}
	return o
}

// By records who is performing the write in meta.created_by, meta.updated_by
// or meta.deleted_by. Without it the write is attributed to "system".
func By(actor string) Option {
	return func(o *opts) {
		if actor != "" {
			o.actor = actor
		}
	}
}

// Deleted includes soft-deleted documents. On a read it lifts the filter
// every read otherwise carries; on a patch it allows editing a deleted
// document instead of answering [ErrNotFound].
func Deleted() Option { return func(o *opts) { o.deleted = true } }

// Hard makes Delete a real delete: the row goes, and Restore cannot bring it
// back.
func Hard() Option { return func(o *opts) { o.hard = true } }

// Session runs the operation on a backend-specific handle — a *sql.Tx for the
// SQL backends. Operations carrying one are never cached, in either
// direction: an uncommitted read must not be published, and a write that may
// still roll back must not evict.
func Session(s any) Option { return func(o *opts) { o.session = s } }

// Query is a find. The zero value finds every active document.
type Query struct {
	// Where is the filter. Nil matches everything.
	Where M
	// Sort orders the results. Nil leaves the order to the backend.
	Sort Sort
	// Limit caps the number of results; 0 means no cap.
	Limit int
	// Skip discards the first n matches.
	Skip int
	// Project restricts which top-level fields (or dot-paths) are decoded.
	//
	// Go cannot return a partial struct, so an omitted field comes back as its
	// zero value and is indistinguishable from a stored empty one. That is why
	// projected results are never written into the by-id cache: a half a
	// document must not be served to a later Get.
	Project []string
	// Deleted includes soft-deleted documents.
	Deleted bool
	// Session is a backend handle; see [Session].
	Session any
}

func (q Query) opOptions() OpOptions { return OpOptions{Session: q.Session} }

func (q Query) findOptions() FindOptions {
	return FindOptions{OpOptions: q.opOptions(), Sort: q.Sort, Limit: q.Limit, Skip: q.Skip}
}

// filter is the query's where clause with the soft-delete condition applied.
func (q Query) filter() M {
	if q.Deleted {
		if q.Where == nil {
			return M{}
		}
		return q.Where
	}
	return active(q.Where)
}

// Set is a sparse update: dotted document paths mapped to their new values.
// It is what the bulk writers take, and what [Service.PatchFields] takes for
// the case where the field names are data rather than code.
//
//	users.PatchWhere(ctx, db.Eq("org_id", org), db.Set{"plan": "pro"})
type Set map[string]any
