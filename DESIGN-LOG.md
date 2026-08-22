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
| `views.wasm` | 6.1 MB raw, **1.71 MB gzipped** |

**The cost is the payload** — 18× React's client runtime and 45× Vue's, both
measured, before either has shipped a line of application code: React 19.2
production is 99.2 kB gzipped (`react` 4.4 + `react-dom-client` 94.7), Vue 3.5's
runtime is 40.0 kB. Go's runtime and GC ship whether used or not. Fine behind a
login on desktop; not for a public page on mobile data. Lazy-loaded only for
routes that need it.

Figures for `views.wasm` are `examples/toy_app` compressed at
`gzip.BestCompression` — the level `core/app/static.go` actually serves at, so
it is the number a browser downloads rather than a number about compression.
Worth re-measuring when the example grows: it read 5.6 MB / 1.63 MB when this
was first written, and neither figure was updated by the changes that moved it.

Untested: TinyGo, which usually lands Go/wasm at 200–800 KB. Whether templ's
generated code survives its reflection limits is the open question.

Also: `net/http` does not compress by default. Uncompressed this ships 6.1 MB,
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

## 10. Reactivity: signals, computed, effects, watch

`Subscribe` was coarse — one callback, re-run on any mutation. Replaced with
fine-grained dependency tracking in `client/signal`:

```go
Todos     = signal.WithEq([]Todo(nil), sameTodos)          // signal
TodoCount = signal.DeriveEq(func() int { … })              // computed
stopEffect = signal.Effect(repaint)                        // auto-tracked
stopWatch  = signal.Watch(store.TodoCount.Get, func(now, before int) { … })
```

### Why there is no dependency array

React needs `useEffect(fn, [a, b, c])` because **JavaScript cannot observe
reads** — React has no way to know what the function looked at, so you declare
it, and a wrong declaration is the classic stale-closure bug.

Here reads *are* observable: while an effect runs it is installed as the current
computation, and every `Signal.Get()` registers the edge both ways. So the
dependency list cannot be wrong, because there isn't one. `WatchAny(cb, srcs…)`
exists for when you want sources named explicitly, but `Effect` is the
idiomatic form.

Re-running an effect **detaches all old dependencies first**, so a conditional
branch that stops reading a signal stops being woken by it — without that, a
branch leaks a permanent subscription.

`Watch` runs its callback inside `Untrack`, or signals read by the callback
would silently become dependencies of the watcher.

### Equality is what makes it fine-grained

- `Of[T comparable]` — skips notification when the value is unchanged.
- `WithEq` — for slices/maps, where `==` does not compile. `Todos` uses it, so
  re-hydrating identical data wakes nothing.
- `DeriveEq` — a recompute that yields the same value stops there, so a
  mutation that leaves a count alone does not repaint things that only read it.

**Measured:** hydrate → `count changed 0 -> 2`; Go click → `2 -> 3`; Go delete
→ `3 -> 2`. Three away-and-back navigations then one add produced **exactly
one** log line — leaked effects would have produced four. Those same three
remounts produced **zero** change logs, because re-hydrating identical data hits
the equality guard.

### Server safety

Signals are package-level, which would be a data race if server request
handlers wrote them. They do not: `(*Store).publish` mirrors into the signals
only when the receiver is the browser's instance. The server's `Store` is a
different pointer, so concurrent requests never touch shared globals.

---

## 11. Bugs worth remembering

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
- **A header is bytes, and `fetch()` decodes response headers as ISO-8859-1.**
  The SPA title travelled in `X-Title`, so `Routing — howl-go` reached the
  client as `Routing â€" howl-go`. Cold loads were fine, which made it read as
  a formatting quirk rather than an encoding bug. The title now rides in the
  fragment body, decoded as UTF-8 per `Content-Type`; `X-Title` survives
  percent-encoded for the prefetch cache. The wasm renderer had emitted
  `<title>` inside `<template data-head>` all along — the comment in `app.js`
  claimed both paths shared a wire shape that they did not.
- **Escaping twice is escaping wrong.** `HeadParts` pulled the title out of
  *rendered* HTML and returned it still escaped; the shell then rendered it
  through templ, which escaped it again, so `A & B` displayed as `A &amp;amp;
  B`. Unescape once at extraction and let each consumer escape for its own
  context — templ, `html.EscapeString`, percent-encoding.
- Verifying build isolation with `strings` gave 93 false hits. `go list -deps`
  is the authoritative check.

---

## 12. Motion is not the framework's decision

The router called `startViewTransition` on every navigation, purely because the
API existed. Nobody had chosen that animation: with no `::view-transition` rules
anywhere in the repo, what shipped was the browser's default cross-fade, applied
to every app built on the framework, with no way to turn it off short of not
using the router.

Two things were wrong with it beyond taste. `prefers-reduced-motion` was never
consulted — the API does not honour that query for you, so a user who had asked
their OS for less motion got the fade anyway. And the transition animated the
*root* snapshot, meaning the header and sidebar cross-faded along with the page
content that had actually changed.

Motion is now declared where the rest of the page's presentation is declared:

```html
<a href="/x" data-transition-slide-left>
<html data-transition-fade-up>
<a href="/y" data-transition-none>
```

Nearest declaration wins, so a document-wide default stays overridable per link,
and `-none` exists precisely to opt out of an inherited one. Back and forward
replay the transition with its direction flipped, because a history stack whose
animation only moves one way feels broken.

The runtime's whole job is to resolve a *name*. It reaches CSS twice — as a
view transition `type`, and as `data-howl-transition` on `<html>` for browsers
that shipped view transitions before types — and the styling lives in an opt-in
stylesheet that names `#outlet` so the outlet animates alone and persistent
chrome stays put. Reduced motion is checked before any of it and starts no
transition at all: neutralising the animation in CSS would still pay for the
snapshot.

The default is now **off**. A framework that animates unless told not to has
made a design choice on behalf of every application built on it.

---

## 13. The HTTP layer

Everything above is rendering. This is the part between the socket and a
component, and the question was how much of howl's TypeScript surface to port.

### Middleware is `func(http.Handler) http.Handler`, and nothing else

howl (TS) has `ctx.cookies`, `ctx.query`, `ctx.json`, `ctx.state`, `ctx.headers`
— a wrapper object, because Fresh's context lacked them. Go's standard library
already has all five. Porting the wrapper would mean a second, worse API for
`r.Cookie` and `json.NewEncoder`, and would cut the framework off from every
middleware ever written for `net/http`.

So: `type Middleware func(http.Handler) http.Handler`, applied outermost-first,
wrapping the whole mux — pages, static files and application handlers alike.
`core/mw` ships `RequestID`, `Logger`, `Recover`, `Compress`, `CORS`, `CSRF`,
`CSP` and `Coalesce`, and none of them import anything of ours.

The one thing worth porting from `ctx.state` was the *typing*, and generics do
it in twenty lines — the key is the type itself:

```go
ctx = state.With(ctx, User{ID: "u_1"})
u := state.Get[User](ctx)
```

### Buffering the render, and the end of the soft 404

`/blog/nope` used to render a 404 body with HTTP 200 (§11). Fixing it requires a
component to change the status *while it renders*, which is impossible while the
response streams — the status line has already gone.

So the page renders into a pooled buffer, the shell into a second one, and only
then is anything written. Three things fall out of that:

| | |
|---|---|
| `router.NotFound(ctx)` inside a `templ` block | the response carries 404 |
| a component that fails halfway | error page, not a truncated document |
| the whole document in hand | `Content-Length` |

`{{ router.NotFound(ctx) }}` is a plain Go statement in the middle of markup —
the same `{{ }}` templ already uses for assignments. It lives in `router`, not
`app`, for the usual cycle reason: the generated table sits inside the page
tree, so anything a page imports must stay a leaf. A page importing the server
runtime to set a status would drag `net/http` into the wasm build.

### Static: compress once, not per request

`GzipStatic` wrapped `http.FileServer` and gzipped **on every request**. At
6.1 MB of wasm that is a core per download — and with no ETag, nothing ever
answered 304. Replaced by a handler that reads each file once, keeps the raw and
gzipped bytes with a content ETag, and hands them to `http.ServeContent`, which
does conditional requests and ranges for free.

The gzipped copy carries its own ETag (`"abc…-gz"`). Two representations under
one validator is how a cache ends up serving compressed bytes to a client that
asked for plain.

### The pooled gzip writer that compressed nothing

`sync.Pool{New: func() any { return &gzip.Writer{} }}` looks obviously right and
is silently wrong: a zero-value `gzip.Writer` has level 0 — `NoCompression` —
and `Reset` keeps whatever level the writer was built with. Every response went
out in a valid gzip frame, decoded correctly by the browser, **larger than the
input**, with `Content-Encoding: gzip` on it.

Measured on the docs site: a 5007 B page became 5035 B "compressed". After the
fix, 1955 B. The test asserts the *ratio* now, not just that it round-trips — a
test that only decodes the body passes happily either way.

### Coalescing, and the four things it must refuse

Single-flighting identical concurrent requests is easy. The interesting part is
what must **not** be shared:

- non-GET;
- requests carrying `Cookie` or `Authorization` — that response is one user's;
- responses that set a cookie: replaying one `Set-Cookie` to every waiter hands
  them all the same session, or the same CSRF token, which quietly disables the
  protection for all of them;
- a panicking handler's half-written buffer — waiters re-run instead.

The key includes `X-Partial`, because a fragment and a document share a URL.

### `GET /` cannot be the catch-all once anything is mounted

`Mount("/admin", h)` registers a method-less pattern, and Go 1.22's ServeMux
**panics at registration** when a method-specific catch-all (`GET /`) meets a
method-less pattern that is more specific in path. The 404 fallback became `/`,
which also means a POST to a nonexistent path gets the application's 404 page
instead of a bare 405.

### The islands the framework had no business shipping

`app.js` contained `counter`, `table-tools` and `toggle` — three toy-app demos,
embedded in the framework and served to every application. The registry stays in
core; the islands moved to the app's own `static/islands.js` and register through
`howl.island(name, setup)`. A late registration re-scans the DOM, so script order
does not matter.

That turned `window.howl` into the deliberate public surface — `navigate`,
`prefetch`, `island`, `config` — which `core/dom` calls into, so `dom.Navigate`
in Go takes exactly the path a link click takes.

### The client config, and the fetch that was never going to succeed

`app.js` fetched `/api/metrics` before enabling the wasm renderer. That path is
the *toy app's*. On the docs site the fetch 404'd, `r.json()` threw, and the
renderer was disabled — the failure mode being "wasm silently never loads",
which is indistinguishable from never having built it.

The shell publishes `router.ClientConfig(ctx)` instead:

```json
{ "wasm": ["/dashboard", "/todos"], "data": "/api/metrics" }
```

No `data` means no fetch; a failed fetch warns and the renderer starts anyway.

### Warming wasm on intent misses everyone who does not hover

`loadWasm()` fired on `pointerover` over a link, or on a cold load of a wasm
route. A keyboard user, a touch tap and `howl.navigate()` all reach a client
route without any `pointerover`, so the binary never downloaded and every
navigation stayed server-rendered — the feature quietly absent rather than
broken. `navigate()` now starts the download when it lands on a wasm route
without one.

Measured after the fix: the first `navigate("/dashboard")` is server-rendered;
the next reports `wasm render → /dashboard/metrics · 0 bytes · server not
contacted`.

### The colourful logger, without a console to patch

howl (TS) prints `[09:25:03.412] [1234]` in a per-method colour by replacing
`globalThis.console`. The Go equivalent is not a console patch — it is a
`log/slog` handler, because slog is *already* the seam every library logs
through. Install one and it covers application code, framework code and
dependency code at once, none of which import `core/console`.

The decision worth recording is when to colour: **whether stdout is a character
device**, i.e. whether a human is reading it right now.

| stdout | output |
|---|---|
| terminal | tinted, aligned columns |
| pipe, file, systemd, Docker | JSON, one object per line |

TS's version prints "plain" in production, which is neither pretty nor
parseable. Splitting on the TTY gives both, with nothing to configure.
`NO_COLOR` wins outright; `FORCE_COLOR` covers the CI viewer that renders
escapes without being a terminal. Detection is `os.File.Stat` and
`os.ModeCharDevice` — a dependency on x/term would buy nothing but a
dependency.

Five attribute names (`method`, `path`, `status`, `bytes`, `took`) render
positionally and in a fixed order, so a request line reads as a request line
regardless of the order the caller passed them — there is a test for exactly
that, because "same record, two different lines" is the kind of thing that
looks fine until you grep.

### Logging who hit the API, and not logging who did not

The startup line moved from `fmt.Printf` to the logger, so it obeys whatever
the process decided about format. A server that is up and a server that is up
*on the port you meant* look identical otherwise.

For requests, the interesting question is not "log the client" — it is "log the
clients that are not us". Every navigation and every fetch from our own pages
would otherwise stamp our own IP on every line and bury the one caller worth
seeing. `Callers: true` adds `ip`/`ua` only when the request did not come from a
page on this host, decided by `Sec-Fetch-Site`, then `Origin`, then `Referer` —
and `cross-site` is believed over a `Referer` that claims otherwise, since the
browser sets the first and script cannot.

`Skip: mw.SkipNoise` drops `/static/`, `/healthz` and `/favicon.ico`: the
majority of requests and the least informative line in any log.

### The dev loop, and refusing to call it HMR

`go run .` plus four commands by hand was the loop. `howl dev` is the loop now:
watch, regenerate, templ, build, restart, reload. **~540 ms** for a templ edit,
**~390 ms** for a Go edit, measured on the toy app.

Go cannot hot-swap a linked binary. howl (TS) hot-reloads a `.vue` file in
~50 ms because Deno compiles it per request; there is no equivalent here and
pretending otherwise would be a lie the first time someone timed it. What can be
removed is the manual part and the reload keystroke — so that is what was.

Three decisions worth keeping:

**A proxy in front of the restarting app.** The browser talks to the dev server,
never to the child, which is started on a private port. The address bar never
stops answering, a request landing mid-rebuild *waits* for the new binary
instead of failing, and — the reason it works at all — the live-reload
connection survives the restart it is reporting. Doing this without a proxy
means the SSE stream dies with the process and the client has to guess when to
come back.

**A failed build keeps the last good binary serving.** The child is not killed;
the compiler error goes to the browser as an overlay instead. Reloading onto a
broken build replaces a working page with a blank one, which is strictly worse
than a stale page with the error printed over it.

**The application has no dev-mode code.** The dev server passes `HOWL_ADDR`,
`HOWL_PUBLIC_DIR` and `HOWL_DEV`, and `app.New` reads them. `HOWL_PUBLIC_DIR` is
the one that earns its place: static files are `//go:embed`ed, so without it a
one-character CSS change would need a full rebuild. With it, CSS is served from
disk and a stylesheet edit skips Go entirely — the client swaps the `<link>`
rather than reloading, which keeps scroll position, focus and open dropdowns.

Polling, not fsnotify. Walking a few hundred files costs well under a
millisecond and keeps the module at one dependency; it also sidesteps the fact
that most editors save by writing a temp file and renaming it over the original,
which arrives as delete-then-create rather than write. Generated files are
excluded from the watch, or generating the route table triggers a rebuild that
generates the route table.

The dev client is served by the dev server and imported dynamically, so a
production build ships one dead `if` instead of a kilobyte of code it will never
run — and the `error` event had to be renamed `build-error`, because
`EventSource` dispatches its own connection failures under that name and the
overlay would have appeared every time the stream hiccuped.

**The reload signal is a revision, not an event.** The first version broadcast a
`reload` event, which is wrong for the same reason polling for state beats
listening for edges: an event fired while the browser was not connected is gone.
A tab that slept through a rebuild, or one left open across a restart of the dev
server itself, sat there stale.

howl (TS) had already solved this on its `/_howl/alive` socket, so howl-go now
uses the same name and the same shape: a monotonic revision, sent on connect
*and* on every change. The client remembers the first number and reloads on
anything higher, which makes a missed rebuild self-correcting — `EventSource`
reconnects by itself, gets greeted with the current revision, and reloads if it
moved. The clock seeds it so a restarted dev server always outranks the one it
replaced; the +1 covers two rebuilds inside one millisecond.

Measured: page open on /about, dev server killed, file edited, dev server
restarted → the page reloaded itself on reconnect with no interaction.

### The docs site stopped building a wasm binary nobody loaded

`www`'s Makefile built `views.wasm` (5.5 MB) and installed `wasm_exec.js` on
every change. Not one route there is `.client`, so `router.NeedsWasm` published
an empty list, the client was told there was nothing to load, and the binary was
never fetched by anything. Dead weight on every build.

The fix was to delete the target, not to opt the site in. A public documentation
site is precisely the case §3's payload argument rules out: 1.71 MB gzipped to
save a round-trip that prefetch-on-intent already reduces to 0.3 ms. The AOT
renderer is demonstrated by `examples/toy_app`, which is behind no such
constraint — measured there as `wasm render → /dashboard/metrics · 0 bytes ·
server not contacted`.

`www/wasm/main.go` stays, with a `wasm-check` target that compiles it to
`/dev/null` and is not part of `all`. Deleting the build step without that
would leave a file nothing compiles, which rots silently under a `js && wasm`
build tag — invisible to `go build ./...` and to every test.

### `.bare` and `.raw`

howl's `skipInheritedLayouts` and `skipAppWrapper`, as file-name modifiers.
`.bare` drops the layout chain — resolved at generation time, so it costs
nothing at runtime. `.raw` drops the document shell too: the component's markup
is the entire response, for embeds and print views.

---

## 14. The document store

A port of the TypeScript `@hushkey/service-core` + `@hushkey/pg-service` pair:
a document store with an audit envelope, soft delete by default, optimistic
locking, and no migration framework. It lives in `db/`, not `core/`, and
nothing in the framework imports it — the non-goal it amends said "no ORM", and
it still holds: there are no relations, no table mapping and no codegen.

### The struct is the schema, so zod has no counterpart

The TypeScript service needs a validator because its types are gone at run
time; something has to check the write boundary. In Go `encoding/json` already
enforces the shape, and the two things zod adds beyond it are defaults and
invariants. Those became two optional methods:

```go
func (u *User) Defaults()       // on create, before validation
func (u *User) Validate() error // on create and every patch -> db.ErrInvalid
```

`Validate() error` is the method `core/api` already calls on a request body, so
a type that is both a stored document and an endpoint body validates once, in
one implementation. This is the same trade the endpoint layer made, and it is
the reason both layers are smaller here than in the original.

### The envelope is embedded, not wrapped

`Record[T]{Data: T}` would have been the obvious Go shape, and it is wrong
twice: the stored JSON grows a `data` key that the TypeScript services do not
write, and every field access grows a `.Data`. Embedding `db.Doc` keeps the
JSON flat, keeps `u.ID` and `u.Email` side by side, and lets a document be an
`api.Spec` response type with no conversion.

It costs a generic pair — `Service[T any, PT Document[T]]` — because mutating
the envelope needs the pointer. Constraint type inference fills `PT` in from
the `*T` term, so the call site is `pg.New[User](…)` and never
`pg.New[User, *User](…)`. `Document`'s method is unexported, which means the
only way to satisfy it is to embed `Doc`: a collection type cannot accidentally
opt out of the envelope the service assumes is there.

### `Patch` takes a closure, and it is better than `Partial<T>`

Go has no partial struct, and the obvious workarounds are both bad: a
`map[string]any` of field names throws away the compiler, and a second
`UserPatch` type has to be maintained beside the first.

The closure falls out of what the operation already is. Validating a whole
document means reading it first, so the caller may as well be handed the value
that was read:

```go
u, err := Users.Patch(ctx, id, func(u *User) { u.Name = "Ada L." })
```

Field names are checked, the write is locked to the version that was read, and
a lost race re-runs the closure against the *fresh* document instead of
replaying a stale delta — which is the bug a partial-struct API cannot avoid,
because by then the delta is all it has. The cost is that the closure may run
more than once, which is documented and is the reason it must stay a pure edit.

What is written is the diff between the document read and the document
produced, as dotted paths, so `Patch` is not a whole-document overwrite: two
writers touching different fields of the same sub-document do not clobber each
other.

### The backend interface has no type parameter

Documents cross the storage boundary as JSON, so `db.Backend` is not generic:
one `pg.Backend` serves every collection in the process instead of one
instantiation per Go type. The generics stop at the service, where they are
buying something — the typed closure, the typed return.

This is the one place the port is *simpler* than the original rather than
equivalent, and it came from Go's constraints, not in spite of them.

### Deep-set, and the removal it cannot express

Updates go through a recursive `howl_jsonb_deep_set` because plain `jsonb_set`
does not create missing intermediate objects: a patch to `profile.plan` on a
document written before `profile` existed is silently dropped. A lost write,
and one that only appears on old rows.

Deep-set has the mirror problem — it cannot remove a key — so `UpdateOptions`
carries an explicit `Unset []string` alongside the paths. Without it a field a
patch cleared would keep its stored value forever, and the schema report would
never come clean.

The diff only unsets a top-level key the *struct still declares*. A stored key
outside that set is an orphan from an older version of the type, and an
ordinary patch must leave it alone; only `DropField` removes those, on purpose.

### The report is exact, not sampled

The TypeScript service samples fifty documents and infers which fields are
missing. Postgres can answer the real question in one grouped query over
`jsonb_object_keys`, so `db.KeyCounter` is an optional backend capability and
the report says which kind of answer it gave. It matters because the report is
what you press "backfill" against.

### The driver stays out of go.mod

`db/pg` talks to an interface `*sql.DB` and `*sql.Tx` both satisfy, so the
application brings pgx or lib/pq and the framework's `go.mod` keeps its single
dependency. A test dependency would have quietly made that untrue, so the live
Postgres conformance run is its own module in `db/pg/livetest`.

`$in` binds one parameter per value rather than `= ANY($1::text[])` for the
same reason: an array bind needs a driver that encodes Go slices, and there is
no driver here to lean on.

### One suite, two backends

`db/memdb` evaluates the same filter grammar over a map. It is a test double,
but the reason it exists is `db/conformance`: a contract described in prose
drifts, because the first implementation defines what the words meant and the
second implements what it read. Both backends run the same 24 cases, and the
Postgres module runs them three times — plain, cached, and with the filtered
paths promoted to columns, since promoting must change which index is used and
no answer at all.

### What Go cannot do here

Projection cannot return a partial struct. An unselected field comes back as
its zero value, indistinguishable from a stored empty one. The mitigation is
the rule the TypeScript service also has for a different reason: a projected
document is never written to its by-id cache key, because half a document must
not be served to a later `Get`.

---

## 15. Teaching the browser half

The server half of this framework is guessable from its types. The browser half
is not, and the evidence was in what people — and agents — produced with it: an
endpoint, a page, a `db` collection all came out right, and anything reactive
came out shaped like React. A signal read through the store instead of the
signal. `Mount` with no `Unmount`. State parked in a page package where a second
page could never reach it.

That is not a documentation failure in the ordinary sense. `www/docs/05` has
been correct the whole time. It is that the two surfaces an agent actually
touches — `howl_conventions` and `howl_scaffold` — described the *API* and
generated the *markup*, and the shape lives in neither. `signal.Effect(fn)` on
its own implies nothing about which file it belongs in, which pointer may write
it, or what releases it. The example app had all of that, correct and commented,
where only somebody already reading the example would find it.

So the shape became three things instead of prose:

**A recipe, not an API list.** "Making a page interactive" in `llms.txt` opens
with a table that talks people *out* of signals — a form is 0 bytes, an island
is a few lines of vanilla, and the wasm renderer is 1.71 MB gzipped. Then the
store split, the three wires (SSR → hydrate → local), and the `Mount`/`Unmount`
contract. Every rule in it is stated with the silent failure it prevents,
because none of them fail loudly.

**A scaffold that writes the wiring.** `kind: "store"` emits two files, and the
split *is* the lesson: `todos.go` compiles for the server and for wasm, and
`todos_client.go` holds the package-level signals with `publish` gated on the
browser's own instance. A page scaffolded with `client: true, store: "todos"`
comes out with the effect bound to a variable `Unmount` can reach, the repaint
reading through the signal, and the hydrate call written out. The previous
version emitted an `<h1>` — which was, for a `.client` route, everything except
the part that is the point.

**Rules, so the mess is reported and not inherited.** `check` now catches a
`Mount` that subscribes with no `Unmount`, a `signal.Effect` whose stop func is
discarded, server code importing `core/signal` at all, and a store importing
something that cannot compile for wasm. The precision matters: a `Mount` that
only reads — the metrics page logs a row count and fetches once — is fine, and a
rule that fires on correct code is a rule people switch off.

The same argument reached one more rule, from a different direction. templ
generates no `//line` directives, so `undefined: components.SettingsShell`
arrives as `settings_templ.go:412` — a file the author never wrote, at a line
that exists in no source, after two generators have run. But the reference is
resolvable without a compiler: a `.templ` file declares its imports, and every
package in the module declares its names. `check` now indexes both and reports
the unknown component, the wrong argument count and the unexported one at the
line that was typed, with a did-you-mean. The precision work was all in what it
must *not* say: an unexported component used inside its own package is the
normal case, `@r.Component()` on a local value reads exactly like a qualified
call and cannot be resolved, and an argument list contains commas — inside
struct literals, JSON blobs and func literals — so counting them naively
reports working code.

The last group came from the same complaint one step further on: not code that
fails, but code that works and costs too much. A regexp compiled per call
instead of once at package level, a string grown with `+=` per row, an HTTP
request or a `db.Get` per iteration, a `defer` inside a loop. The compiler has
no opinion about any of them, so they survive review by looking exactly like
working code — because they are.

Two things made these worth having. The first is that they reach `.templ`
files, which is where `Mount` and `repaint` live: blanking the `templ`, `css`
and `script` blocks with spaces, newlines kept, leaves Go that parses at its
original line numbers. The second is that they are warnings. A judgement call
that fails a build is a rule people delete rather than argue with.

Calibrating them against this repo is the part worth recording, because four of
the six first hits taught the rule something. `regexp.MustCompile` in `mddocs`
was real and got hoisted. `Commas` in the toy app grew a string per digit in a
function that runs per table row; it uses a `strings.Builder` now. The other
two were the rule being wrong: `fsroutes` declares its accumulator *inside* the
loop, which is one short string per iteration and not a quadratic append, and
the `db` conformance suite is a table-driven test whose loop of single queries
is the entire point. Those became exclusions — a target declared in the loop
body, and any function taking a `*testing.T`. A rule compiling a pattern from
`regexp.QuoteMeta(item)` survived a third way: a pattern built from a value
cannot be hoisted, so "hoist it" is advice the author cannot take, and the rule
now only says it about a constant one.

The measurements that prompted the sweep found nothing to fix on the server. A
simple page renders in 1.5 µs and 2 kB; a 200-row table in 16 µs and 23 kB;
over a real socket both disappear into ~62 µs of loopback. The one thing worth
knowing is that per-request context assembly is O(routes) — `router.Lookup`
re-scans the table the mux already matched, and `NeedsWasm` rebuilds a constant
slice — so a 128-route site pays 12 µs and 282 allocations before rendering
anything. It is the largest server-side number in the framework and it is still
smaller than one loopback RTT, which is why it stayed a note rather than a
change.

---

## 16. Three ceilings, removed

Benchmarking the render path to answer "is the server fast" turned up something
the benchmark was not looking for: the server was fast, and getting slower with
every route added. Context assembly was O(routes) — `router.Lookup` re-scanning
a table the mux had already matched, and `NeedsWasm` rebuilding a constant slice
— so at 128 routes a request spent 11.4 µs and 282 allocations before a byte of
page existed, 89% of the whole request. Both halves are constant per process.
`NeedsWasm` now runs once in `New`, and parameters come from `r.PathValue`,
which is the same data the mux parsed to dispatch. Context assembly is flat at
~930 ns and 18 allocations from 4 routes to 128, and a static route allocates no
parameter map at all.

The lesson is narrow and worth keeping: the cost was invisible because it scaled
with the *table*, not with the request. Nothing an individual page did made it
worse, so no page-level profiling would ever have found it.

`dom.On` was the opposite kind of bug — known, documented, and left alone with a
comment saying listeners live as long as their element. That is true of the DOM
listener and false of the `js.Func` behind it: a `js.Func` is held alive from the
JS side until `Release`, and if the handle is dropped nothing can ever reach it.
Pages mount on every client-side navigation, and the todo page's `repaint`
re-binds a delete button per row on every mutation, so "one leak per call" was
one per row per repaint, for the life of the tab. `On` now returns its release
func, and `dom.Off(release...)` takes a slice of them — which composes with
signals for free, because a stop func and a release func are both `func()`. The
signature change is source-compatible: Go lets a caller ignore a single return
value, so a listener that genuinely should outlive the page still reads the same.
`howl check` warns when the handle is discarded inside a page.

Per-route client data was the real design gap. `ClientData` was one endpoint,
fetched once, handed to every client route and unmarshalled as one type — fine
for the toy app, where every client route wanted the same metrics, and unusable
for anything with two client routes that want different things. The fix follows
the grain the rest of the framework already has: a `//howl:data /api/metrics`
directive on the page file, read by `fsroutes` into `Route.Data`, published as
`router.Client.Pages` keyed by pattern. Nothing is written down twice, and the
generated table stays the only source of truth.

Two details in the client were not obvious. The lookup has to go through the
same pattern matcher `wasmCandidate` uses, because `Pages` is keyed by pattern
and a navigation arrives with a concrete path — an object lookup would resolve
`/blog/{article_id}` for nothing. And the cache holds the *promise*, not the
result, so several routes naming one endpoint share a single request and a
concurrent second render does not start a second fetch. A failed fetch drops its
entry rather than caching the failure, and hands the renderer `{}` instead of
taking it down: a page that needs data decides for itself what empty means.

`renderLocally` became async as a result, which needed the staleness guard the
rest of `navigate` already had — `if (seq !== mine) return` — or a slow data
fetch could apply its fragment after a newer navigation had already won.

Deliberately not fixed: `.client` still gates rendering rather than bundling, so
every page's components link into one `views.wasm`. That is the trade the SPA
feel is bought with, and it stays.

## The flash that looked like slowness

Reported as "the page is instantaneous but the layout is the old one for a bit",
which is the kind of description worth taking literally: both halves were true,
and the second was caused by the first.

`applyHead` removed the outgoing page's tags, appended the incoming ones, and
returned — and the very next line was `outlet.innerHTML = body`. A
`<link rel="stylesheet">` does not load synchronously. So the new markup was
painted after the old page's CSS had been removed and before the new page's had
arrived: one or more frames of the incoming DOM wearing neither page's styles.
The faster the swap, the wider that window looks, because there is nothing else
on screen to explain the delay.

The fix is only an ordering change, but the order is the whole thing. Load the
incoming stylesheets *first*, while the outgoing page is still on screen and
still styled; then remove the old head, apply the rest and swap the body as one
step. Nothing is painted unstyled at any point. Three details make it safe:

- **The common case must not pay.** A page with no stylesheet of its own gets
  `null` back from `preloadHead` and the swap never awaits, so it stays exactly
  as synchronous as before. Only pages that actually bring CSS wait for it.
- **A stylesheet that never answers must not strand the navigation.** The wait
  is raced against a 300 ms timeout, which is longer than a cache hit and long
  enough for a warm CDN; past that it degrades to the old flash rather than to a
  hang.
- **Waiting introduces a race.** The swap now yields, so a newer navigation can
  start mid-wait; it re-checks `seq` before committing, the same guard the rest
  of `navigate` already used.

Removal moved to a generation counter rather than a bare attribute, because the
incoming tags are in the document *before* the outgoing ones come out — the two
sets have to be told apart. Server-rendered head tags marked at boot carry an
empty value, which is not the current generation, so they are removed correctly
on the first navigation.

Prefetch now also warms the sheet: a prefetched fragment already carries its
head, so hovering a link issues a `<link rel="preload" as="style">` for anything
it finds. Hover fires 200-800 ms before the click, which is the entire budget
the swap would otherwise have to wait out.

Verified by booting the real `app.js` in jsdom and driving `applyFragment`
through a navigation: the outgoing stylesheet is still present and the body is
still the old one after the call returns, both change together once `load`
fires, a page with no stylesheet swaps synchronously, and a sheet that never
answers swaps anyway at the timeout. That test is not checked in — jsdom is a
Node dependency, and "no Node dependency" is a stated non-goal of this
framework.

---

## 17. Open questions

- **TinyGo** — would it bring 1.71 MB gzipped down to the 200–800 KB range, and
  does templ's generated code survive its reflection limits?
- **Per-route wasm tables.** `.client` gates *rendering*, not *bundling*: every
  page's components link into `views.wasm` because the one table references them
  all. A wasm-only table containing just the `.client` routes would let the
  linker drop the rest. Currently a deliberate no: one binary is what makes any
  client route instant once loaded, and splitting it trades that for a download
  per route group. Revisit if the binary outgrows the routes that need it.
(Answered since: the typed endpoint layer, which was an open question here until
`core/api` and `core/cmd/fsapis` shipped it — method, roles, validated
query/body/responses, OpenAPI built from the registered table, and a generated
typed client. §13.)
