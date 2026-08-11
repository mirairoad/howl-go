# howl-go

Go + templ rendering the same components on the server, into static files, and
inside the browser via WebAssembly. One Go module, no Node, no npm, no client
framework.

**Full documentation: run `make run-www` and open http://localhost:9001** — the
docs site is itself built with the framework, from the Markdown in `www/docs/`.

## Layout

```
core/                 the framework
  app/                server runtime: SSR/SPA switch, mux, static export, gzip
  router/             Route model, layout composition, context
  signal/             fine-grained reactivity: signal, computed, effect, watch
  dom/                browser API — real for wasm, no-ops elsewhere
  runtime/app.js      client runtime: router, prefetch, head merge, hydration
  cmd/fsroutes/       directory tree -> generated route table
  cmd/mddocs/         Markdown -> templ pages

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
})
log.Fatal(a.Listen(a.Mux()))
```

## Running

```bash
make            # build core, both apps
make run-www    # the documentation site   -> :9001
make run-toy    # the example app          -> :9000
```

Per app: `make -C examples/toy_app slow` adds `LATENCY=240ms` to every request,
which is roughly Sydney to us-east-1 — the whole point of the wasm renderer.
`make -C www static` writes every route to `www/dist`.

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
| [Navigation](www/docs/06-navigation.md) | prefetch on intent, scroll, progress |
| [Constraints](www/docs/07-constraints.md) | what the Go toolchain refuses |

[DESIGN-LOG.md](DESIGN-LOG.md) records how this was arrived at — the React
detour and why it was deleted, every measurement, and the bugs worth remembering.

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

## Client (`client/public/app.js`, ~15 KB raw / 4.6 KB gzipped)

- **Router** — intercepts same-origin `<a>` clicks, swaps `#outlet.innerHTML`,
  `pushState`. View Transitions when available; any failure degrades to
  `location.href`, i.e. a plain MPA.
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
  header counter.

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
components, and exports `howlRender(path, dataJSON)`. Not a port — it resolves
the path through `router.Lookup`, exactly as the server does:

| | |
|---|---|
| steady-state render | **0.08 ms** |
| bytes per navigation | **0** — server not contacted |
| works for never-visited routes | **yes** (a prefetch cache cannot) |
| `client/public/views.wasm` | 5.6 MB raw, **1.63 MB gzipped** |

This is the thing a JS framework structurally cannot do: one component
definition, server-rendered for SEO and client-rendered for interaction, no
duplication and no JS runtime on the server.

**The cost is the payload.** 1.63 MB gzipped versus ~50 KB for an equivalent
React bundle — roughly 33×. Go's runtime and GC ship whether you use them or
not. Fine behind a login on desktop; not fine for a public page on mobile data.
So it is lazy-loaded only for users heading into a route that needs it.
Untested next step: TinyGo, which usually lands Go/wasm at 200–800 KB; whether
templ's generated code survives its reflection limits is the open question.

`net/http` does not compress by default — uncompressed this ships 5.6 MB, so
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
  linker already drops unused code; the 5.6 MB is runtime and GC.
