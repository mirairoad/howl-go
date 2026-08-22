// Package db is a document store: one JSON document per row, an audit and
// soft-delete envelope the store maintains, optimistic locking on a version
// counter, and no migration framework — the table shape is fixed, so evolving
// a collection means editing a Go struct.
//
// It is not an ORM. There are no relations, no query builder over tables and
// no code generation. A collection is a struct embedding [Doc]:
//
//	type User struct {
//		db.Doc
//		Email string `json:"email"`
//		Name  string `json:"name"`
//	}
//
//	users, err := pg.New[User](ctx, conn, pg.Options{Collection: "users"})
//	u, err := users.Create(ctx, User{Email: "a@b.com", Name: "Ada"})
//	u, err = users.Patch(ctx, u.ID, func(u *User) { u.Name = "Ada L." })
//
// The filter grammar is the small Mongo-shaped subset a SQL backend can
// compile — $eq $ne $in $nin $gt $gte $lt $lte $or $and $exists plus
// dot-paths. Aggregations, $regex and array operators are deliberately out;
// anything richer goes through the backend's escape hatch, uncached.
//
// # Why a struct is enough
//
// The TypeScript original needs zod, because there the type disappears at run
// time and something has to validate the write boundary. In Go the struct is
// the schema: encoding/json enforces the shape, unknown stored keys are
// ignored on read, and the two things a validator adds beyond that — defaults
// and invariants — are the optional [Defaulter] and [Validator] methods. That
// is the same trade core/api makes for endpoint bodies, for the same reason.
//
// # Wire compatibility
//
// The stored shape matches the TypeScript @hushkey/service-core contract key
// for key: the same envelope names, the same epoch-millisecond timestamps,
// the same `id TEXT PRIMARY KEY, doc JSONB` layout, the same `sql` cache
// namespace. A Go service and a Deno service can drive the same table.
package db

import "time"

// Doc is the envelope the store owns. Embed it in a collection struct; the
// service stamps every field and callers only read them.
//
// It is embedded rather than wrapped so the JSON stays flat — {"id":…,
// "version":…,"meta":{…},"email":…} — which is what makes the stored document
// interchangeable with the TypeScript service's, and what lets a handler
// return the document straight out of an api.Spec response type.
type Doc struct {
	// ID is a UUIDv7: time-ordered, so inserts stay at the right edge of the
	// primary key's B-tree instead of scattering across it.
	ID string `json:"id"`
	// Version is the optimistic lock. It starts at 1 and increments on every
	// patch; a delete does not bump it.
	Version int64 `json:"version"`
	// Meta is the audit and soft-delete record.
	Meta Meta `json:"meta"`
}

// envelope is unexported on purpose: it makes [Document] unsatisfiable from
// outside this package except by embedding Doc, so a collection type cannot
// accidentally opt out of the envelope the service assumes is there.
func (d *Doc) envelope() *Doc { return d }

// Meta is the audit and soft-delete record stamped on every document.
//
// Timestamps are epoch milliseconds rather than time.Time because they are
// also generated columns in Postgres and sort keys in every backend — a
// BIGINT compares and indexes without a parse, and it is the shape the
// TypeScript services already wrote. Use [Meta.Created] and friends to get a
// time.Time.
type Meta struct {
	CreatedAt int64  `json:"created_at"`
	CreatedBy string `json:"created_by"`
	UpdatedAt int64  `json:"updated_at"`
	UpdatedBy string `json:"updated_by"`
	// DeletedAt and DeletedBy are nil while the document is active. They are
	// pointers because the absence is the whole state — a zero timestamp would
	// be indistinguishable from "deleted at the epoch", and the soft-delete
	// filter every read carries tests exactly this for NULL.
	DeletedAt *int64  `json:"deleted_at"`
	DeletedBy *string `json:"deleted_by"`
}

// Created is the creation time.
func (m Meta) Created() time.Time { return time.UnixMilli(m.CreatedAt) }

// Updated is the time of the last patch.
func (m Meta) Updated() time.Time { return time.UnixMilli(m.UpdatedAt) }

// Deleted reports the soft-delete time, and whether the document is deleted
// at all.
func (m Meta) Deleted() (time.Time, bool) {
	if m.DeletedAt == nil {
		return time.Time{}, false
	}
	return time.UnixMilli(*m.DeletedAt), true
}

// IsDeleted reports whether the document is soft-deleted.
func (m Meta) IsDeleted() bool { return m.DeletedAt != nil }

// Document constrains a collection type's pointer. The `*T` term is what lets
// the service take and return values of T while still mutating the envelope,
// and it is what constraint type inference reads to fill PT in — so callers
// write pg.New[User](…), never pg.New[User, *User](…).
type Document[T any] interface {
	*T
	envelope() *Doc
}

// Defaulter fills in a document's zero fields before validation. Implement it
// on the pointer receiver. It runs on create only: a patch already starts
// from the stored document, so re-running defaults there would resurrect a
// field the caller deliberately cleared.
//
// This is zod's .default() with the same timing and none of the machinery.
type Defaulter interface{ Defaults() }

// Validator rejects a document at the write boundary. Implement it on the
// pointer receiver; it runs on create and on every patch, after defaults.
// A returned error becomes [ErrInvalid], which an endpoint maps to a 400.
//
// It is the same method core/api already calls on a request body, so a type
// that is both a stored document and an endpoint body validates once, in one
// place, with one implementation.
type Validator interface{ Validate() error }
