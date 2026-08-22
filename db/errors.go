package db

import (
	"errors"
	"fmt"
)

// The errors every backend and every caller agree on. They are sentinels
// rather than types because the only thing a caller does with them is choose
// a status code:
//
//	u, err := users.Get(ctx, id)
//	if errors.Is(err, db.ErrNotFound) {
//		return u, api.NotFound("no such user")
//	}
var (
	// ErrNotFound is returned by every read and write addressed by id when no
	// document matched — including a document that exists but is soft-deleted
	// and was not asked for with [Deleted].
	ErrNotFound = errors.New("db: document not found")

	// ErrConflict is a lost optimistic lock: the document changed between the
	// read and the write of a patch, and the retry lost too. Map it to 409.
	ErrConflict = errors.New("db: version conflict")

	// ErrInvalid wraps whatever a [Validator] returned. Map it to 400 — the
	// wrapped error is the caller's own message and is safe to show.
	ErrInvalid = errors.New("db: invalid document")

	// ErrUnsupported is returned when an operation needs a backend capability
	// this backend does not have (schema introspection on a document store
	// with no columns, for instance).
	ErrUnsupported = errors.New("db: unsupported by this backend")
)

// errNotAnObject means storage returned something that is not a JSON object
// where a document belongs. It is a corruption report, not a caller error.
var errNotAnObject = errors.New("db: stored value is not a JSON object")

// invalid wraps a validator's error so errors.Is finds ErrInvalid and
// errors.Unwrap still reaches the original message.
func invalid(collection string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrInvalid, collection, err)
}
