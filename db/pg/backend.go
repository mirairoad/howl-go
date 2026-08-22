package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/mirairoad/howl-go/db"
)

// Conn is the database handle this package drives. *sql.DB and *sql.Tx both
// satisfy it, which is the whole dependency story: the package needs no
// driver of its own, and an application brings whichever one it already has
// (pgx/stdlib, lib/pq, or anything else registered with database/sql).
type Conn interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Backend stores one collection as a table: a JSONB document per row, plus a
// typed generated column for every promoted path.
//
// Not pure-generic JSONB, because a JSONB path has no planner statistics and
// no range index. Not a column per field, because then every added field is a
// migration. The hybrid gives real indexes exactly where somebody said they
// were needed, and leaves the rest of the document free to change without
// touching the table.
type Backend struct {
	conn     Conn
	table    string
	promoted map[string]column
	unique   map[string]bool
	indexes  []Index
	log      *slog.Logger
}

// The document as callers see it: the row's id lifted back into the JSON.
// Storage keeps it in the primary key and out of doc — one copy, and the
// column is what the index is on.
const selectDoc = `jsonb_set(doc, '{id}', to_jsonb(id))`

// The deep-set every update goes through. jsonb_set alone does not create
// missing intermediate objects, so a patch to a path whose parent the stored
// document lacks would be silently dropped — a lost write, and the kind that
// only shows up for documents written before the field existed.
const deepSetFunction = `
CREATE OR REPLACE FUNCTION howl_jsonb_deep_set(target jsonb, path text[], val jsonb)
RETURNS jsonb AS $$
BEGIN
  IF array_length(path, 1) = 1 THEN
    RETURN jsonb_set(COALESCE(target, '{}'::jsonb), path, val, true);
  END IF;
  RETURN jsonb_set(
    COALESCE(target, '{}'::jsonb),
    path[1:1],
    howl_jsonb_deep_set(
      CASE WHEN jsonb_typeof(target -> path[1]) = 'object'
        THEN target -> path[1] ELSE '{}'::jsonb END,
      path[2:],
      val
    ),
    true
  );
END;
$$ LANGUAGE plpgsql IMMUTABLE;`

// Prefix is the cache namespace. It is "sql" and not "pg" so that a shared
// cache stays compatible with the TypeScript SQL backends writing the same
// keys against the same tables.
func (b *Backend) Prefix() string { return "sql" }

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

	if _, err := b.conn.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id TEXT PRIMARY KEY,
			doc JSONB NOT NULL,
			%q BIGINT GENERATED ALWAYS AS %s STORED,
			%q BIGINT GENERATED ALWAYS AS %s STORED
		)`, b.quoted(), version.name, generatedExpr(version), deleted.name, generatedExpr(deleted))); err != nil {
		return fmt.Errorf("pg: creating table %s: %w", b.table, err)
	}
	if _, err := b.conn.ExecContext(ctx, deepSetFunction); err != nil {
		return fmt.Errorf("pg: creating howl_jsonb_deep_set: %w", err)
	}

	// The soft-delete condition rides on every read, so it gets a partial
	// index: the rows it excludes are not in it at all.
	b.ddl(ctx, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %q ON %s (id) WHERE %q IS NULL`,
		b.table+"_active_idx", b.quoted(), deleted.name))

	for path, promoted := range b.promoted {
		if path == db.VersionPath || path == db.DeletedAtPath {
			continue
		}
		b.ensureColumn(ctx, promoted, b.unique[path])
	}
	for _, index := range b.indexes {
		if err := b.ensureIndex(ctx, index); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) ensureColumn(ctx context.Context, c column, unique bool) {
	b.ddl(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS %q %s GENERATED ALWAYS AS %s STORED`,
		b.quoted(), c.name, strings.ToUpper(string(c.typ)), generatedExpr(c)))

	kind, suffix := "INDEX", "idx"
	if unique {
		kind, suffix = "UNIQUE INDEX", "key"
	}
	b.ddl(ctx, fmt.Sprintf(`CREATE %s IF NOT EXISTS %q ON %s (%q)`,
		kind, b.table+"_"+c.name+"_"+suffix, b.quoted(), c.name))
}

func (b *Backend) ensureIndex(ctx context.Context, index Index) error {
	terms := make([]string, 0, len(index.Keys))
	parts := make([]string, 0, len(index.Keys))
	for _, key := range index.Keys {
		direction := ""
		if key.Desc {
			direction = " DESC"
		}
		expr, err := b.orderExpr(key.Field)
		if err != nil {
			return err
		}
		terms = append(terms, expr+direction)
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

// ddl runs a statement whose failure must not take the process down. A
// promoted column that cannot be added — because an earlier deploy created it
// with a different expression, say — degrades to filtering through JSONB,
// which is slower and still correct. Losing the table or the deep-set
// function is a different matter, and those are not routed here.
func (b *Backend) ddl(ctx context.Context, statement string) {
	if _, err := b.conn.ExecContext(ctx, statement); err != nil {
		b.log.Debug("pg: DDL skipped", "table", b.table, "error", err, "statement", statement)
	}
}

func (b *Backend) quoted() string { return `"` + b.table + `"` }

func generatedExpr(c column) string {
	// Spelled exactly as the TypeScript backend spells it, so a table created
	// by either side keeps the same generated-column definition.
	text := "(doc #>> '{" + strings.Join(c.segments, ",") + "}')"
	if c.typ == Text {
		return text
	}
	return "((" + text + ")::" + string(c.typ) + ")"
}

// ============================================================
// Reads
// ============================================================

func (b *Backend) FindOne(ctx context.Context, where db.M, o db.OpOptions) (json.RawMessage, error) {
	text, params, err := compileWhere(where, b.promoted, 1)
	if err != nil {
		return nil, err
	}
	rows, err := b.exec(o).QueryContext(ctx,
		"SELECT "+selectDoc+" FROM "+b.quoted()+" WHERE "+text+" LIMIT 1", params...)
	if err != nil {
		return nil, fmt.Errorf("pg: %s: %w", b.table, err)
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
	text, params, err := compileWhere(where, b.promoted, 1)
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
		query += " LIMIT $" + strconv.Itoa(len(params))
	}
	if o.Skip > 0 {
		params = append(params, o.Skip)
		query += " OFFSET $" + strconv.Itoa(len(params))
	}

	rows, err := b.exec(o.OpOptions).QueryContext(ctx, query, params...)
	if err != nil {
		return nil, fmt.Errorf("pg: %s: %w", b.table, err)
	}
	defer rows.Close()
	return scan(rows)
}

func (b *Backend) Count(ctx context.Context, where db.M, o db.OpOptions) (int64, error) {
	text, params, err := compileWhere(where, b.promoted, 1)
	if err != nil {
		return 0, err
	}
	rows, err := b.exec(o).QueryContext(ctx,
		"SELECT COUNT(*) FROM "+b.quoted()+" WHERE "+text, params...)
	if err != nil {
		return 0, fmt.Errorf("pg: %s: %w", b.table, err)
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

// orderBy spells out the null placement rather than taking the dialect's
// default. It happens to match Postgres's, but SQLite's is the opposite, and a
// query ordered by a field some documents lack must not return a different
// page depending on which backend answered it.
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
	segments := strings.Split(path, ".")
	for _, segment := range segments {
		if _, err := assertIdent(segment); err != nil {
			return "", err
		}
	}
	return "(doc #> '{" + strings.Join(segments, ",") + "}')", nil
}

// ============================================================
// Writes
// ============================================================

func (b *Backend) Insert(ctx context.Context, id string, doc json.RawMessage, o db.OpOptions) error {
	// `- 'id'` strips the key in the statement rather than in Go: the id is
	// already the primary key, and a second copy inside the document is a
	// second thing that can disagree.
	_, err := b.exec(o).ExecContext(ctx,
		"INSERT INTO "+b.quoted()+" (id, doc) VALUES ($1, $2::jsonb - 'id')", id, string(doc))
	if err != nil {
		return fmt.Errorf("pg: %s: insert: %w", b.table, err)
	}
	return nil
}

func (b *Backend) UpdatePaths(ctx context.Context, id string, paths map[string]any, o db.UpdateOptions) (json.RawMessage, error) {
	params := []any{id}
	expr, err := b.setExpr(paths, o, &params)
	if err != nil {
		return nil, err
	}
	where := "id = $1"
	if o.ExpectedVersion != 0 {
		params = append(params, o.ExpectedVersion)
		where += fmt.Sprintf(" AND %q = $%d", b.promoted[db.VersionPath].name, len(params))
	}

	rows, err := b.exec(o.OpOptions).QueryContext(ctx,
		"UPDATE "+b.quoted()+" SET doc = "+expr+" WHERE "+where+" RETURNING "+selectDoc, params...)
	if err != nil {
		return nil, fmt.Errorf("pg: %s: update: %w", b.table, err)
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

// UpdatePathsWhere is the [db.BulkWriter] capability: the same deep-set
// applied to every match in one statement, with no per-row round trip.
func (b *Backend) UpdatePathsWhere(ctx context.Context, where db.M, paths map[string]any, o db.UpdateOptions) (int64, error) {
	var params []any
	expr, err := b.setExpr(paths, o, &params)
	if err != nil {
		return 0, err
	}
	// The filter's parameters follow the SET clause's, so the compiler starts
	// numbering after them.
	text, filterParams, err := compileWhere(where, b.promoted, len(params)+1)
	if err != nil {
		return 0, err
	}
	result, err := b.exec(o.OpOptions).ExecContext(ctx,
		"UPDATE "+b.quoted()+" SET doc = "+expr+" WHERE "+text, append(params, filterParams...)...)
	if err != nil {
		return 0, fmt.Errorf("pg: %s: bulk update: %w", b.table, err)
	}
	return result.RowsAffected()
}

// setExpr builds the nested jsonb expression for one update: the deep-sets,
// then the removals, then the version bump. The bump reads `doc` — the
// pre-update row value — so the increment is atomic within the statement.
func (b *Backend) setExpr(paths map[string]any, o db.UpdateOptions, params *[]any) (string, error) {
	expr := "doc"
	for _, path := range sortedKeys(paths) {
		segments := strings.Split(path, ".")
		for _, segment := range segments {
			if _, err := assertIdent(segment); err != nil {
				return "", err
			}
		}
		encoded, err := json.Marshal(paths[path])
		if err != nil {
			return "", err
		}
		*params = append(*params, string(encoded))
		expr = fmt.Sprintf("howl_jsonb_deep_set(%s, '{%s}', $%d::jsonb)",
			expr, strings.Join(segments, ","), len(*params))
	}
	for _, path := range o.Unset {
		segments := strings.Split(path, ".")
		for _, segment := range segments {
			if _, err := assertIdent(segment); err != nil {
				return "", err
			}
		}
		expr = "(" + expr + ") #- '{" + strings.Join(segments, ",") + "}'"
	}
	if !o.NoBump {
		expr = fmt.Sprintf("jsonb_set(%s, '{version}', to_jsonb(((doc->>'version'))::bigint + 1), true)", expr)
	}
	return expr, nil
}

func (b *Backend) DeleteOne(ctx context.Context, id string, o db.OpOptions) (json.RawMessage, error) {
	rows, err := b.exec(o).QueryContext(ctx,
		"DELETE FROM "+b.quoted()+" WHERE id = $1 RETURNING "+selectDoc, id)
	if err != nil {
		return nil, fmt.Errorf("pg: %s: delete: %w", b.table, err)
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
	// jsonb_exists rather than the `?` operator: some database/sql drivers
	// treat a bare ? as a placeholder and rewrite it.
	result, err := b.exec(o).ExecContext(ctx,
		"UPDATE "+b.quoted()+" SET doc = doc - $1 WHERE jsonb_exists(doc, $1)", field)
	if err != nil {
		return 0, fmt.Errorf("pg: %s: unset %s: %w", b.table, field, err)
	}
	return result.RowsAffected()
}

// ============================================================
// Schema introspection
// ============================================================

// KeyCounts answers [db.KeyCounter] in one grouped query, which is what makes
// a schema report exact instead of sampled.
func (b *Backend) KeyCounts(ctx context.Context, o db.OpOptions) (map[string]int64, int64, error) {
	deleted := b.promoted[db.DeletedAtPath].name
	conn := b.exec(o)

	rows, err := conn.QueryContext(ctx, fmt.Sprintf(
		`SELECT key, COUNT(*) FROM %s, LATERAL jsonb_object_keys(doc) AS key WHERE %q IS NULL GROUP BY key`,
		b.quoted(), deleted))
	if err != nil {
		return nil, 0, fmt.Errorf("pg: %s: key counts: %w", b.table, err)
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
		return nil, 0, fmt.Errorf("pg: %s: key counts: %w", b.table, err)
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
	rows, err := b.conn.QueryContext(ctx,
		`SELECT column_name, data_type FROM information_schema.columns
		 WHERE table_name = $1 AND is_generated = 'ALWAYS' ORDER BY ordinal_position`, b.table)
	if err != nil {
		return nil, fmt.Errorf("pg: %s: listing columns: %w", b.table, err)
	}
	defer rows.Close()

	declared := b.declaredColumns()
	var out []db.Column
	for rows.Next() {
		var c db.Column
		if err := rows.Scan(&c.Name, &c.Type); err != nil {
			return nil, err
		}
		c.Declared = declared[c.Name]
		out = append(out, c)
	}
	return out, rows.Err()
}

// DropColumn implements [db.SchemaAdmin]. Postgres cascades the dependent
// index, so one DROP COLUMN is the whole operation.
func (b *Backend) DropColumn(ctx context.Context, name string, purgeData bool) error {
	if _, err := assertIdent(name); err != nil {
		return err
	}
	if b.declaredColumns()[name] {
		return fmt.Errorf("pg: %s: %q is still declared — remove it from Promote or Unique first", b.table, name)
	}
	if _, err := b.conn.ExecContext(ctx,
		fmt.Sprintf(`ALTER TABLE %s DROP COLUMN IF EXISTS %q`, b.quoted(), name)); err != nil {
		return fmt.Errorf("pg: %s: dropping %s: %w", b.table, name, err)
	}
	if purgeData {
		if _, err := b.conn.ExecContext(ctx,
			"UPDATE "+b.quoted()+" SET doc = doc - $1 WHERE jsonb_exists(doc, $1)", name); err != nil {
			return fmt.Errorf("pg: %s: purging %s: %w", b.table, name, err)
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

var errReservedColumn = errors.New("pg: cannot promote to the reserved columns id or doc")
