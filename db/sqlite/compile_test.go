package sqlite

import (
	"testing"

	"github.com/mirairoad/howl-go/db"
)

// The routing decision is the whole compiler: a promoted path must reach its
// column and an unpromoted one must not. Asserting on the SQL text is the only
// way to see which happened — both answers are correct, and only one is
// indexed.
func TestCompileRoutesPromotedPathsToColumns(t *testing.T) {
	promoted := map[string]column{
		db.VersionPath:   {name: "version", segments: []string{"version"}, typ: Bigint},
		db.DeletedAtPath: {name: "deleted_at", segments: []string{"meta", "deleted_at"}, typ: Bigint},
		"org_id":         {name: "org_id", segments: []string{"org_id"}, typ: Text},
	}

	for _, c := range []struct {
		name   string
		where  db.M
		want   string
		params []any
	}{
		{
			name:   "promoted path hits its column",
			where:  db.Eq("org_id", "acme"),
			want:   `"org_id" = ?`,
			params: []any{"acme"},
		},
		{
			name:   "unpromoted path reads the document, natively typed",
			where:  db.Eq("nickname", "ada"),
			want:   `doc->>'$.nickname' = ?`,
			params: []any{"ada"},
		},
		{
			name:   "dot path reaches into the document",
			where:  db.Eq("profile.plan", "pro"),
			want:   `doc->>'$.profile.plan' = ?`,
			params: []any{"pro"},
		},
		{
			name:   "id hits the primary key",
			where:  db.M{"id": "u_1"},
			want:   `id = ?`,
			params: []any{"u_1"},
		},
		{
			// The whole reason this dialect needs no null gymnastics: `->>`
			// maps a stored null and an absent key both to SQL NULL, which is
			// Mongo's rule already.
			name:  "the soft-delete filter is a column IS NULL",
			where: db.M{db.DeletedAtPath: nil},
			want:  `"deleted_at" IS NULL`,
		},
		{
			name:  "null on a document path is the same IS NULL",
			where: db.Eq("nickname", nil),
			want:  `doc->>'$.nickname' IS NULL`,
		},
		{
			name:   "ne matches absent fields too",
			where:  db.Ne("org_id", "acme"),
			want:   `"org_id" IS NOT ?`,
			params: []any{"acme"},
		},
		{
			name:   "numeric comparison needs no cast",
			where:  db.Gte("score", 10),
			want:   `doc->>'$.score' >= ?`,
			params: []any{10},
		},
		{
			name:   "in binds one parameter per value",
			where:  db.In("org_id", []string{"a", "b"}),
			want:   `"org_id" IN (?, ?)`,
			params: []any{"a", "b"},
		},
		{
			name:   "nin lets absent fields through",
			where:  db.Nin("org_id", []string{"a"}),
			want:   `("org_id" IS NULL OR "org_id" NOT IN (?))`,
			params: []any{"a"},
		},
		{
			// `->>` cannot answer this one: it flattens a stored null into the
			// same SQL NULL an absent key produces.
			name:  "exists asks json_type, not the column",
			where: db.Exists("org_id", true),
			want:  `json_type(doc, '$.org_id') IS NOT NULL`,
		},
		{
			name:   "two operators on one field",
			where:  db.M{"score": db.M{db.OpGte: 10, db.OpLt: 20}},
			want:   `(doc->>'$.score' >= ? AND doc->>'$.score' < ?)`,
			params: []any{10, 20},
		},
		{
			name:   "and",
			where:  db.And(db.Eq("org_id", "acme"), db.Gt("score", 1)),
			want:   `(("org_id" = ?) AND (doc->>'$.score' > ?))`,
			params: []any{"acme", 1},
		},
		{
			name:   "or",
			where:  db.Or(db.Eq("org_id", "a"), db.Eq("org_id", "b")),
			want:   `(("org_id" = ?) OR ("org_id" = ?))`,
			params: []any{"a", "b"},
		},
		{
			name:  "an empty filter is true, not an error",
			where: db.M{},
			want:  `1`,
		},
		{
			// SQLite has no boolean storage class, and a JSON true extracts as
			// 1 — so the parameter is converted rather than left to the driver.
			name:   "booleans bind as integers",
			where:  db.Eq("verified", true),
			want:   `doc->>'$.verified' = ?`,
			params: []any{1},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			text, params, err := compileWhere(c.where, promoted)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if text != c.want {
				t.Errorf("SQL  = %s\nwant = %s", text, c.want)
			}
			if len(params) != len(c.params) {
				t.Fatalf("params = %v, want %v", params, c.params)
			}
			for i := range params {
				if params[i] != c.params[i] {
					t.Errorf("param %d = %#v, want %#v", i+1, params[i], c.params[i])
				}
			}
		})
	}
}

func TestCompileRejectsWhatItCannotIndex(t *testing.T) {
	if _, _, err := compileWhere(db.M{"name": db.M{"$regex": "^a"}}, nil); err == nil {
		t.Error("compiled $regex; it is not in the grammar")
	}
	if _, _, err := compileWhere(db.M{"name; DROP TABLE users": "x"}, nil); err == nil {
		t.Error("compiled an identifier that would escape quoting")
	}
	if _, _, err := compileWhere(db.M{"$where": "1=1"}, nil); err == nil {
		t.Error("compiled an unknown top-level operator")
	}
	// An IN list of documents has no indexable form here, and quietly
	// comparing JSON text would be a different question than the caller asked.
	if _, _, err := compileWhere(db.In("profile", []map[string]any{{"plan": "pro"}}), nil); err == nil {
		t.Error("compiled an $in over documents")
	}
}

// A composite operand compares as JSON, the way Mongo compares a
// sub-document: whole, and key-order sensitive.
func TestCompileComparesDocumentsAsJSON(t *testing.T) {
	text, params, err := compileWhere(db.Eq("profile", map[string]any{"plan": "pro"}), nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if want := `doc->'$.profile' = json(?)`; text != want {
		t.Errorf("SQL = %s, want %s", text, want)
	}
	if len(params) != 1 || params[0] != `{"plan":"pro"}` {
		t.Errorf("params = %#v", params)
	}
}

func TestPromotedColumnsAreDerivedFromTheOptions(t *testing.T) {
	columns, err := buildColumns(Options{
		Collection: "users",
		Unique:     []string{"email"},
		Promote:    []Promote{{Path: "profile.plan"}, {Path: "score", Type: Numeric}},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for path, want := range map[string]struct {
		name string
		typ  Type
	}{
		db.VersionPath:   {"version", Bigint},
		db.DeletedAtPath: {"deleted_at", Bigint},
		"email":          {"email", Text},
		"profile.plan":   {"profile_plan", Text},
		"score":          {"score", Numeric},
	} {
		got, ok := columns[path]
		if !ok {
			t.Errorf("%s was not promoted", path)
			continue
		}
		if got.name != want.name || got.typ != want.typ {
			t.Errorf("%s -> %q %s, want %q %s", path, got.name, got.typ, want.name, want.typ)
		}
	}

	if _, err := buildColumns(Options{Collection: "users", Promote: []Promote{{Path: "doc"}}}); err == nil {
		t.Error("promoting to the reserved doc column was accepted")
	}
}

// VIRTUAL, not STORED: ALTER TABLE refuses to add a STORED generated column,
// so a promote list would only ever work on a table created from scratch.
func TestGeneratedColumnsAreVirtual(t *testing.T) {
	b := &Backend{table: "users"}
	got := b.columnSQL(column{name: "deleted_at", segments: []string{"meta", "deleted_at"}, typ: Bigint})
	want := `"deleted_at" INTEGER GENERATED ALWAYS AS (doc->>'$.meta.deleted_at') VIRTUAL`
	if got != want {
		t.Errorf("column  = %s\nwant    = %s", got, want)
	}
	got = b.columnSQL(column{name: "email", segments: []string{"email"}, typ: Text})
	if want := `"email" TEXT GENERATED ALWAYS AS (doc->>'$.email') VIRTUAL`; got != want {
		t.Errorf("column = %s, want %s", got, want)
	}
}

// Ordering spells out the null placement, because SQLite's default is the
// opposite of Postgres's and a shared contract cannot have two answers.
func TestOrderBySpellsOutNullPlacement(t *testing.T) {
	b := &Backend{table: "users", promoted: map[string]column{
		"score": {name: "score", segments: []string{"score"}, typ: Bigint},
	}}
	got, err := b.orderBy(db.Desc("score").Asc("name"))
	if err != nil {
		t.Fatalf("order by: %v", err)
	}
	want := ` ORDER BY "score" DESC NULLS FIRST, doc->>'$.name' ASC NULLS LAST`
	if got != want {
		t.Errorf("order = %s\nwant  = %s", got, want)
	}
}
