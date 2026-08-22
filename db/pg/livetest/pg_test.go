// Package livetest runs the db conformance suite against a real Postgres
// server.
//
// It is a separate Go module so that the driver it needs stays out of the
// framework's go.mod: db/pg talks to database/sql and nothing else, and a
// test dependency in the root module would quietly make that untrue.
//
//	docker run -d --name howl-pg -p 54329:5432 \
//	  -e POSTGRES_PASSWORD=conf -e POSTGRES_DB=howl_conformance postgres:16-alpine
//	PG_URL=postgres://postgres:conf@localhost:54329/howl_conformance go test ./...
package livetest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/mirairoad/howl-go/db"
	"github.com/mirairoad/howl-go/db/conformance"
	"github.com/mirairoad/howl-go/db/pg"
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

// Promoting a path must not change a single answer — only which index the
// planner reaches for. Running the whole suite against a promoted table is
// how that claim stays true.
func TestConformancePromoted(t *testing.T) {
	conn := open(t)
	conformance.Run(t, func(t *testing.T) conformance.Service {
		return service(t, conn, db.Cache{}, pg.Promote{Path: "kind"}, pg.Promote{Path: "score", Type: pg.Bigint})
	})
}

func TestPromotedColumnsAreIntrospectable(t *testing.T) {
	ctx := context.Background()
	conn := open(t)
	s := service(t, conn, db.Cache{}, pg.Promote{Path: "kind"})

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
			t.Errorf("%s is not a generated column", name)
			continue
		}
		if !c.Declared {
			t.Errorf("%s reported as an orphan while it is still declared", name)
		}
	}
	if err := s.DropColumn(ctx, "kind", false); err == nil {
		t.Error("dropped a column that is still declared")
	}
}

// Removing a path from Promote leaves the column behind; that orphan is what
// DropColumn exists for.
func TestOrphanColumnIsDroppable(t *testing.T) {
	ctx := context.Background()
	conn := open(t)
	name := fmt.Sprintf("orphan_%d", table.Add(1))
	t.Cleanup(func() { conn.Exec(`DROP TABLE IF EXISTS "` + name + `"`) })

	if _, err := pg.New[conformance.Doc](ctx, conn, pg.Options{
		Collection: name,
		Promote:    []pg.Promote{{Path: "kind"}},
	}); err != nil {
		t.Fatalf("first construction: %v", err)
	}
	// The same table, without the promote entry: the column stays.
	later, err := pg.New[conformance.Doc](ctx, conn, pg.Options{Collection: name})
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

func open(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("PG_URL")
	if url == "" {
		t.Skip("PG_URL is not set; skipping the live Postgres suite")
	}
	conn, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping %s: %v", url, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// service gives each case its own table, so no case can see another's rows.
func service(t *testing.T, conn *sql.DB, cache db.Cache, promote ...pg.Promote) conformance.Service {
	t.Helper()
	name := fmt.Sprintf("conf_%d", table.Add(1))
	t.Cleanup(func() { conn.Exec(`DROP TABLE IF EXISTS "` + name + `"`) })

	s, err := pg.New[conformance.Doc](context.Background(), conn, pg.Options{
		Collection: name,
		Cache:      cache,
		Promote:    promote,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return s.Service
}
