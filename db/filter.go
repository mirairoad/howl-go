package db

// M is a filter: field names (or dot-paths) mapped to a value for equality,
// or to an operator object. It is the same document the TypeScript service
// sends, so a filter can be pasted between the two:
//
//	db.M{"status": "active", "score": db.M{"$gt": 10}}
//
// The constructors below build the same thing with the operator names spelled
// by the compiler instead of by hand, which is the difference between a typo
// that fails to compile and one that silently matches nothing:
//
//	db.And(db.Eq("status", "active"), db.Gt("score", 10))
//
// Both spellings compile to identical SQL. Mix them freely.
type M map[string]any

// The supported operators. This is the empirically-used subset of Mongo's
// query language and nothing more: it is the largest grammar that compiles to
// an indexable SQL predicate without a query planner of its own. $regex,
// $elemMatch, aggregations and text search are out — they belong behind a
// backend's escape hatch, where the caller can see they are uncached and
// backend-specific.
const (
	OpEq     = "$eq"
	OpNe     = "$ne"
	OpIn     = "$in"
	OpNin    = "$nin"
	OpGt     = "$gt"
	OpGte    = "$gte"
	OpLt     = "$lt"
	OpLte    = "$lte"
	OpExists = "$exists"
	OpOr     = "$or"
	OpAnd    = "$and"
)

// Eq matches documents whose field equals v. A nil v matches both a stored
// JSON null and an absent key, which is Mongo's rule and the one the soft
// delete filter depends on.
func Eq(field string, v any) M { return M{field: M{OpEq: v}} }

// Ne matches documents whose field differs from v, including documents that
// do not have the field at all.
func Ne(field string, v any) M { return M{field: M{OpNe: v}} }

// In matches documents whose field equals any element of values.
func In[E any](field string, values []E) M { return M{field: M{OpIn: anySlice(values)}} }

// Nin matches documents whose field equals no element of values — including
// documents that do not have the field.
func Nin[E any](field string, values []E) M { return M{field: M{OpNin: anySlice(values)}} }

// Gt matches documents whose field is greater than v.
func Gt(field string, v any) M { return M{field: M{OpGt: v}} }

// Gte matches documents whose field is greater than or equal to v.
func Gte(field string, v any) M { return M{field: M{OpGte: v}} }

// Lt matches documents whose field is less than v.
func Lt(field string, v any) M { return M{field: M{OpLt: v}} }

// Lte matches documents whose field is less than or equal to v.
func Lte(field string, v any) M { return M{field: M{OpLte: v}} }

// Exists matches on key presence. It is not the same question as Eq(field,
// nil): a stored JSON null is present.
func Exists(field string, present bool) M { return M{field: M{OpExists: present}} }

// And matches documents matching every branch. Use it when two conditions
// address the same field — a single M can only hold one entry per key.
func And(branches ...M) M { return M{OpAnd: branches} }

// Or matches documents matching at least one branch. An empty Or matches
// nothing, which is what the SQL compiles to.
func Or(branches ...M) M { return M{OpOr: branches} }

func anySlice[E any](values []E) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// active returns where with the soft-delete condition added, unless the
// caller already constrained meta.deleted_at themselves. Every read and every
// bulk write goes through this: soft delete is the default, and the condition
// is index-backed because meta.deleted_at is a promoted column with a partial
// index on it.
func active(where M) M {
	out := make(M, len(where)+1)
	for k, v := range where {
		out[k] = v
	}
	if _, taken := out[DeletedAtPath]; !taken {
		out[DeletedAtPath] = nil
	}
	return out
}

// The paths the service writes and filters on by name. They are dotted
// document paths, not Go field names — the backend never sees Go types.
// Exported because a backend needs them too: the SQL backends promote
// VersionPath and DeletedAtPath into generated columns, which is what makes
// the optimistic lock and the soft-delete filter index-backed.
const (
	IDPath        = "id"
	VersionPath   = "version"
	MetaPath      = "meta"
	DeletedAtPath = "meta.deleted_at"
	DeletedByPath = "meta.deleted_by"
	UpdatedAtPath = "meta.updated_at"
	UpdatedByPath = "meta.updated_by"
)

// SortKey is one term of an ORDER BY: a field name or dot-path, and a
// direction.
type SortKey struct {
	Field string
	Desc  bool
}

// Sort is an ordered list of sort terms. Build it by chaining, so a single
// term needs no slice literal:
//
//	Sort: db.Desc("meta.created_at")
//	Sort: db.Desc("score").Asc("name")
type Sort []SortKey

// Asc starts a sort ascending by field.
func Asc(field string) Sort { return Sort{{Field: field}} }

// Desc starts a sort descending by field.
func Desc(field string) Sort { return Sort{{Field: field, Desc: true}} }

// Asc appends an ascending term.
func (s Sort) Asc(field string) Sort { return append(s, SortKey{Field: field}) }

// Desc appends a descending term.
func (s Sort) Desc(field string) Sort { return append(s, SortKey{Field: field, Desc: true}) }
