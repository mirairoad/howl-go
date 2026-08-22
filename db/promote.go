package db

// Promote lifts a document path into a typed generated column: a real index
// and real planner statistics, instead of a JSON expression the query planner
// can only guess at.
//
// Promote what a query filters or sorts on. Everything else is better off in
// the document, where changing it costs nothing.
//
// This lives in the neutral package, not in a backend, so that a collection
// moves between the SQL backends by changing the constructor and nothing
// else — sqlite.New to pg.New with the same options literal. That is the
// whole point of the ladder: start on a file, move to a server, keep the
// code. Backends with no column concept ignore it.
type Promote struct {
	// Path is the dotted document path, e.g. "org_id" or "profile.plan".
	Path string
	// Type defaults to [Text].
	Type PromoteType
	// Column overrides the derived column name (the path with dots replaced
	// by underscores).
	Column string
}

// PromoteType is the storage type a promoted column carries. The names are
// Postgres's; SQLite maps them onto its affinities.
type PromoteType string

// The promotable types. Anything else stays in the document, which is not so
// much a limitation as the point: a promoted column exists to be indexed and
// compared, and these are the shapes that index and compare.
const (
	Text    PromoteType = "text"
	Bigint  PromoteType = "bigint"
	Numeric PromoteType = "numeric"
	Boolean PromoteType = "boolean"
)

// Index is an additional index over promoted columns or document paths.
type Index struct {
	// Keys are the indexed fields, in order, with their directions.
	Keys Sort
	// Name defaults to <table>_<fields>_idx.
	Name string
	// Unique makes it a unique index.
	Unique bool
}
