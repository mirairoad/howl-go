# The document store

`db` is optional and separate from the framework: a document store with an audit envelope, soft delete, optimistic locking and no migration framework. A collection is a Go struct.

It is not an ORM. There are no relations, no query builder over tables, no code generation, and nothing in `core/` imports it. Reach for it when the alternative is hand-rolling `created_at`/`updated_at`/`deleted_at` and a migration directory for the fourth time; reach for `database/sql` when you want SQL.

## A collection is a struct

```go
package users

import "github.com/mirairoad/howl-go/db"

type User struct {
	db.Doc
	Email string `json:"email"`
	Name  string `json:"name"`
	Plan  string `json:"plan"`
}

// Optional. Runs on create, before validation — this is zod's .default().
func (u *User) Defaults() {
	if u.Plan == "" {
		u.Plan = "free"
	}
}

// Optional. Runs on create and on every patch. A plain error becomes a 400.
func (u *User) Validate() error {
	if !strings.Contains(u.Email, "@") {
		return errors.New("email: not an address")
	}
	return nil
}
```

`db.Doc` is embedded, not wrapped, so the stored JSON stays flat — `{"id":…,"version":…,"meta":{…},"email":…}` — and the document can be returned straight out of an `api.Spec` response type.

There is no schema library, for the same reason `core/api` has none: the struct is the schema. `encoding/json` enforces the shape, and the two things a validator adds beyond that — defaults and invariants — are those two methods. The TypeScript original needs zod because there the type is gone at run time.

## Wiring the storage

```go
var Users = must(pg.New[User](context.Background(), conn, pg.Options{
	Collection: "users",
	Unique:     []string{"email"},                    // unique generated column
	Promote:    []pg.Promote{{Path: "plan"}, {Path: "seats", Type: pg.Bigint}},
	Cache:      db.Cache{TTL: 5 * time.Minute},
}))
```

`conn` is anything satisfying `QueryContext`/`ExecContext` — `*sql.DB`, `*sql.Tx`, so pgx/stdlib or lib/pq, whichever the application already has. The package has no driver dependency of its own.

Domain queries are methods on your package, not on the service:

```go
func ByEmail(ctx context.Context, email string) (User, error) {
	return Users.One(ctx, db.Query{Where: db.Eq("email", email)})
}
```

## The surface

```go
u, err := Users.Create(ctx, User{Email: "a@b.com", Name: "Ada"}, db.By(actor))
u, err := Users.Get(ctx, id)                      // db.ErrNotFound when absent
m, err := Users.GetMany(ctx, ids)                 // map[string]User
xs, err := Users.Find(ctx, db.Query{
	Where: db.And(db.Eq("plan", "pro"), db.Gte("seats", 2)),
	Sort:  db.Desc("meta.created_at"),
	Limit: 20,
})
u, err := Users.One(ctx, db.Query{Where: db.Eq("email", email)})
n, err := Users.Count(ctx, db.Query{Where: db.Eq("plan", "free")})

u, err := Users.Patch(ctx, id, func(u *User) { u.Name = "Ada L." }, db.By(actor))
u, err := Users.PatchFields(ctx, id, db.Set{"profile.plan": "pro"})
err     := Users.Delete(ctx, id, db.By(actor))    // soft; db.Hard() removes the row
u, err  := Users.Restore(ctx, id)

n, err := Users.PatchWhere(ctx, db.Eq("org_id", org), db.Set{"plan": "pro"})
n, err := Users.DeleteWhere(ctx, db.In("id", ids))
```

Options are variadic (`db.By`, `db.Hard()`, `db.Deleted()`, `db.Session(tx)`) for the operations addressed by an id, because `Get(ctx, id)` has to stay two arguments. Anything with a filter takes a `db.Query` struct instead, the same shape as `app.Config` and `api.Spec`.

### Patch takes a closure

```go
u, err := Users.Patch(ctx, id, func(u *User) { u.Name = "Ada L." })
```

Go has no `Partial<T>`, and this is better than one. The service has to read before it writes anyway — that is what validating the whole document means — so the caller may as well be handed the value it read. Field names are checked by the compiler, the write is locked to the version that was read, and a lost race is retried from the fresh document rather than by re-applying a stale delta. The closure may run more than once, so keep it a pure edit.

`PatchFields` is the same operation for when the field names are data rather than code: a form, a script, an admin tool. It reads, deep-sets the dotted paths, and validates the result exactly as `Patch` does.

## The envelope

Every document carries one, and only the store writes it:

| field | |
|---|---|
| `id` | UUIDv7 — time-ordered, so inserts stay at the right edge of the primary key's B-tree instead of scattering across it |
| `version` | the optimistic lock. Starts at 1, increments on every patch, unchanged by a delete |
| `meta.created_at` / `created_by` | epoch milliseconds and the actor from `db.By` |
| `meta.updated_at` / `updated_by` | stamped on every write |
| `meta.deleted_at` / `deleted_by` | `nil` while the document is active |

Soft delete is the default. Every read carries `meta.deleted_at IS NULL`, which is index-backed because that path is a promoted column with a partial index on it. `db.Deleted()` lifts the filter; `db.Hard()` removes the row for good.

## Filters

The grammar is the subset that compiles to an indexable SQL predicate: `$eq $ne $in $nin $gt $gte $lt $lte $or $and $exists`, plus dot-paths. Two spellings, one meaning:

```go
db.And(db.Eq("plan", "pro"), db.Gt("seats", 10))
db.M{"plan": "pro", "seats": db.M{"$gt": 10}}
```

A comparison against `nil` matches a stored JSON null **and** an absent key — Mongo's rule, and the one soft delete is built on: a document written before the field existed has no such key at all.

Aggregations, `$regex`, `$elemMatch` and text search are out. They need a query planner of their own; `SQL()` is the door, and everything through it is uncached and backend-specific.

## Storage: JSONB plus promoted columns

```sql
CREATE TABLE "users" (
  id   TEXT PRIMARY KEY,
  doc  JSONB NOT NULL,
  version    BIGINT GENERATED ALWAYS AS (((doc #>> '{version}'))::bigint) STORED,
  deleted_at BIGINT GENERATED ALWAYS AS (((doc #>> '{meta,deleted_at}'))::bigint) STORED
  -- one more per Promote entry
)
```

Not pure-generic JSONB, because a JSONB path has no planner statistics and no range index. Not a column per field, because then every added field is a migration. The filter compiler routes a promoted path to its column and everything else to JSONB operators — a query is fast exactly where somebody said it needed to be, and correct everywhere else.

**The table shape is fixed**, so adding a field to the struct changes no DDL at all. The only thing that does is the `Promote` list, applied idempotently with `ADD COLUMN IF NOT EXISTS` at construction.

Updates go through a recursive `howl_jsonb_deep_set`: plain `jsonb_set` does not create missing intermediate objects, so a patch to a path whose parent the stored document lacks would be silently dropped — a lost write, and one that only shows up for documents written before the field existed.

## Evolving without migrations

Adding a Go field is not a schema change, but the documents written before it exists do not carry the key. `Report` is how you find out, and it is exact rather than sampled on any backend that can count JSON keys in one query:

```go
report, _ := Users.Report(ctx)
for _, f := range report.Missing {   // declared, absent from some documents
	fmt.Println(f.Field, f.Docs, string(f.Default))
}
for _, f := range report.Orphans {   // stored, no longer declared
	fmt.Println(f.Field, f.Docs)
}

Users.Backfill(ctx, "plan")          // writes the same default the report showed
Users.DropField(ctx, "nickname")     // refuses the envelope and declared fields
```

Removing a path from `Promote` is additive in the other direction: the column and its index stay, maintained on every write and queried by nothing. `Columns` finds those orphans and `DropColumn` removes them; a still-declared column is refused, because the config is the source of truth.

## In an endpoint

No glue — the document is already the response type:

```go
// server/apis/users/id.dyn.api.go  ->  GET /api/users/{id}
var Get = api.Define(api.Spec[api.None, api.None, users.User]{
	Name: "GetUser",
	Handler: func(r *api.Request[api.None, api.None]) (users.User, error) {
		u, err := users.Users.Get(r.Context(), r.Param("id"))
		if errors.Is(err, db.ErrNotFound) {
			return u, api.NotFound("no such user")
		}
		return u, err
	},
})
```

`db.ErrConflict` is a 409 and `db.ErrInvalid` a 400; the wrapped message is your own validator's and is safe to show.

## Caching

Off by default. `db.Cache{TTL: …}` turns on an in-process LRU, which is correct for one process and wrong the moment there are two — invalidation is per-process, so a second replica keeps serving its own copy. Supply a shared adapter for anything replicated.

Keys are `<prefix>:<collection>:v<version>:get|find:…`. Invalidation moves the version, which makes every key built before it unreachable at once — no pattern scan, no key enumeration. Nothing is deleted; the LRU reclaims the orphans in its own time.

Two things are never cached: anything carrying `db.Session(tx)`, because an uncommitted read must not be published and a write that may roll back must not evict; and a projected document under its by-id key, because half a document must not be served to a later `Get`.

## Testing

`db/memdb` is the same contract over a map: no persistence, no index, every query a scan.

```go
users, _ := memdb.NewService[User](db.Options{Collection: "users"})
```

Both backends run the same suite in `db/conformance` — 24 cases, over 100 assertions. A contract described in prose drifts, because the first implementation defines what the words meant and the second implements what it read. The live Postgres run lives in `db/pg/livetest`, its own module so the driver it needs stays out of the framework's `go.mod`:

```sh
docker run -d --name howl-conf-pg -p 54329:5432 \
  -e POSTGRES_PASSWORD=conf -e POSTGRES_DB=howl_conformance postgres:16-alpine
make test-db-pg
```

## Tooling

`howl check` carries two rules for the store, both for mistakes that fail silently rather than loudly:

| rule | |
|---|---|
| `page-imports-db` | a page importing `db` — the same rule as `core/app`. The page tree has to stay leaves, and `db` links `database/sql` into the wasm build. Load documents in an endpoint or `Config.Data` |
| `collection-value-receiver` | `Defaults` on a value receiver. It still satisfies the interface, so the service calls it — and every default is written to a copy. Nothing fails; the field is empty forever |

`howl_scaffold` takes `kind: "collection"`:

```json
{"kind": "collection", "name": "users", "fields": ["email:string", "seats:int64"]}
```

It writes `server/store/users.go` with the envelope embedded, the pointer receiver correct, and the `Unique`/`Promote` lines commented in place. Unlike a page or an endpoint the location carries no machine meaning — nothing generates from that directory, so move the file wherever it belongs.

## Interoperating with the TypeScript services

The stored shape matches `@hushkey/service-core` key for key: the same envelope names, the same epoch-millisecond timestamps, the same generated-column definitions, the same `sql` cache namespace. A Go service and a Deno service can drive the same table, which is what makes porting one endpoint at a time possible.

## What is deliberately missing

- **Projection returns a partial struct** — it cannot. An omitted field comes back as its zero value, indistinguishable from a stored empty one. Use `Project` to save bandwidth, not to signal absence.
- **Transactions are not wrapped.** Run `BEGIN`/`COMMIT` on a `*sql.Tx` and pass it as `db.Session(tx)`; caching is skipped for anything that carries one.
- **Bulk writes do not validate.** `PatchWhere` writes the same fixed values to every match with no per-row read — that is the point of it. For anything that has to be checked per document, loop over `Patch`.
- **Roles, ownership and permissions.** Same answer as `core/api`: the store has no user model, and a framework that guesses at one is a framework you have to fight.
