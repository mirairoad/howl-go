# howl-go

A spike answering one question: how far can Go + templ go toward an SPA, and
where does that actually break?

One Go module. No Node, no npm, no client framework. The same components render
on the server, into static files, and inside the browser via WebAssembly.

## Layout

```
main.go                    server: mux from the generated table, SSR/fragment switch
tools/fsroutes/            build-time crawler -> client/pages/fsroutes_gen.go
client/router/             Route model, layout composition, ctx  (leaf)
client/dom/                browser API; real for wasm, no-ops elsewhere
client/store/              domain types + state, zero server deps (leaf)
client/ui/                 components shared across pages        (leaf)
client/public/             css, js, generated wasm               (embedded)
client/pages/              the route tree — see below
wasm/main.go               browser entrypoint: GOOS=js GOARCH=wasm
```

### Filesystem routing

```
client/pages/
  app.templ                      document shell — reserved, never a route
  index.templ                    /
  about/index.templ              /about
  blog/index.templ               /blog
  blog/article_id.dyn.templ      /blog/{article_id}
  todos/index.templ              /todos
  dashboard/layout.templ         wraps /dashboard and everything below
  dashboard/index.templ          /dashboard
  dashboard/metrics/index.templ  /dashboard/metrics
  dashboard/settings/index.templ /dashboard/settings
```

`make routes` runs `tools/fsroutes`, which walks the tree and writes
`client/pages/fsroutes_gen.go`. The mux, the layout chain, the static export and
the wasm renderer all read that one generated table. Adding a page is adding a
file.

Every `.templ` is a route except two reserved basenames — `layout.templ` and
`app.templ`. A route's component is whichever zero-argument `templ Name()` the
file declares first, so files sharing a directory just name their components
differently.

**Behaviour is encoded in dot-separated modifiers on the file name:**

| file | route | meaning |
|---|---|---|
| `index.templ` | `/dir` | server-rendered |
| `index.client.templ` | `/dir` | also rendered in the browser by wasm |
| `article_id.dyn.templ` | `/dir/{article_id}` | path parameter |
| `article_id.dyn.client.templ` | `/dir/{article_id}` | both, either order |

An unknown modifier is a hard error (`unknown modifier "cleint"`), because a
silently ignored typo is a route that quietly loses a capability.

Modifiers are **suffixes, not prefixes**, and this is forced: Go ignores every
file whose name starts with `_`, so `_index.templ` generates `_index_templ.go`
and the package then reports *"no Go files"*. Brackets fail for a related
reason — Go rejects `[` and `{` in file names (*invalid input file name*) and
in import paths (*malformed import path: invalid char*). Dots are legal in
both, so dots carry the convention.

One directive comment remains, for the one thing neither the file name nor the
file contents can express:

```go
//howl:route /custom  // override the derived pattern entirely
```

### `func Mount()` — the lifecycle templ does not have

templ has no lifecycle. A component is `Render(ctx, io.Writer)`: it runs once,
writes a string, and is finished. There is no mount, no effect, no re-render.
So "do something on first paint" lives outside the component — in Go:

```go
// in client/pages/dashboard/metrics/index.client.templ, beside Page and Head
func Mount() {
    rows := dom.Root().QueryAll("[data-rows] tr")
    dom.Log("[metrics] mounted — rows in DOM:", len(rows))

    go func() {                          // net/http in wasm IS fetch()
        res, err := http.Get("/api/metrics")
        …
        dom.Log("fetched", len(payload.Rows), "regions")
    }()
}
```

It lives in the page's own `.templ`, next to `Page` and `Head`. That works
because it touches the DOM through `client/dom` rather than `syscall/js`
directly — a page's generated Go is compiled for **both** targets, so importing
`syscall/js` there would break the server build. The platform split lives in
`client/dom` instead, once: `dom_js.go` is the real implementation, `dom_stub.go`
is no-ops for every other GOOS. `Mount` is a plain `func()`, so it needs no
build tag and sits in the same generated table the server uses, where it is
simply never called.

`Mount` runs after the markup is in the DOM, on the cold load and again after
every client-side navigation to that route. Verified in the console, both paths:

```
[metrics] mounted — rows in DOM: 8
[metrics] fetched from Go in the browser — 8 regions, total 315001
```

**You do not need htmx or JS `fetch` for this.** Go's wasm `net/http` transport
is the browser's Fetch API, so `http.Get` works from the page. It must run in a
goroutine: blocking the JS callback deadlocks the Go scheduler, because the
fetch can only resolve once control returns to the event loop.

Three levels, pick by need:

| need | mechanism | language |
|---|---|---|
| DOM behaviour on an element | `data-island` + `register()` | JS, ~10 lines |
| page lifecycle, typed data, HTTP | `func Mount()` in the page's `.templ` | Go |
| server-owned markup on demand | fragment endpoint returning templ | Go |

### `templ Head()`

A page may declare a reserved `Head` component. Its output is merged into the
document `<head>`, so a route owns its title *and* whatever else it needs:

```templ
templ Head() {
	<title>Metrics-and-more</title>
	<meta name="description" content="Per-region throughput, filtered in the browser."/>
	<link rel="canonical" href="https://example.com/dashboard/metrics"/>
}
```

Rules that fall out of making this work properly:

- **Exactly one `<title>` in the document.** The server pulls the `<title>` out
  of the head fragment and the shell emits it in one place. Two title elements
  and the browser silently keeps the first — the page would lose to the shell.
- **A fragment has no `<head>`.** SPA responses ship the page's head in an inert
  `<template data-head>`, and `applyHead()` merges it. Without that, every
  navigation after the first would keep the initial page's title and canonical.
- **Page tags are removed on leaving.** Server-rendered head tags sit between
  `<!--page-head-->` markers and are tagged `data-page-head` once at boot, so
  swapping never touches the shell's own stylesheet and script. Verified:
  navigating metrics → settings drops the `meta`/`link` rather than stacking them.
- **The wasm renderer emits the same wire shape**, so one client code path
  handles both.

`Route.Label` is separate and is nav/tab text only, derived from the file or
directory name. Header navigation is **derived, not declared** —
`router.Nav()` returns the top-level static routes, so `/about` gets a link
while `/dashboard/metrics` (reached through its layout's tabs) and
`/blog/{article_id}` (no single URL) do not.

Path parameters arrive as `router.Param(ctx, "article_id")`.
Layouts compose at build time: a page package never imports its parent, so
there is no cycle with the generated table that imports every page package.
Domain types live in `client/store` for the same reason — if they sat in
`pages`, a page importing them would close the loop.

`client/` is what reaches the browser. `pages` and `store` are ordinary Go
packages compiled into *both* binaries — the server renders them and the wasm
build renders them, from one source. `client/public` is embedded with
`//go:embed client/public` and served under the `/static/` URL prefix, which
stays fixed as a public contract regardless of on-disk layout.

## Running it

```bash
make run      # http://localhost:9000
make slow     # same + LATENCY=240ms per request (simulates Sydney -> us-east-1)
make static   # render routes to ./dist and exit
```

`make` regenerates the route table and the wasm renderer first. Plain `go run .` works, but without
`make wasm` there is no `client/public/views.wasm`, so the client silently falls
back to server-rendered fragments — a round-trip per navigation, with nothing
telling you why. Use `make run`.

**Seeing ~240ms on every request from `go run .`?** `LATENCY` is set in your
environment — `echo $LATENCY`. `make run` does not set it.

After editing a `.templ`: `go tool templ generate`. The CLI is a `tool` directive
in `go.mod`, so nothing is installed globally, and `client/pages/*_templ.go` is
committed generated source — CI needs only the Go toolchain.

Editing `client/public/app.js` needs a rebuild: it is embedded in the binary, so
a running server keeps serving the old copy until you `make`.

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
