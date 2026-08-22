package db

import (
	"context"
	"encoding/json"
)

// Backend is the contract a storage engine implements. Everything
// interesting — validation, the envelope, soft delete, optimistic locking,
// cache invalidation, timeouts — is in [Service]; a backend supplies these
// collection-level operations over the neutral filter grammar and nothing
// else.
//
// It carries no type parameter. Documents cross this boundary as JSON, so a
// backend is one plain implementation shared by every collection in the
// process, instead of one instantiation per Go type. That is the one place
// this port is simpler than the TypeScript original rather than the same.
//
// Documents in and out are public-shaped: the "id" key is present in the JSON
// even where storage keeps it in a separate column. Attaching and stripping
// it is the backend's job, because only the backend knows where the key
// lives.
type Backend interface {
	// Prefix is the cache-key namespace this backend writes under ("sql",
	// "mongo", "mem"). [Service] builds every key with it, and checks it
	// against a shared cache adapter's own prefix at construction — a silent
	// mismatch would make invalidation miss every entry.
	Prefix() string

	// NewID generates a document id.
	NewID() string

	// Insert stores one document. The JSON carries the id as well.
	Insert(ctx context.Context, id string, doc json.RawMessage, o OpOptions) error

	// FindOne returns the first document matching where, or [ErrNotFound].
	FindOne(ctx context.Context, where M, o OpOptions) (json.RawMessage, error)

	// FindMany returns every document matching where, honouring sort, limit
	// and skip.
	FindMany(ctx context.Context, where M, o FindOptions) ([]json.RawMessage, error)

	// Count returns the number of documents matching where.
	Count(ctx context.Context, where M, o OpOptions) (int64, error)

	// UpdatePaths deep-sets dotted document paths on one document, atomically
	// with the version bump and the optional version check. It returns the
	// document as it is after the update, or [ErrNotFound] when nothing
	// matched — an absent id and a failed version check are the same answer
	// here, and [Service] tells them apart by re-reading.
	//
	// Deep-set, not set: a path whose parent object is missing must create the
	// parent, or a patch to a field the stored document has never had is
	// silently dropped.
	UpdatePaths(ctx context.Context, id string, paths map[string]any, o UpdateOptions) (json.RawMessage, error)

	// DeleteOne hard-deletes one document, returning it, or [ErrNotFound].
	DeleteOne(ctx context.Context, id string, o OpOptions) (json.RawMessage, error)

	// UnsetField removes a top-level JSON key from every document that has
	// it, returning how many were changed. It is storage maintenance below the
	// service contract — no validation, no version bump, no audit — and exists
	// to reclaim an orphan field.
	UnsetField(ctx context.Context, field string, o OpOptions) (int64, error)
}

// OpOptions are the options common to every backend operation.
type OpOptions struct {
	// Session is an opaque backend handle — a *sql.Tx for the SQL backends.
	// Service threads it through without interpreting it, and skips the cache
	// for any operation that carries one: a read inside an uncommitted
	// transaction must not become visible to anyone else.
	Session any
}

// FindOptions are the options for [Backend.FindMany].
type FindOptions struct {
	OpOptions
	Sort  Sort
	Limit int
	Skip  int
}

// UpdateOptions are the options for [Backend.UpdatePaths].
type UpdateOptions struct {
	OpOptions
	// ExpectedVersion is the optimistic lock: update only if the stored
	// version still equals this. Zero means no check, which is why versions
	// start at 1.
	ExpectedVersion int64
	// NoBump suppresses the version increment. Soft delete passes it — a
	// delete is not an edit, and bumping would invalidate the caller's handle
	// on a document they are about to restore.
	NoBump bool
	// Unset are dotted paths to remove from the document, applied after the
	// deep-sets. A deep-set cannot express a removal, and without this a field
	// a patch cleared would keep its stored value forever.
	Unset []string
}

// BulkWriter is the optional capability for set-wide writes: apply the same
// paths to every match in one statement, instead of [Service]'s per-document
// read-modify-write.
//
// It sits below the per-document contract — no per-row read, no optimistic
// lock, no validation. [Service] feature-detects it and falls back to a
// bounded concurrent loop over the matching ids.
type BulkWriter interface {
	UpdatePathsWhere(ctx context.Context, where M, paths map[string]any, o UpdateOptions) (int64, error)
}

// SchemaAdmin is the optional capability for promoted-column introspection
// and orphan cleanup. A document store with no column concept does not
// implement it, and the corresponding [Service] methods answer
// [ErrUnsupported].
//
// Promoted-column DDL is additive: removing a path from a backend's promote
// list stops queries routing to its column but leaves the column and its
// index physically there, maintained on every write for no read benefit. This
// is how an operator finds those and drops them. Creating columns stays in
// code — the config is the source of truth — so this surface is deliberately
// introspect-and-clean-up only.
type SchemaAdmin interface {
	Columns(ctx context.Context) ([]Column, error)
	DropColumn(ctx context.Context, column string, purgeData bool) error
}

// Column is one promoted column physically present in storage.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Declared reports whether the live config still asks for this column.
	// False means it is an orphan, and the only kind
	// [SchemaAdmin.DropColumn] will remove.
	Declared bool `json:"declared"`
}

// KeyCounter is the optional capability behind an exact [Service.Report]: for
// every top-level JSON key across the active documents, how many documents
// have it, and how many active documents there are.
//
// The TypeScript service samples 50 documents and infers. A backend that can
// answer this in one grouped query — Postgres can, with jsonb_object_keys —
// turns that estimate into a fact, which matters when the report is what you
// press "backfill" against.
type KeyCounter interface {
	KeyCounts(ctx context.Context, o OpOptions) (counts map[string]int64, total int64, err error)
}
