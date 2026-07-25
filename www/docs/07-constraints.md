# Toolchain constraints

Several conventions from JavaScript frameworks cannot be ported to Go. These were verified, not assumed, and they explain why howl-go's naming looks the way it does.

## Underscore prefixes are invisible

The Go tool ignores every file and directory whose name begins with `_` or `.`.

```
_layout.templ  ->  _layout_templ.go  ->  package reports "no Go files"
```

So `_layout.templ`, `_app.templ` and `_index.templ` are impossible. howl-go uses reserved basenames instead: `layout.templ`, `app.templ`, `index.templ`.

## Brackets are illegal in both names and paths

| attempted | result |
|---|---|
| `[article_id].templ` | `invalid input file name` |
| `[article_id]/` directory | `malformed import path: invalid char '['` |
| `{article_id}/` directory | `malformed import path: invalid char '{'` |
| `article_id.dyn.templ` | **OK** |

Dots are legal in file names *and* import paths, which is why modifiers are dot-separated suffixes.

## Package clauses cannot collide

Two routes in one directory share a Go package, so per-page `const Title` does not compile. That is why metadata moved first to directive comments and then into file names.

Directory names may differ from package names, so `getting-started/` can hold `package getting_started`.

## `syscall/js` only exists on `GOOS=js`

A `.templ` file's generated Go compiles for both targets, so a page cannot import `syscall/js`. The split lives in `core/dom` instead — one API, `dom_js.go` real and `dom_stub.go` no-ops. Verify isolation with `go list -deps`, not `strings`:

```
server: syscall/js NOT in deps
wasm:   syscall/js in deps
```

## `GET /` is a catch-all

In `net/http`'s `ServeMux`, `GET /` matches everything. The root route must be registered as `GET /{$}` or it collides with the 404 handler.

## Generated code must be committed

`*_templ.go` and `fsroutes_gen.go` are generated Go source and belong in version control, so consumers and CI need only the Go toolchain. The `templ` CLI is a `tool` directive in `go.mod` — nothing is installed globally.
