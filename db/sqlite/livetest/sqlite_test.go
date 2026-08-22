// Package livetest runs the db conformance suite against real SQLite.
//
// It is a separate Go module for the same reason db/pg/livetest is: the driver
// it needs stays out of the framework's go.mod, which claims to have exactly
// one dependency. Unlike the Postgres suite it needs nothing installed —
// modernc.org/sqlite is pure Go and the database is a temporary file — so
// `make test-db-sqlite` runs anywhere.
package livetest

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mirairoad/howl-go/db"
	"github.com/mirairoad/howl-go/db/conformance"
	"github.com/mirairoad/howl-go/db/sqlite"
)

var table atomic.Int64

func TestConformance(t *testing.T) {
	conn := open(t)
	conformance.Run(t, func(t *testing.T) conformance.Service {
		return service(t, conn, db.Cache{})
	})
}

// The same suite with caching on: every case here already passed uncached, so
// a failure is an invalidation bug and nothing else.
func TestConformanceCached(t *testing.T) {
	conn := open(t)
	conformance.Run(t, func(t *testing.T) conformance.Service {
		return service(t, conn, db.Cache{TTL: time.Minute})
	})
}

// Promoting a path must change which index the planner reaches for and no
// answer at all. Running the whole suite against a promoted table is how that
// claim stays true rather than stated.
func TestConformancePromoted(t *testing.T) {
	conn := open(t)
	conformance.Run(t, func(t *testing.T) conformance.Service {
		return service(t, conn, db.Cache{},
			sqlite.Promote{Path: "kind"}, sqlite.Promote{Path: "score", Type: sqlite.Bigint})
	})
}

// Generated columns are VIRTUAL here because ALTER TABLE cannot add a STORED
// one — so the promote list has to work on a table that already exists, not
// only on one created from scratch.
func TestPromoteOnAnExistingTable(t *testing.T) {
	ctx := context.Background()
	conn := open(t)
	name := fmt.Sprintf("later_%d", table.Add(1))

	first, err := sqlite.New[conformance.Doc](ctx, conn, sqlite.Options{Collection: name})
	if err != nil {
		t.Fatalf("first construction: %v", err)
	}
	doc, err := first.Create(ctx, conformance.Doc{Name: "before the column", Kind: "gadget"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Same table, now with a promoted path: the column has to be added to a
	// table that already holds rows, and the existing document has to be
	// visible through it.
	later, err := sqlite.New[conformance.Doc](ctx, conn, sqlite.Options{
		Collection: name,
		Promote:    []sqlite.Promote{{Path: "kind"}},
	})
	if err != nil {
		t.Fatalf("second construction: %v", err)
	}
	found, err := later.Find(ctx, db.Query{Where: db.Eq("kind", "gadget")})
	if err != nil {
		t.Fatalf("find through the new column: %v", err)
	}
	if len(found) != 1 || found[0].ID != doc.ID {
		t.Fatalf("the promoted column does not see the existing row: %+v", found)
	}

	// Constructing a third time must be a no-op, not a duplicate-column error.
	if _, err := sqlite.New[conformance.Doc](ctx, conn, sqlite.Options{
		Collection: name,
		Promote:    []sqlite.Promote{{Path: "kind"}},
	}); err != nil {
		t.Fatalf("third construction is not idempotent: %v", err)
	}
}

func TestPromotedColumnsAreIntrospectable(t *testing.T) {
	ctx := context.Background()
	conn := open(t)
	s := service(t, conn, db.Cache{}, sqlite.Promote{Path: "kind"})

	columns, err := s.Columns(ctx)
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	found := map[string]db.Column{}
	for _, c := range columns {
		found[c.Name] = c
	}
	for _, name := range []string{"version", "deleted_at", "kind"} {
		c, ok := found[name]
		if !ok {
			t.Errorf("%s is not a generated column: %+v", name, columns)
			continue
		}
		if !c.Declared {
			t.Errorf("%s reported as an orphan while it is still declared", name)
		}
	}
	if _, ok := found["id"]; ok {
		t.Error("id is an ordinary column and must not be reported as promoted")
	}
	if err := s.DropColumn(ctx, "kind", false); err == nil {
		t.Error("dropped a column that is still declared")
	}
}

// Removing a path from Promote leaves the column behind; that orphan is what
// DropColumn exists for. SQLite refuses to drop an indexed column, so this
// also proves the index goes first.
func TestOrphanColumnIsDroppable(t *testing.T) {
	ctx := context.Background()
	conn := open(t)
	name := fmt.Sprintf("orphan_%d", table.Add(1))

	if _, err := sqlite.New[conformance.Doc](ctx, conn, sqlite.Options{
		Collection: name,
		Promote:    []sqlite.Promote{{Path: "kind"}},
	}); err != nil {
		t.Fatalf("first construction: %v", err)
	}
	later, err := sqlite.New[conformance.Doc](ctx, conn, sqlite.Options{Collection: name})
	if err != nil {
		t.Fatalf("second construction: %v", err)
	}

	columns, err := later.Columns(ctx)
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	var orphan bool
	for _, c := range columns {
		if c.Name == "kind" {
			orphan = !c.Declared
		}
	}
	if !orphan {
		t.Fatalf("kind was not reported as an orphan: %+v", columns)
	}
	if err := later.DropColumn(ctx, "kind", false); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	columns, err = later.Columns(ctx)
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	for _, c := range columns {
		if c.Name == "kind" {
			t.Error("the orphan column survived the drop")
		}
	}
}

// A session scopes every operation to one transaction, and a rollback must
// leave nothing behind — including in the cache, which is why session-scoped
// operations never touch it.
func TestSessionRollsBack(t *testing.T) {
	ctx := context.Background()
	conn := open(t)
	s := service(t, conn, db.Cache{TTL: time.Minute})

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	doc, err := s.Create(ctx, conformance.Doc{Name: "rolled back"}, db.Session(tx))
	if err != nil {
		t.Fatalf("create in transaction: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := s.Get(ctx, doc.ID); err == nil {
		t.Error("a rolled-back document is readable")
	}
	n, err := s.Count(ctx, db.Query{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d after a rollback, want 0", n)
	}
}

// open returns a connection to a temporary file database.
//
// A file and not ":memory:" because *sql.DB is a pool and SQLite is not a
// server: each pooled connection to ":memory:" gets its own empty database, so
// a table created on one is invisible to the next. The pool is pinned to a
// single connection anyway, since SQLite has one writer and the tests do not
// need more.
func open(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "conformance.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	conn.SetMaxOpenConns(1)
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// service gives each case its own table, so no case can see another's rows.
func service(t *testing.T, conn *sql.DB, cache db.Cache, promote ...sqlite.Promote) conformance.Service {
	t.Helper()
	name := fmt.Sprintf("conf_%d", table.Add(1))
	s, err := sqlite.New[conformance.Doc](context.Background(), conn, sqlite.Options{
		Collection: name,
		Cache:      cache,
		Promote:    promote,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return s.Service
}
