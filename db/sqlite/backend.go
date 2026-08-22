package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mirairoad/howl-go/db"
)

// Conn is the database handle this package drives. *sql.DB and *sql.Tx both
// satisfy it, so the package needs no driver of its own — bring
// modernc.org/sqlite (pure Go) or mattn/go-sqlite3 (cgo), or anything else
// registered with database/sql.
type Conn interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Backend stores one collection as a table: a JSON document per row, plus a
// typed generated column for every promoted path.
//
// The same hybrid as the Postgres backend, in SQLite's dialect. The
// differences are three, and each one is a place the code had to change
// rather than a preference:
//
//   - Generated columns are VIRTUAL, because ALTER TABLE cannot add a STORED
//     one. They are still indexable, which is all the promote mechanism needs.
//   - There is no ADD COLUMN IF NOT EXISTS, so the existing columns are read
//     first and only the missing ones are added.
//   - Updates merge through json_patch rather than a recursive deep-set:
//     RFC 7386 already creates missing parent objects, which is the whole
//     reason Postgres needed a plpgsql function.
type Backend struct {
	conn     Conn
	table    string
	promoted map[string]column
	unique   map[string]bool
	indexes  []db.Index
	log      *slog.Logger
}

// The document as callers see it: the row's id lifted back into the JSON.
// Storage keeps it in the primary key and out of doc — one copy, and the
// column is what the index is on.
const selectDoc = `json_set(doc, '$.id', id)`

// Prefix is the cache namespace. It differs from the Postgres backend's
// because the two are not interchangeable at the key level: a document cached
// from a local file and one cached from a server are different documents, and
// a shared cache must not confuse them.
func (b *Backend) Prefix() string { return "sqlite" }

// NewID returns a UUIDv7.
func (b *Backend) NewID() string { return db.NewID() }

func (b *Backend) exec(o db.OpOptions) Conn {
	if conn, ok := o.Session.(Conn); ok {
		return conn
	}
	return b.conn
}

// ============================================================
// Table shape
// ============================================================

func (b *Backend) ensure(ctx context.Context) error {
	version := b.promoted[db.VersionPath]
	deleted := b.promoted[db.DeletedAtPath]

	// WAL improves single-writer concurrency for a file database and is a
	// no-op for :memory:. It is an optimisation, so a read-only file or an
	// exotic VFS must not stop the process from starting.
	b.ddl(ctx, "PRAGMA journal_mode = WAL")

	if _, err := b.conn.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id TEXT PRIMARY KEY,
			doc TEXT NOT NULL CHECK (json_valid(doc)),
			%s,
			%s
		)`, b.quoted(), b.columnSQL(version), b.columnSQL(deleted))); err != nil {
		return fmt.Errorf("sqlite: creating table %s: %w", b.table, err)
	}

	// The soft-delete condition rides on every read, so it gets a partial
	// index: the rows it excludes are not in it at all.
	b.ddl(ctx, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %s (id) WHERE %q IS NULL`,
		b.table+"_active_idx", b.quoted(), deleted.name))
	b.ddl(ctx, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %s (%q)`,
		b.table+"_"+version.name+"_idx", b.quoted(), version.name))

	// SQLite has no ADD COLUMN IF NOT EXISTS. Reading what is already there
	// beats adding blindly and treating "duplicate column name" as success:
	// that string is the only thing separating idempotency from a real error.
	existing, err := b.physicalColumns(ctx)
	if err != nil {
		return err
	}
	for path, promoted := range b.promoted {
		if path == db.VersionPath || path == db.DeletedAtPath {
			continue
		}
		if !existing[promoted.name] {
			b.ddl(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s`, b.quoted(), b.columnSQL(promoted)))
		}
		kind, suffix := "INDEX", "idx"
		if b.unique[path] {
			kind, suffix = "UNIQUE INDEX", "key"
		}
		b.ddl(ctx, fmt.Sprintf(`CREATE %s IF NOT EXISTS %q ON %s (%q)`,
			kind, b.table+"_"+promoted.name+"_"+suffix, b.quoted(), promoted.name))
	}
	for _, index := range b.indexes {
		if err := b.ensureIndex(ctx, index); err != nil {
			return err
		}
	}
	return nil
}

// columnSQL is the generated-column definition. VIRTUAL, not STORED: ALTER
// TABLE refuses to add a STORED generated column, and a promote list that
// only worked on a table created from scratch would not be a promote list.
// Virtual columns are indexable, which is the only property this needs.
func (b *Backend) columnSQL(c column) string {
	path, err := jsonPath(c.segments)
	if err != nil {
		return ""
	}
	return fmt.Sprintf(`%q %s GENERATED ALWAYS AS (doc->>%s) VIRTUAL`, c.name, affinity(c.typ), path)
}

func (b *Backend) ensureIndex(ctx context.Context, index db.Index) error {
	terms := make([]string, 0, len(index.Keys))
	parts := make([]string, 0, len(index.Keys))
	for _, key := range index.Keys {
		expr, err := b.orderExpr(key.Field)
		if err != nil {
			return err
		}
		if key.Desc {
			expr += " DESC"
		}
		terms = append(terms, expr)
		parts = append(parts, strings.ReplaceAll(key.Field, ".", "_"))
	}
	name := index.Name
	if name == "" {
		name = b.table + "_" + strings.Join(parts, "_") + "_idx"
	}
	if _, err := assertIdent(name); err != nil {
		return err
	}
	kind := "INDEX"
	if index.Unique {
		kind = "UNIQUE INDEX"
	}
	b.ddl(ctx, fmt.Sprintf(`CREATE %s IF NOT EXISTS %q ON %s (%s)`,
		kind, name, b.quoted(), strings.Join(terms, ", ")))
	return nil
}

// physicalColumns reads the columns the table actually has. table_xinfo and
// not table_info, because a generated column is hidden and table_info does
// not report it — which would make every promoted column look missing and be
// re-added on every start.
func (b *Backend) physicalColumns(ctx context.Context) (map[string]bool, error) {
	rows, err := b.conn.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_xinfo(%q)`, b.table))
	if err != nil {
		return nil, fmt.Errorf("sqlite: %s: reading columns: %w", b.table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		column, _, err := scanXInfo(rows)
		if err != nil {
			return nil, err
		}
		out[column.Name] = true
	}
	return out, rows.Err()
}

// ddl runs a statement whose failure must not take the process down. A
// promoted column that cannot be added — because an earlier deploy created it
// with a different expression, say — degrades to filtering through the
// document, which is slower and still correct. Losing the table is a
// different matter, and that is not routed here.
func (b *Backend) ddl(ctx context.Context, statement string) {
	if _, err := b.conn.ExecContext(ctx, statement); err != nil {
		b.log.Debug("sqlite: DDL skipped", "table", b.table, "error", err, "statement", statement)
	}
}

func (b *Backend) quoted() string { return `"` + b.table + `"` }

// ============================================================
// Reads
// ============================================================

func (b *Backend) FindOne(ctx context.Context, where db.M, o db.OpOptions) (json.RawMessage, error) {
	text, params, err := compileWhere(where, b.promoted)
	if err != nil {
		return nil, err
	}
	rows, err := b.exec(o).QueryContext(ctx,
		"SELECT "+selectDoc+" FROM "+b.quoted()+" WHERE "+text+" LIMIT 1", params...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: %s: %w", b.table, err)
	}
	defer rows.Close()
	docs, err := scan(rows)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, db.ErrNotFound
	}
	return docs[0], nil
}

func (b *Backend) FindMany(ctx context.Context, where db.M, o db.FindOptions) ([]json.RawMessage, error) {
	text, params, err := compileWhere(where, b.promoted)
	if err != nil {
		return nil, err
	}
	query := "SELECT " + selectDoc + " FROM " + b.quoted() + " WHERE " + text
	order, err := b.orderBy(o.Sort)
	if err != nil {
		return nil, err
	}
	query += order
	if o.Limit > 0 {
		params = append(params, o.Limit)
		query += " LIMIT ?"
	}
	if o.Skip > 0 {
		// SQLite only accepts OFFSET after a LIMIT. -1 is its documented
		// "no limit" sentinel, so a skip without a limit still works.
		if o.Limit <= 0 {
			query += " LIMIT -1"
		}
		params = append(params, o.Skip)
		query += " OFFSET ?"
	}

	rows, err := b.exec(o.OpOptions).QueryContext(ctx, query, params...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: %s: %w", b.table, err)
	}
	defer rows.Close()
	return scan(rows)
}

func (b *Backend) Count(ctx context.Context, where db.M, o db.OpOptions) (int64, error) {
	text, params, err := compileWhere(where, b.promoted)
	if err != nil {
		return 0, err
	}
	rows, err := b.exec(o).QueryContext(ctx,
		"SELECT COUNT(*) FROM "+b.quoted()+" WHERE "+text, params...)
	if err != nil {
		return 0, fmt.Errorf("sqlite: %s: %w", b.table, err)
	}
	defer rows.Close()
	var n int64
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
	}
	return n, rows.Err()
}

// orderBy spells out the null placement instead of taking the dialect's
// default. SQLite sorts NULLs first ascending and Postgres sorts them last, so
// a query that ordered by a field some documents lack would return a different
// page from each backend — the one thing a shared contract cannot allow.
func (b *Backend) orderBy(sort db.Sort) (string, error) {
	if len(sort) == 0 {
		return "", nil
	}
	terms := make([]string, 0, len(sort))
	for _, key := range sort {
		expr, err := b.orderExpr(key.Field)
		if err != nil {
			return "", err
		}
		if key.Desc {
			expr += " DESC NULLS FIRST"
		} else {
			expr += " ASC NULLS LAST"
		}
		terms = append(terms, expr)
	}
	return " ORDER BY " + strings.Join(terms, ", "), nil
}

func (b *Backend) orderExpr(path string) (string, error) {
	if path == "id" {
		return "id", nil
	}
	if promoted, ok := b.promoted[path]; ok {
		return `"` + promoted.name + `"`, nil
	}
	literal, err := jsonPath(strings.Split(path, "."))
	if err != nil {
		return "", err
	}
	return "doc->>" + literal, nil
}

// ============================================================
// Writes
// ============================================================

func (b *Backend) Insert(ctx context.Context, id string, doc json.RawMessage, o db.OpOptions) error {
	// json_remove strips the id in the statement rather than in Go: it is
	// already the primary key, and a second copy inside the document is a
	// second thing that can disagree.
	_, err := b.exec(o).ExecContext(ctx,
		"INSERT INTO "+b.quoted()+" (id, doc) VALUES (?, json_remove(json(?), '$.id'))", id, string(doc))
	if err != nil {
		return fmt.Errorf("sqlite: %s: insert: %w", b.table, err)
	}
	return nil
}

func (b *Backend) UpdatePaths(ctx context.Context, id string, paths map[string]any, o db.UpdateOptions) (json.RawMessage, error) {
	expr, params, err := b.setExpr(paths, o)
	if err != nil {
		return nil, err
	}
	params = append(params, id)
	where := "id = ?"
	if o.ExpectedVersion != 0 {
		params = append(params, o.ExpectedVersion)
		where += fmt.Sprintf(" AND %q = ?", b.promoted[db.VersionPath].name)
	}

	rows, err := b.exec(o.OpOptions).QueryContext(ctx,
		"UPDATE "+b.quoted()+" SET doc = "+expr+" WHERE "+where+" RETURNING "+selectDoc, params...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: %s: update: %w", b.table, err)
	}
	defer rows.Close()
	docs, err := scan(rows)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, db.ErrNotFound
	}
	return docs[0], nil
}

// UpdatePathsWhere is the [db.BulkWriter] capability: the same merge applied
// to every match in one statement, with no per-row round trip.
func (b *Backend) UpdatePathsWhere(ctx context.Context, where db.M, paths map[string]any, o db.UpdateOptions) (int64, error) {
	expr, params, err := b.setExpr(paths, o)
	if err != nil {
		return 0, err
	}
	text, filterParams, err := compileWhere(where, b.promoted)
	if err != nil {
		return 0, err
	}
	// SQLite's placeholders are positional, so the SET parameters simply come
	// first — there is no offset to thread through the compiler.
	result, err := b.exec(o.OpOptions).ExecContext(ctx,
		"UPDATE "+b.quoted()+" SET doc = "+expr+" WHERE "+text, append(params, filterParams...)...)
	if err != nil {
		return 0, fmt.Errorf("sqlite: %s: bulk update: %w", b.table, err)
	}
	return result.RowsAffected()
}

// setExpr builds the nested json expression for one update.
//
// Non-null values merge through json_patch, which is RFC 7386 and therefore
// creates missing parent objects for free — the behaviour Postgres needed a
// recursive plpgsql function for. The catch is the other half of RFC 7386: a
// null member means REMOVE THE KEY, and the envelope stores real nulls
// (meta.deleted_at on an active document). So null paths cannot ride the
// patch; they are chained as explicit json_set(…, null) afterwards.
//
// A value that arrives as the JSON literal null counts as null here too. It is
// how a nil pointer field reaches this function, and letting it through the
// patch would delete the key instead of setting it.
func (b *Backend) setExpr(paths map[string]any, o db.UpdateOptions) (string, []any, error) {
	merge := map[string]any{}
	var nulls []string
	for _, path := range sortedKeys(paths) {
		encoded, err := json.Marshal(paths[path])
		if err != nil {
			return "", nil, err
		}
		if paths[path] == nil || string(encoded) == "null" {
			nulls = append(nulls, path)
			continue
		}
		if err := nest(merge, path, json.RawMessage(encoded)); err != nil {
			return "", nil, err
		}
	}

	encoded, err := json.Marshal(merge)
	if err != nil {
		return "", nil, err
	}
	params := []any{string(encoded)}
	expr := "json_patch(doc, json(?))"

	for _, path := range nulls {
		literal, err := jsonPath(strings.Split(path, "."))
		if err != nil {
			return "", nil, err
		}
		expr = "json_set(" + expr + ", " + literal + ", null)"
	}
	for _, path := range o.Unset {
		literal, err := jsonPath(strings.Split(path, "."))
		if err != nil {
			return "", nil, err
		}
		expr = "json_remove(" + expr + ", " + literal + ")"
	}
	if !o.NoBump {
		// The right-hand doc is the pre-update row value, so the increment is
		// atomic within the statement.
		expr = "json_set(" + expr + ", '$.version', (doc->>'$.version') + 1)"
	}
	return expr, params, nil
}

// nest rebuilds the nested object json_patch expects from a dotted leaf path.
func nest(root map[string]any, path string, value any) error {
	segments := strings.Split(path, ".")
	for _, segment := range segments {
		if _, err := assertIdent(segment); err != nil {
			return err
		}
	}
	node := root
	for _, segment := range segments[:len(segments)-1] {
		child, _ := node[segment].(map[string]any)
		if child == nil {
			child = map[string]any{}
			node[segment] = child
		}
		node = child
	}
	node[segments[len(segments)-1]] = value
	return nil
}

func (b *Backend) DeleteOne(ctx context.Context, id string, o db.OpOptions) (json.RawMessage, error) {
	rows, err := b.exec(o).QueryContext(ctx,
		"DELETE FROM "+b.quoted()+" WHERE id = ? RETURNING "+selectDoc, id)
	if err != nil {
		return nil, fmt.Errorf("sqlite: %s: delete: %w", b.table, err)
	}
	defer rows.Close()
	docs, err := scan(rows)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, db.ErrNotFound
	}
	return docs[0], nil
}

func (b *Backend) UnsetField(ctx context.Context, field string, o db.OpOptions) (int64, error) {
	if _, err := assertIdent(field); err != nil {
		return 0, err
	}
	path := "'$." + field + "'"
	result, err := b.exec(o).ExecContext(ctx,
		"UPDATE "+b.quoted()+" SET doc = json_remove(doc, "+path+") "+
			"WHERE json_type(doc, "+path+") IS NOT NULL")
	if err != nil {
		return 0, fmt.Errorf("sqlite: %s: unset %s: %w", b.table, field, err)
	}
	return result.RowsAffected()
}

// ============================================================
// Schema introspection
// ============================================================

// KeyCounts answers [db.KeyCounter] in one grouped query over json_each,
// which is what makes a schema report exact instead of sampled.
func (b *Backend) KeyCounts(ctx context.Context, o db.OpOptions) (map[string]int64, int64, error) {
	deleted := b.promoted[db.DeletedAtPath].name
	conn := b.exec(o)

	rows, err := conn.QueryContext(ctx, fmt.Sprintf(
		`SELECT key, COUNT(*) FROM %s, json_each(doc) WHERE %q IS NULL GROUP BY key`,
		b.quoted(), deleted))
	if err != nil {
		return nil, 0, fmt.Errorf("sqlite: %s: key counts: %w", b.table, err)
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var key string
		var n int64
		if err := rows.Scan(&key, &n); err != nil {
			return nil, 0, err
		}
		counts[key] = n
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	totals, err := conn.QueryContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %q IS NULL`, b.quoted(), deleted))
	if err != nil {
		return nil, 0, fmt.Errorf("sqlite: %s: key counts: %w", b.table, err)
	}
	defer totals.Close()
	var total int64
	if totals.Next() {
		if err := totals.Scan(&total); err != nil {
			return nil, 0, err
		}
	}
	// The id lives in its own column, so it is not among the document's keys —
	// report it as present on every row, which is what it is.
	counts["id"] = total
	return counts, total, totals.Err()
}

// Columns implements [db.SchemaAdmin]: the generated columns physically
// present, each flagged declared or orphan.
func (b *Backend) Columns(ctx context.Context) ([]db.Column, error) {
	rows, err := b.conn.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_xinfo(%q)`, b.table))
	if err != nil {
		return nil, fmt.Errorf("sqlite: %s: listing columns: %w", b.table, err)
	}
	defer rows.Close()

	declared := b.declaredColumns()
	var out []db.Column
	for rows.Next() {
		column, generated, err := scanXInfo(rows)
		if err != nil {
			return nil, err
		}
		if !generated {
			continue // id and doc are ordinary columns, not promoted paths
		}
		column.Declared = declared[column.Name]
		out = append(out, column)
	}
	return out, rows.Err()
}

// DropColumn implements [db.SchemaAdmin]. SQLite refuses to drop a column an
// index depends on, so the convention-named indexes go first.
func (b *Backend) DropColumn(ctx context.Context, name string, purgeData bool) error {
	if _, err := assertIdent(name); err != nil {
		return err
	}
	if b.declaredColumns()[name] {
		return fmt.Errorf("sqlite: %s: %q is still declared — remove it from Promote or Unique first", b.table, name)
	}
	b.ddl(ctx, fmt.Sprintf(`DROP INDEX IF EXISTS %q`, b.table+"_"+name+"_idx"))
	b.ddl(ctx, fmt.Sprintf(`DROP INDEX IF EXISTS %q`, b.table+"_"+name+"_key"))
	if _, err := b.conn.ExecContext(ctx,
		fmt.Sprintf(`ALTER TABLE %s DROP COLUMN %q`, b.quoted(), name)); err != nil {
		return fmt.Errorf("sqlite: %s: dropping %s: %w", b.table, name, err)
	}
	if purgeData {
		path := "'$." + name + "'"
		if _, err := b.conn.ExecContext(ctx,
			"UPDATE "+b.quoted()+" SET doc = json_remove(doc, "+path+") "+
				"WHERE json_type(doc, "+path+") IS NOT NULL"); err != nil {
			return fmt.Errorf("sqlite: %s: purging %s: %w", b.table, name, err)
		}
	}
	return nil
}

func (b *Backend) declaredColumns() map[string]bool {
	out := make(map[string]bool, len(b.promoted))
	for _, c := range b.promoted {
		out[c.name] = true
	}
	return out
}

// ============================================================
// Plumbing
// ============================================================

// scanXInfo reads one PRAGMA table_xinfo row. The pragma's shape is
// (cid, name, type, notnull, dflt_value, pk, hidden), and hidden is what
// separates an ordinary column (0) from a generated one (2 VIRTUAL, 3 STORED).
// Scanning into any keeps it working across drivers that type the columns
// differently.
func scanXInfo(rows *sql.Rows) (db.Column, bool, error) {
	var (
		cid, notnull, pk, hidden int
		name, typ                string
		dflt                     any
	)
	if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk, &hidden); err != nil {
		return db.Column{}, false, fmt.Errorf("sqlite: reading table_xinfo: %w", err)
	}
	return db.Column{Name: name, Type: typ}, hidden == 2 || hidden == 3, nil
}

func scan(rows *sql.Rows) ([]json.RawMessage, error) {
	var out []json.RawMessage
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(raw))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

var errReservedColumn = errors.New("sqlite: cannot promote to the reserved columns id or doc")
