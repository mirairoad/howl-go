package pg

import (
	"testing"

	"github.com/mirairoad/howl-go/db"
)

// The routing decision is the whole compiler: a promoted path must reach its
// column and an unpromoted one must not. Asserting on the SQL text is the
// only way to see which happened — both answers are correct, and only one is
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
			want:   `"org_id" = $1`,
			params: []any{"acme"},
		},
		{
			name:   "unpromoted path compiles to JSONB",
			where:  db.Eq("nickname", "ada"),
			want:   `doc->'nickname' = $1::jsonb`,
			params: []any{`"ada"`},
		},
		{
			name:   "dot path reaches into the document",
			where:  db.Eq("profile.plan", "pro"),
			want:   `doc->'profile'->'plan' = $1::jsonb`,
			params: []any{`"pro"`},
		},
		{
			name:   "id hits the primary key",
			where:  db.M{"id": "u_1"},
			want:   `id = $1`,
			params: []any{"u_1"},
		},
		{
			name:   "the soft-delete filter is a column IS NULL",
			where:  active(),
			want:   `"deleted_at" IS NULL`,
			params: nil,
		},
		{
			name:   "null on a JSONB path matches a stored null and an absent key",
			where:  db.Eq("nickname", nil),
			want:   `(doc->'nickname' IS NULL OR doc->'nickname' = 'null'::jsonb)`,
			params: nil,
		},
		{
			name:   "ne matches absent fields too",
			where:  db.Ne("org_id", "acme"),
			want:   `"org_id" IS DISTINCT FROM $1`,
			params: []any{"acme"},
		},
		{
			name:   "in binds one parameter per value",
			where:  db.In("org_id", []string{"a", "b"}),
			want:   `"org_id" IN ($1, $2)`,
			params: []any{"a", "b"},
		},
		{
			name:   "nin lets absent fields through",
			where:  db.Nin("org_id", []string{"a"}),
			want:   `("org_id" IS NULL OR "org_id" NOT IN ($1))`,
			params: []any{"a"},
		},
		{
			name:   "exists asks the document, not the column",
			where:  db.Exists("org_id", true),
			want:   `doc->'org_id' IS NOT NULL`,
			params: nil,
		},
		{
			name:   "two operators on one field",
			where:  db.M{"score": db.M{db.OpGte: 10, db.OpLt: 20}},
			want:   `(doc->'score' >= $1::jsonb AND doc->'score' < $2::jsonb)`,
			params: []any{"10", "20"},
		},
		{
			name:   "and",
			where:  db.And(db.Eq("org_id", "acme"), db.Gt("score", 1)),
			want:   `(("org_id" = $1) AND (doc->'score' > $2::jsonb))`,
			params: []any{"acme", "1"},
		},
		{
			name:   "or",
			where:  db.Or(db.Eq("org_id", "a"), db.Eq("org_id", "b")),
			want:   `(("org_id" = $1) OR ("org_id" = $2))`,
			params: []any{"a", "b"},
		},
		{
			name:  "an empty filter is TRUE, not an error",
			where: db.M{},
			want:  `TRUE`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			text, params, err := compileWhere(c.where, promoted, 1)
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
				if got, want := toString(params[i]), toString(c.params[i]); got != want {
					t.Errorf("param %d = %v, want %v", i+1, params[i], c.params[i])
				}
			}
		})
	}
}

// Parameters are numbered from an offset so a WHERE clause can follow a SET
// clause in the same statement — the bulk writers depend on it.
func TestCompileNumbersFromTheGivenOffset(t *testing.T) {
	text, params, err := compileWhere(db.Eq("name", "ada"), nil, 4)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if text != `doc->'name' = $4::jsonb` {
		t.Errorf("SQL = %s", text)
	}
	if len(params) != 1 {
		t.Fatalf("params = %v", params)
	}
}

func TestCompileRejectsWhatItCannotIndex(t *testing.T) {
	if _, _, err := compileWhere(db.M{"name": db.M{"$regex": "^a"}}, nil, 1); err == nil {
		t.Error("compiled $regex; it is not in the grammar")
	}
	if _, _, err := compileWhere(db.M{"name; DROP TABLE users": "x"}, nil, 1); err == nil {
		t.Error("compiled an identifier that would escape quoting")
	}
	if _, _, err := compileWhere(db.M{"$where": "1=1"}, nil, 1); err == nil {
		t.Error("compiled an unknown top-level operator")
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

// The generated column has to read the same path the filter compiler routes
// to it, or the index answers a different question than the query asks.
func TestGeneratedExpressionMatchesThePath(t *testing.T) {
	got := generatedExpr(column{name: "deleted_at", segments: []string{"meta", "deleted_at"}, typ: Bigint})
	want := `(((doc #>> '{meta,deleted_at}'))::bigint)`
	if got != want {
		t.Errorf("expression = %s, want %s", got, want)
	}
	got = generatedExpr(column{name: "email", segments: []string{"email"}, typ: Text})
	if want := `(doc #>> '{email}')`; got != want {
		t.Errorf("expression = %s, want %s", got, want)
	}
}

// active mirrors what db.Service adds to every read.
func active() db.M { return db.M{db.DeletedAtPath: nil} }

func toString(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	default:
		return ""
	}
}
