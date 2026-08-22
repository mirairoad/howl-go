// Package sqlite stores a [db] collection in SQLite: one JSON document per
// row, plus a typed generated column for every promoted path.
//
//	users, err := sqlite.New[User](ctx, conn, sqlite.Options{
//		Collection: "users",
//		Unique:     []string{"email"},
//		Promote:    []sqlite.Promote{{Path: "plan"}, {Path: "seats", Type: sqlite.Bigint}},
//	})
//
// This is the bottom rung of the ladder: no server, no container, a file or
// :memory:. The options literal is the same one [github.com/mirairoad/howl-go/db/pg]
// takes, so moving up is changing sqlite.New to pg.New — that is what the
// shared contract is for, and the conformance suite is what keeps it true.
//
// # Two things about database/sql and SQLite
//
// SQLite is not a server, and *sql.DB is a connection pool, which combine
// badly in one specific way: each pooled connection to ":memory:" gets its own
// empty database, so a table created on one connection is invisible to the
// next. Either pin the pool or name the database:
//
//	conn.SetMaxOpenConns(1)                       // simplest, and correct
//	sql.Open("sqlite", "file::memory:?cache=shared")
//
// For a file database the same pool will happily produce SQLITE_BUSY under
// concurrent writes — SQLite has one writer. WAL is enabled at construction,
// which helps readers; a busy timeout is a driver setting and belongs in the
// DSN.
//
// # Requirements
//
// SQLite 3.35 or newer, for RETURNING. Every current driver bundles something
// far newer.
package sqlite

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mirairoad/howl-go/db"
)

// The promoted-column vocabulary is shared with the other SQL backends, so a
// collection moves from SQLite to Postgres by changing the constructor and
// leaving the options literal alone.
type (
	Promote = db.Promote
	Index   = db.Index
)

// Options configure a collection. Only Collection is required.
type Options struct {
	// Collection is the table name.
	Collection string
	// Unique promotes these paths to columns with a unique index. This is
	// where a "one account per email" rule belongs: enforced by the database,
	// not by a read-then-write in the application.
	Unique []string
	// Promote lifts paths into typed generated columns.
	Promote []Promote
	// Indexes are additional indexes, including composite ones.
	Indexes []Index
	// Cache configures query-result caching. The zero value disables it, and
	// on this backend that is usually the right answer: a read is a local
	// microsecond, so a cache mostly buys invalidation bugs.
	Cache db.Cache
	// Timeout bounds every operation. Default 30s.
	Timeout time.Duration
	// Log receives the debug records, including the DDL this package chooses
	// to skip. Defaults to slog.Default().
	Log *slog.Logger
	// Debug logs every operation with its duration.
	Debug bool
}

// Service is a SQLite-backed collection: the whole [db.Service] contract, plus
// the SQL escape hatch.
type Service[T any, PT db.Document[T]] struct {
	*db.Service[T, PT]
	backend *Backend
}

// New creates the table if it does not exist, applies the promoted-column
// DDL, and returns the service.
//
// It runs DDL, so it takes a context and returns an error rather than being a
// package-level construction. In an application that reads:
//
//	var Users = must(sqlite.New[User](context.Background(), conn, sqlite.Options{…}))
func New[T any, PT db.Document[T]](ctx context.Context, conn Conn, o Options) (*Service[T, PT], error) {
	if conn == nil {
		return nil, fmt.Errorf("sqlite: %s: conn is nil", o.Collection)
	}
	if _, err := assertIdent(o.Collection); err != nil {
		return nil, err
	}
	promoted, err := buildColumns(o)
	if err != nil {
		return nil, err
	}

	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	backend := &Backend{
		conn:     conn,
		table:    o.Collection,
		promoted: promoted,
		unique:   set(o.Unique),
		indexes:  o.Indexes,
		log:      log,
	}
	if err := backend.ensure(ctx); err != nil {
		return nil, err
	}

	service, err := db.NewService[T, PT](backend, db.Options{
		Collection: o.Collection,
		Cache:      o.Cache,
		Timeout:    o.Timeout,
		Log:        o.Log,
		Debug:      o.Debug,
	})
	if err != nil {
		return nil, err
	}
	return &Service[T, PT]{Service: service, backend: backend}, nil
}

// SQL is the escape hatch: the connection, for the queries the grammar does
// not cover. Anything written through it is backend-specific and bypasses the
// contract — no soft-delete filter, no validation, no cache invalidation.
func (s *Service[T, PT]) SQL() Conn { return s.backend.conn }

// buildColumns turns the options into the promoted-column map. version and
// meta.deleted_at are always promoted: one carries the optimistic lock and the
// other the filter every read applies, and neither should ever depend on
// somebody remembering to list it.
func buildColumns(o Options) (map[string]column, error) {
	columns := map[string]column{}
	add := func(path string, typ Type, name string) error {
		segments := strings.Split(path, ".")
		for _, segment := range segments {
			if _, err := assertIdent(segment); err != nil {
				return err
			}
		}
		if name == "" {
			name = strings.ReplaceAll(path, ".", "_")
		}
		if _, err := assertIdent(name); err != nil {
			return err
		}
		if name == "id" || name == "doc" {
			return fmt.Errorf("%w (path %q)", errReservedColumn, path)
		}
		columns[path] = column{name: name, segments: segments, typ: typ}
		return nil
	}

	if err := add(db.VersionPath, Bigint, ""); err != nil {
		return nil, err
	}
	if err := add(db.DeletedAtPath, Bigint, "deleted_at"); err != nil {
		return nil, err
	}
	for _, field := range o.Unique {
		if err := add(field, Text, ""); err != nil {
			return nil, err
		}
	}
	for _, promote := range o.Promote {
		typ := promote.Type
		if typ == "" {
			typ = Text
		}
		if err := add(promote.Path, typ, promote.Column); err != nil {
			return nil, err
		}
	}
	return columns, nil
}

func set(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}
