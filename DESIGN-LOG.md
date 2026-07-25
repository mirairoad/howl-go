# howl-go — design log

A running record of what was tried, what was measured, and what the Go
toolchain refused to allow. Written so the dead ends do not get re-explored.

The question throughout: **how far can Go + templ go toward an SPA, and where
does it actually break?**

---

## 1. SSR and SPA from one component tree

`templ.Component` is just `Render(ctx, io.Writer)`. A component cannot tell
whether the writer is an HTTP response, a file, or a `strings.Builder` inside
WebAssembly. That single fact is what the whole spike rests on.

One switch, in `main.go`, is the only code that knows the difference:

```go
if r.Header.Get("X-Partial") == "1" {
    c.Render(ctx, w)                    // fragment for the SPA
    return
}
pages.App(title, head).Render(templ.WithChildren(ctx, c), w)   // full document
```

**Measured (cold load, 240ms simulated RTT):** full document vs a 1175 B
fragment in 5 ms. Islands outside `#outlet` keep state across navigation;
islands inside re-hydrate. That boundary *is* the SSR/SPA seam.

---

## 2. The React detour, and why it was deleted

Built a React SPA at `/app`, bundled with **esbuild's Go library** plus a
resolver plugin pulling React from esm.sh — no Node, no npm, no `node_modules`.
It worked: 151 KB bundle in 613 ms, client nav in 1.6–3.1 ms.

Then the same dashboard was built in templ with prefetching, and won on every
axis:

| | templ + precache | React SPA |
|---|---|---|
| route change | **0.3 ms** | 1.6–3.1 ms |
| framework shipped | **0 KB** | 151 KB |
| SEO on every route | **yes** | shell only |
| static export | **yes** | no |

An `innerHTML` swap of a small fragment beats a VDOM reconcile. React was
deleted along with esbuild.

**Conclusion:** latency is a prefetching problem, not a framework problem.
React earns its place when the *client state graph* outgrows what you want to
hand-write — not because of navigation speed.

---

## 3. Prefetching is not precompiling

Prefetching only moves *when* you pay. The server still renders every route's
HTML, every time; a route nobody hovered still costs a round-trip.

So the renderer itself was shipped: `GOOS=js GOARCH=wasm` compiles the **same**
`pages` package. `howlRender(path, data)` resolves the path through the same
`router.Lookup` the server uses and renders locally.

| | |
|---|---|
| steady-state render | **0.08 ms** |
| bytes per navigation | **0** |
| works for never-visited routes | **yes** (a prefetch cache cannot) |
| `views.wasm` | 5.6 MB raw, **1.63 MB gzipped** |

**The cost is the payload** — ~33× a comparable React bundle. Go's runtime and
GC ship whether used or not. Fine behind a login on desktop; not for a public
page on mobile data. Lazy-loaded only for routes that need it.

Untested: TinyGo, which usually lands Go/wasm at 200–800 KB. Whether templ's
generated code survives its reflection limits is the open question.

Also: `net/http` does not compress by default. Uncompressed this ships 5.6 MB,
so `gzipStatic` is not optional at this size.

---

## 4. The store runs in the browser too

`store.Store` has no `net/http`, no database, no filesystem — so it compiles for
both targets. The browser runs the identical mutation code:

```go
func (s *Store) Apply(op Op) {   // called by the HTTP handler AND by the browser
    switch op.Kind {
    case "add": s.Add(op.Text)
    case "del": s.Del(op.ID)
    }
}
```

Add paints in **1.0–4.2 ms** with the server 240 ms away; the op is POSTed to
`/api/todos/sync` afterwards, off the critical path.

This is what makes local-first cheap here. Normally it costs a second
implementation of every rule in TypeScript, hand-synced with the backend — and
the bug is always in the divergence. There is one implementation.

---

## 5. Filesystem routing

`tools/fsroutes` walks `client/pages` and generates `fsroutes_gen.go`. The mux,
layout chain, static export and wasm renderer all read that one table.

### What the Go toolchain refused

These are hard constraints, verified rather than assumed:

| attempted | result |
|---|---|
| `_layout.templ`, `_index.templ` | templ emits `_layout_templ.go`; Go reports **"no Go files in package"** — it ignores every file/dir starting with `_` |
| `[article_id].templ` | **"invalid input file name"** |
| `[article_id]/` directory | **"malformed import path: invalid char '['"** |
| `{article_id}/` directory | same, invalid char `{` |
| `article_id.dyn.templ` | **OK** — dots are legal in file names and import paths |
| `at-article_id/` | OK — hyphens legal too |

So howl's underscore and bracket conventions **cannot be ported to Go**. Dots
carry the convention instead.

### The convention that survived

```
app.templ                    document shell — reserved, never a route
layout.templ                 wraps its directory and below
index.templ                  /dir
index.client.templ           also rendered in the browser by wasm
article_id.dyn.templ         /dir/{article_id}
article_id.dyn.client.templ  both, either order
```

Modifiers are dot-separated and parse generically, so they compose. An unknown
modifier is a hard error — a silently ignored typo is a route that quietly
loses a capability.

### Metadata: consts → directives → file names

1. Started with `const Title` / `const Client` per page. Broke as soon as two
   routes shared a directory: same Go package, duplicate consts, no compile.
2. Moved to `//howl:` directive comments — attached to the file, not a symbol.
3. Then behaviour moved into the file name (`.client`, `.dyn`), because that is
   what the user actually reaches for. Only `//howl:route` remains, for the
   pattern override the filesystem cannot express.

`Nav` was deleted entirely and derived: `router.Nav()` returns top-level static
routes. `/about` gets a header link; `/dashboard/metrics` (reached via its
layout's tabs) and `/blog/{article_id}` (no single URL) do not.

### Cycle avoidance shaped the packages

The generated table lives in `pages` and imports every page package, so nothing
a page imports may lead back to `pages`. Hence the leaves:

```
client/router/   Route model, layout composition, ctx
client/store/    domain types + state
client/ui/       shared components
client/dom/      browser API, real for wasm and no-ops elsewhere
```

Layouts compose at build time, so `dashboard/metrics` never imports `dashboard`.

---

## 6. Prefetching, Turbo Drive style

The behaviour that mattered was the **100 ms hover delay with cancellation**.
Firing on the first `pointerover` means a mouse crossing a 5-link header issues
5 server renders nobody asked for.

```
fast sweep across every nav link  → 0 fetches
lingering 400 ms on one link      → exactly 1
```

Plus: immediate fetch on `pointerdown`/`focusin` (already commitment), opt-out
via `data-no-prefetch`, skipped under `saveData` or 2G, scroll restoration, and
a progress bar after 500 ms.

---

## 7. `templ Head()`

A page declares a reserved `Head` component; its output is merged into
`<head>`. Four things this forced:

- **Exactly one `<title>` in the document.** Two title elements and the browser
  keeps the *first* — the page would silently lose to the shell fallback. The
  server splits the title out of the head fragment and the shell emits it once.
- **A fragment has no `<head>`.** SPA responses ship it in an inert
  `<template data-head>`; without that, every navigation after the first keeps
  the initial page's title and canonical.
- **Page tags are removed on leaving, not stacked.** Server head tags sit
  between `<!--page-head-->` markers and are tagged at boot.
- **The wasm renderer emits the same wire shape**, so one client path handles
  both.

---

## 8. Lifecycle: `func Mount()`

templ has no lifecycle and cannot have one — a component renders once to a
writer and is finished. So "on first paint" is a plain Go func in the page file.

First attempt put it in `mount_js.go`, because `syscall/js` only exists on
`GOOS=js` and a `.templ`'s generated Go compiles for **both** targets. That
worked but split the page across two files.

Better: move the platform split down instead of pushing the code out.
`client/dom` has one API with two implementations (`dom_js.go` real,
`dom_stub.go` no-ops). The page then writes `dom.Root()` and `dom.Log()`, needs
no build tag, and `Mount` is a plain `func()` — so it sits in the *same*
generated table the server uses, where it is simply never called. The separate
js-only table was deleted.

`net/http` in wasm **is** the browser's `fetch()`. No htmx, no JS glue:

```
[metrics] mounted — rows in DOM: 8
[metrics] fetched from Go in the browser — 8 regions, total 315001
```

It must run in a goroutine — blocking the JS callback deadlocks the Go
scheduler, because the fetch can only resolve once control returns to the event
loop.

**`templ Mount()` is not possible**, and that is not a limitation: a `templ`
block compiles to a function that writes HTML. It has no way to express "run
this, return nothing". Hence the convention: **`templ` for anything that
produces markup, `func` for anything that does something.**

---

## 9. Unmount, the client store, and the end of islands

`Mount` alone is half a lifecycle. A page that subscribes to something on mount
must release it on the way out, or the callback fires against a DOM that was
thrown away — and every visit adds another subscription.

```go
var cancelSub func()

func Mount()   { cancelSub = store.Subscribe(repaint); … }
func Unmount() { cancelSub(); cancelSub = nil }
```

**Measured:** three away-and-back navigations, `subscribers: 1` each time.
Without `Unmount` it climbs 1, 2, 3.

### Reaching the store from a handler

On the server a `Store` is per-process and read through the request context —
two requests must never see each other's state. In the browser there is one
user and one tab, so `store.Client()` is a package-level instance any `Mount`,
`Unmount` or click handler can reach without threading it through a component
tree. Mutations call `store.Notify()`, so a handler never has to know how to
repaint; one subscriber does.

```go
root.Query("[data-add]").On("click", func() {
    mutate(store.Op{Kind: "add", Text: "added from a Go click handler"})
})
```

`mutate` applies locally for an instant repaint and POSTs the op afterwards, so
the user never waits on the round-trip.

### The JS island registry is gone from this page

`/todos` used to be driven by `register("todos", …)` in `app.js`, with a
*second* store instance living inside `wasm/main.go`. Two systems wrote to
`#todo-list` and raced; the visible symptom was a click that appeared to do
nothing. Deleting the island and the duplicate store fixed it: the page now
hydrates, renders (`ui.TodoList`, the same component the server uses), mutates
and syncs entirely in Go.

**Measured:** hydrate 2 items → Go click handler 3 → Go delete 2, subscribers
constant at 1.

Islands are not obsolete in principle — you still need a DOM anchor, and three
remain for genuinely trivial behaviour (`counter`, `table-tools`, `toggle`).
What is obsolete is writing *page state* in JS once the store is in wasm.

### `*.component.templ`

Colocated markup that is never a route, so a component can live beside the page
that uses it without publishing a URL. `counter.component.templ` is the
example. Per-component *lifecycle* is not implemented — components are wired
by their page's `Mount` querying `data-` attributes. Automatic per-component
mount would need a registry keyed by a DOM marker, i.e. islands, but with the
registry generated in Go instead of hand-written in JS.

### `NeedsWasm`

The hardcoded `WASM_PREFIX = "/dashboard"` in `app.js` is gone. The shell
publishes `router.NeedsWasm(routes)` as JSON, derived from the same generated
table, so adding `.client` or a `Mount` to any route is enough — nothing has to
be told about it twice.

---

## 10. Bugs worth remembering

- `(window.requestIdleCallback ?? setTimeout)(fn, 1)` throws — the second arg is
  `IdleRequestOptions`, not a delay, and it must stay bound to `window`. It
  threw at module top level and silently disabled the entire router.
- One helper answering two questions: `prefetchable()` decided both "warm this?"
  and "intercept this click?". When wasm claimed a prefix it returned null for
  the first reason, the click handler read it as the second, skipped
  `preventDefault()`, and **the SPA silently became full page loads on exactly
  the routes that had been optimised**. Now `spaTarget()` and `shouldPrefetch()`.
- `startViewTransition` defers its callback, so a `window.scrollTo` outside it
  targets the old DOM and the browser clamps the offset to 0. Scroll restore has
  to happen *inside* the swap.
- An aborted view transition never invokes its callback, leaving stale DOM on
  screen. Always keep a fallback swap path, guarded against double-running.
- `GET /` is ServeMux's catch-all; the root route needs `GET /{$}` or it
  collides with the 404 handler.
- A `switch` with a `default:` arm as a router made every typo render the
  overview with **200**. A table lookup cannot do that.
- `//go:embed` bakes `app.js` into the binary — editing it does nothing until
  rebuild.
- Verifying build isolation with `strings` gave 93 false hits. `go list -deps`
  is the authoritative check.

---

## 11. Open questions

- **TinyGo** — would it bring 1.63 MB gzipped down to the 200–800 KB range, and
  does templ's generated code survive its reflection limits?
- **Per-route wasm tables.** `.client` currently gates *rendering*, not
  *bundling*: every page's components are linked into `views.wasm` because the
  one table references them all. Emitting a wasm-only table containing just the
  `.client` routes would let the linker drop the rest.
- **Per-route data.** The wasm renderer receives one blob (`/api/metrics`) and
  unmarshals it as one type, so a client route needing different data has no
  mechanism.
- **Soft 404s.** `/blog/nope` renders a 404 body with HTTP 200.
- **`dom.On` never releases its `js.Func`** — one leaked closure per listener
  per mount.
