# howl-go

Go + templ rendering the same components on the server, into static files, and
inside the browser via WebAssembly. One Go module, no Node, no npm, no client
framework.

**Full documentation: run `make run-www` and open http://localhost:9001** — the
docs site is itself built with the framework, from the Markdown in `www/docs/`.

## Layout

```
core/                 the framework
  app/                server runtime: SSR/SPA switch, mux, static export, status
  mw/                 middleware: logger, recover, compress, cors, csrf, csp, coalesce
  console/            slog handler: tinted columns on a terminal, JSON in a pipe
  router/             Route model, layout composition, context, status
  state/              typed request state on context, via generics
  signal/             fine-grained reactivity: signal, computed, effect, watch
  dom/                browser API — real for wasm, no-ops elsewhere
  runtime/app.js      client runtime: router, prefetch, head merge, hydration
  api/                typed endpoints: Spec[Q,B,R], roles hook, generated client
  cmd/howl/           dev server, `howl check`, `howl mcp`
  cmd/fsapis/         endpoint tree -> route table + typed client
  cmd/fsroutes/       directory tree -> generated route table
  cmd/mddocs/         Markdown -> templ pages

db/                   optional document store — nothing in core/ imports it
  doc.go              the envelope: id (UUIDv7), version, audit + soft delete
  service.go          the contract: validation, locking, soft delete, cache
  pg/                 Postgres backend: JSONB + promoted generated columns
  memdb/              the same contract over a map — the test double
  conformance/        one behavioural suite, run by every backend

examples/toy_app/     kitchen sink: dashboard, blog, local-first todos
www/                  the documentation site
  docs/*.md           the documentation source
```

`core` knows nothing about either app. An application supplies a route table, a
document shell and its own static files:

```go
a := app.New(app.Config{
    Routes:   pages.FsClientRoutes(),   // generated from client/pages/
    Shell:    pages.App,                // its app.templ
    NotFound: pages.NotFound,
    Public:   public,                   // its own css; app.js comes from core
    Data:     data,                     // context for every render
	Bootstrap: func(ctx context.Context, path string) any {
		return state.Get[AppState](ctx)    // runtime state restored before wasm renders
	},
    Use: []mw.Middleware{               // outermost first; plain net/http
        mw.RequestID, mw.Logger(nil), mw.Recover(nil), mw.Compress{}.Handler,
    },
})
log.Fatal(a.Listen(a.Mux()))
```

## Running

```bash
make            # build core, both apps
make run-www    # the documentation site   -> :9001
make run-toy    # the example app          -> :9000
make dev-toy    # the same app, watched: rebuild + restart + reload on save
```

In your own project, replace `go run .` with:

```bash
go run github.com/mirairoad/howl-go/core/cmd/howl dev
```

It regenerates routes, runs templ, rebuilds and restarts on every save — **~540 ms
for a templ edit, ~390 ms for a Go edit** — and reloads the browser. A CSS change
skips the rebuild entirely and swaps the stylesheet in place. There is no HMR:
Go cannot hot-swap a linked binary, and the docs say so rather than implying
otherwise. What it removes is the manual part.

The dev server proxies a stable port in front of the restarting app, so the
address bar never stops answering and a request that arrives mid-rebuild waits
for the new binary instead of failing. **A failed build keeps the last good
binary serving** and puts the compiler error on screen as an overlay — reloading
onto a broken build would replace a working page with a blank one. Your app needs
no dev-mode code: the dev server configures it through `HOWL_ADDR`,
`HOWL_PUBLIC_DIR` and `HOWL_DEV`.

Per app: `make -C examples/toy_app slow` adds `LATENCY=240ms` to every request,
which is roughly Sydney to us-east-1 — the whole point of the wasm renderer.
`make -C www static` writes every route to `www/dist`. The docs site builds no
wasm binary: no route there is `.client`, and 1.71 MB gzipped is the wrong
trade for a public content site whose navigation prefetch already makes
instant. `examples/toy_app` is where the browser renderer runs.

## Use it from another module

howl-go is a normal Go module. Applications import the packages under `core/`;
the example applications are reference code, not framework dependencies.

```bash
go mod init example.com/myapp
go get github.com/mirairoad/howl-go@v0.1.0
go get -tool github.com/a-h/templ/cmd/templ@v0.3.1020
```

```go
import (
    "github.com/mirairoad/howl-go/core/app"
    "github.com/mirairoad/howl-go/core/router"
)
```

Generate the route table and templ Go source before building:

```bash
go run github.com/mirairoad/howl-go/core/cmd/fsroutes@v0.1.0 \
  -module example.com/myapp/client/pages
go tool templ generate
go build .
```

The consuming module needs its own templ tool declaration; tool directives do
not propagate from dependencies. See [`examples/hello`](examples/hello) for a
complete standalone module that imports the tagged framework release.

Each app's `Makefile` runs the same three steps: generate the route table,
`templ generate`, build. `www` adds a Markdown step before them.

## Documentation

| | |
|---|---|
| [Getting started](www/docs/01-getting-started.md) | the smallest app |
| [Routing](www/docs/02-routing.md) | filesystem routes, modifiers, layouts |
| [Rendering](www/docs/03-rendering.md) | SSR, SPA, wasm, static — one component |
| [Lifecycle](www/docs/04-lifecycle.md) | `Mount` / `Unmount`, fetching from Go |
| [Reactivity](www/docs/05-reactivity.md) | signals, computed, effects, watch |
| [Navigation](www/docs/06-navigation.md) | prefetch on intent, transitions, scroll |
| [Constraints](www/docs/07-constraints.md) | what the Go toolchain refuses |
| [HTTP layer](www/docs/08-http.md) | middleware, static, status, errors, state |
| [Dev server](www/docs/09-dev.md) | watch, rebuild, restart, live reload |
| [Document store](www/docs/10-database.md) | `db` — collections as structs, no migrations |

[DESIGN-LOG.md](DESIGN-LOG.md) records how this was arrived at — the React
detour and why it was deleted, every measurement, and the bugs worth remembering.

`howl check` enforces what this file describes rather than describing it —
a page importing `core/app` or `db`, `templ Mount()`, a shell missing `#outlet`,
an endpoint reading the raw query it declared a type for, roles with no
`Authorize` wired, a collection whose `Defaults` is on a value receiver and so
silently mutates a copy. `howl mcp` serves the same checks plus the route and endpoint tables as
MCP tools over stdio, configured by the `.mcp.json` in this repo, so an agent
can ask instead of guessing.

[llms.txt](llms.txt) is the whole framework in one file, written for coding
agents: conventions, API surface, the constraints the Go toolchain imposes, and
an explicit list of things not to invent. The docs site serves it at
[`/llms.txt`](http://localhost:9001/llms.txt). Point a new chat at it before
asking for howl-go code — the file-naming rules in particular cannot be guessed,
because Go rejects the conventions every JS framework uses.

## The core trick

`page()` in `main.go` is the only code that knows about SSR vs SPA:

```go
func page(w http.ResponseWriter, r *http.Request, title string, c templ.Component) {
    if r.Header.Get("X-Partial") == "1" {          // SPA navigation
        w.Header().Set("X-Title", title)
        c.Render(r.Context(), w)                   // fragment only
        return
    }
    ctx := templ.WithChildren(withData(r.Context(), path), c)  // cold request
    pages.App(title).Render(ctx, w)                            // full document
}
```

A `templ.Component` is just `Render(ctx, io.Writer)`. It cannot tell whether it
is being written into a document, a fragment response, a file, or a wasm string
buffer. That is why every mode below coexists with no branching in components:

| mode | caller | output |
|---|---|---|
| cold SSR | `App(...).Render(WithChildren(ctx, route.Component()))` | full HTML doc |
| SPA nav | `page.Render(ctx, w)` | fragment for `#outlet` |
| static gen | `page.Render(ctx, file)` | `.html` on disk |
| fragment API | `TodoItem(t).Render(ctx, w)` | one `<li>` |
| **wasm** | `route.Component().Render(ctx, &sb)` | HTML, in the browser |

## Client (`core/runtime/app.js`, ~23 KB raw / 8.3 KB gzipped)

- **Router** — intercepts same-origin `<a>` clicks, swaps `#outlet.innerHTML`,
  `pushState`. Any failure degrades to `location.href`, i.e. a plain MPA.
- **Transitions are declared, not assumed.** `data-transition-slide-left` on a
  link, or on `<html>` for every navigation; `data-transition-none` opts back
  out. Nearest declaration wins, back/forward plays it reversed, and nothing
  animates unless asked — an unstyled browser cross-fade on every navigation is
  a design decision the framework has no business making. `prefers-reduced-
  motion` overrides all of it: the View Transitions API does not honour that
  query itself, and suppressing the animation in CSS still pays for the
  snapshot, so no transition is started at all. Styling lives in the opt-in
  `/static/transitions.css` (1.6 KB gzipped), tuned through custom properties.
- **Two separate questions, deliberately.** `spaTarget(a)` decides whether the
  router handles a click; `shouldPrefetch(url)` decides whether to warm it.
  Conflating them is a nasty bug: a link you decline to *prefetch* is still a
  link you must *intercept*, and one shared helper returning null skips
  `preventDefault()` — the browser does a full page load and the SPA behaviour
  silently vanishes on exactly the routes you optimised.
- **Prefetch on intent, Turbo Drive style.** Nothing is warmed on load. Hover
  arms a **100 ms timer** and cancels it on `pointerleave`; keyboard focus and
  `pointerdown` fire immediately, since those are already commitment. The delay
  is the point: firing on the first `pointerover` means a mouse crossing a
  5-link header issues 5 server renders nobody asked for. Measured — a fast
  sweep across every nav link produces **0** fetches; lingering 400 ms on one
  produces exactly **1**. Skipped entirely under `navigator.connection.saveData`
  or a 2G `effectiveType`, and per link via `data-no-prefetch`.
- **Scroll restoration.** `history.scrollRestoration = "manual"`; the outgoing
  offset is written to its own history entry before `pushState`, and replayed
  from `popstate`. New navigations start at the top. The restore has to run
  *inside* the swap callback — `startViewTransition` defers it, so scrolling
  outside targets the old DOM and the browser clamps the offset to 0.
- **Progress bar after 500 ms.** A bar that flashes on every fast navigation
  reads as jank; one that never appears makes a slow link feel broken.
- **Islands** — `<div data-island="name" data-props="{…}">` over server-rendered
  markup. Islands *outside* `#outlet` keep state across navigation; islands
  *inside* re-hydrate. That boundary is the SSR/SPA seam, made visible by the
  header counter. The framework ships the **registry**, never the islands: an
  application registers its own with `howl.island(name, setup)` from its own
  script, and a registration arriving after boot hydrates immediately.
- **`window.howl` is the whole public API** — `navigate(url, {replace,
  transition, scroll})`, `prefetch(url)`, `island(name, setup)`, `config`.
  `core/dom` calls straight into it, so Go running in wasm navigates through
  the same path a click takes: `dom.Navigate("/", dom.Replace())`.
- **Nothing is hardcoded in the client.** The shell publishes
  `router.ClientConfig(ctx)` as JSON: which patterns need the wasm binary
  (`router.NeedsWasm`, derived from the generated table) and which endpoint —
  if any — supplies its data. An app with no client data makes no fetch.

## The HTTP layer

Middleware is `func(http.Handler) http.Handler`. No framework handler type, no
context wrapper: `r.Cookie`, `r.URL.Query` and `json.NewEncoder` already exist,
and a wrapper around them is a tax the TypeScript side pays because its standard
library does not.

```go
Use: []mw.Middleware{mw.RequestID, mw.Logger(nil), mw.Recover(nil), mw.Compress{}.Handler}
```

`mw` ships `RequestID`, `Logger` (slog, level follows status), `Recover`,
`Compress`, `CORS`, `CSRF`, `CSP` and `Coalesce`. Everything composes with
anything written for chi or the standard library, in either direction.

Three details that are the whole reason these exist rather than being left to
the application:

- **Pages render into a buffer before anything is sent.** That buys a status a
  component can still change — `router.NotFound(ctx)` inside a `templ` block,
  which is what makes `/blog/nope` answer **404** instead of a soft 200 — plus an
  error page rather than a truncated document when a render fails halfway, and a
  `Content-Length`.
- **Static files are compressed once and kept**, with an ETag per representation
  and `304` on revalidation. Gzipping a 6.1 MB wasm binary per request burns a
  core per download; gzipping it once costs 1.71 MB of memory and nothing per
  request.
- **`Coalesce` shares one render across identical concurrent requests** and
  refuses to share four things that would be bugs: non-GET, requests with
  cookies, responses that set one, and anything past 8 MiB.

### Logging

```go
console.Setup(console.Options{})
```

```
09:45:31.924 INFO  listening   url=http://localhost:4399 routes=5
09:45:33.988 INFO  http        POST /api/logs 202 0B 6.76ms id=b13b1e4b ip=10.1.4.9 ua=otel-collector/0.96
09:45:34.220 WARN  http        GET /nope 404 1.6kB 45µs id=24ab09a8
```

Level and status are tinted; `method`/`path`/`status`/`bytes`/`took` render
positionally so a request line reads as one. **Terminal → colour, pipe → JSON**,
decided by whether stdout is a character device, so the same binary is pleasant
to run and parseable to ship. `NO_COLOR` wins outright.

howl (TS) achieves this by patching `globalThis.console`. Go needs no such
thing: `log/slog` is already the seam, so one handler covers your code, the
framework's and your dependencies' — none of which import `core/console`.

`mw.LogWith(mw.LogOptions{Callers: true, Skip: mw.SkipNoise})` adds `ip` and
`ua` **only for requests that did not come from a page on this host**. Your own
SPA carries a same-origin `Referer`/`Sec-Fetch-Site` and stays quiet; an
exporter, a scraper or curl gets identified. Static assets and health checks are
dropped.

## Latency, and the three answers to it

Fragment-SSR pays **one RTT per navigation before any pixel moves** — fine next
to the datacentre, unusable from Sydney. Measured with `make slow` (240ms):

| approach | cost per navigation | what crosses the wire |
|---|---|---|
| fragment SSR, cold | 240 ms+ | HTML |
| fragment SSR, prefetched on hover | **0.3–0.4 ms** | HTML, earlier |
| wasm renderer | **0.6–9 ms** | **nothing** |

Prefetching is most of the win and costs nothing: latency only hurts if you pay
it *at click time*. But it is still server-rendered — a route the user never
hovered, or data that changed, costs a round-trip again.

### Rendering in the browser

```bash
GOOS=js GOARCH=wasm go build -o client/public/views.wasm ./wasm
```

`wasm/main.go` imports the **same generated route table** and the same templ
components, and exports `howlRender(path, renderPayloadJSON)`. The payload keeps
per-request bootstrap state separate from route data. Not a port — it resolves
the path through `router.Lookup`, exactly as the server does:

| | |
|---|---|
| steady-state render | **0.08 ms** |
| bytes per navigation | **0** — server not contacted |
| works for never-visited routes | **yes** (a prefetch cache cannot) |
| `client/public/views.wasm` | 5.99 MB raw, **1.66 MB gzipped** |

This is the thing a JS framework structurally cannot do: one component
definition, server-rendered for SEO and client-rendered for interaction, no
duplication and no JS runtime on the server.

**The cost is the payload.** 1.66 MB gzipped versus ~50 KB for an equivalent
React bundle — roughly 33×. It was 2.90 MB until the client stopped importing
net/http: under GOOS=js that package is also fetch underneath, but it links
crypto/tls and crypto/x509 to re-implement what the browser already does, and
measured on an otherwise empty wasm binary that is 0.51 MB gzipped against
2.56 MB. `core/dom` has fetch instead — see `dom.GetJSON`. Go's runtime and GC ship whether you use them or
not. Fine behind a login on desktop; not fine for a public page on mobile data.
So it is lazy-loaded only for users heading into a route that needs it.
Untested next step: TinyGo, which usually lands Go/wasm at 200–800 KB; whether
templ's generated code survives its reflection limits is the open question.

`net/http` does not compress by default — uncompressed this ships 6.1 MB, so
`gzipStatic` in `main.go` is not optional at this size.

### The store runs in the browser too (`/todos`)

`store.Store` has no `net/http`, no database, no filesystem, so it compiles for
the server *and* for wasm. The browser runs the identical mutation code:

```go
func (s *Store) Apply(op Op) {   // called by the HTTP handler AND by the browser
    switch op.Kind {
    case "add": s.Add(op.Text)
    case "del": s.Del(op.ID)
    }
}
```

Server SSRs the list → wasm hydrates from `GET /api/todos` → every add and
delete mutates the local store, re-renders via `pages.TodoList`, and paints. The
op is queued and POSTed to `/api/todos/sync` afterwards, off the critical path.
With the server at 240ms, an add paints in **1.0–4.2 ms**.

This is what makes local-first cheap here. Normally it costs a second
implementation of every rule in TypeScript, hand-synced with the backend, and
the bug is always in the divergence. There is one implementation.

**Deliberately unfinished:** ids are assigned by whichever store applies the op
first, so a client id can collide until the server snapshot is adopted (only
when the pending queue is empty, otherwise in-flight edits get clobbered). Two
tabs diverge. Nothing survives reload. Real answers — per-client id ranges or
ULIDs, an op log, IndexedDB — are all ordinary Go now that the store is shared.

## Where this genuinely breaks down

Navigation is solved. The remaining limits are structural:

1. **Swap destroys client state.** `innerHTML` resets focus, caret, scroll, open
   dropdowns and typed-but-unsubmitted input. When stale data must refresh under
   a user mid-interaction you must choose between showing stale data and
   destroying their work. `app.js` defers the swap while an input is focused —
   a patch over a structural gap, not a fix. A VDOM never faces the choice.
2. **Unbounded URL space.** You can prefetch three tabs. You cannot prefetch
   `?filter=…&sort=…&page=…`. That state has to live on the client anyway.
3. **Complex interdependent state.** Vanilla islands are fine for a filter. A
   wizard with optimistic updates, undo and cross-component sync becomes
   spaghetti without a component model.
4. **Payload.** See the wasm numbers above.

## Notes

- templ has **no configuration file** — only CLI flags (`-path`, `-f`, `-watch`).
  Folder organisation is just Go packages; cross-package components compose
  normally because `templ.Component` is an interface. templ does not validate
  package names, so a folder/package mismatch surfaces as a Go compile error.
- `client/store` imports `client/pages` (for `pages.Todo`). Keep that direction:
  if a page ever needs the store, the shared types must move to their own
  package or you get a cycle.
- Splitting `client/pages` into subpackages will not shrink the wasm — the Go
  linker already drops unused code; the 6.1 MB is runtime and GC.
