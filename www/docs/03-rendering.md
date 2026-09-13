# Rendering

A `templ.Component` is just `Render(ctx, io.Writer)`. It cannot tell whether the writer is an HTTP response, a file, or a `strings.Builder` inside WebAssembly. Every mode below is the same component with a different caller.

| mode | caller | output |
|---|---|---|
| cold SSR | `App(...).Render(WithChildren(ctx, route.Component()))` | full HTML document |
| SPA navigation | `page.Render(ctx, w)` | fragment for `#outlet` |
| static export | `page.Render(ctx, file)` | `.html` on disk |
| wasm | `route.Component().Render(ctx, &sb)` | HTML, in the browser |

## The hybrid switch

One function knows the difference, and it lives in the framework:

```go
if r.Header.Get("X-Partial") == "1" {
	c.Render(ctx, w)                    // fragment
	return
}
shell(title, head).Render(templ.WithChildren(ctx, c), w)   // full document
```

## Rendering in the browser

Mark a page `.client` and it is rendered by the wasm build:

```
client/pages/dashboard/metrics/index.client.templ
```

The wasm binary imports the same generated route table and resolves the path through the same `router.Lookup` the server uses. A navigation costs **zero bytes** — the server is not contacted at all, and unlike a prefetch cache it works for routes the user has never visited.

The cost is the payload: a Go wasm binary carries the runtime and GC whether you use them or not — 2.27 MB gzipped for `examples/toy_app`. It is fetched at idle after the cold page has painted, or when the pointer heads toward a link into a client route, never on the critical path.

## Which routes should be `.client`

The decision is one file name at a time, and it is a suggestion rather than a framework default. Both kinds of route send the same server-rendered HTML on the cold request — a `.client` page is not an empty div waiting for a bundle, and it is indexed like any other. What changes is every navigation after the first.

| | plain route | `.client` route |
|---|---|---|
| cold request | full HTML | full HTML — identical |
| navigation | fragment: one RTT plus the render | **0.08 ms, 0 bytes** |
| a route never opened before | a request | already local |
| cost | none | 2.27 MB gzipped, once, lazily |

So the honest answer differs per route inside one application:

- **Behind a login — go full SPA.** A dashboard, an admin, an editor, a back-office table: long sessions, usually a desktop, and the binary amortises over hundreds of navigations. Mark every route under that prefix `.client` and after the first paint the app never contacts the server to change screens — including screens nobody has visited, which a prefetch cache cannot do. `examples/toy_app` does this under `/dashboard`; each file is named `index.client.templ`, there is no directory-wide modifier.
- **Public pages — leave them server-rendered.** Marketing, blog, docs, pricing: a read-once audience on mobile data, where the cold render is already the whole product.

Nothing enforces this. An app with no `.client` route ships no wasm at all and changes routes in 0.3 ms through prefetching; an app where every route is `.client` is a full SPA that still server-renders the first hit and still static-exports. Mixed usually wins only because the public half of a product and the half people work in all day rarely share a visitor.

## Runtime state must cross the wire

A `.client` component is compiled into both the server binary and `views.wasm`.
Package globals belong to those separate binaries, so `build.Tag()`,
`os.Getenv`, `time.Now()` or `runtime.Version()` can change after a local
navigation even though the same component rendered both pages.

Client-renderable components—including their layouts and shared UI—may only
depend on route parameters, explicit route data and hydrated application state.
Put runtime global state in the server context and publish the same typed value
as bootstrap state:

```go
type AppState struct {
	Version string `json:"version"`
	Viewer  Viewer `json:"viewer"`
}

app.Config{
	Data: func(ctx context.Context, path string) context.Context {
		return state.With(ctx, loadAppState(ctx))
	},
	Bootstrap: func(ctx context.Context, path string) any {
		return state.Get[AppState](ctx)
	},
}
```

The WASM renderer receives bootstrap and route data separately:

```go
payload, err := router.DecodeRenderPayload(args[1].String())
ctx, err = state.Hydrate[AppState](ctx, payload.Bootstrap)
json.Unmarshal(payload.RouteData, &pageData)
```

Bootstrap is evaluated per request, serialized in `howl-client`, and retained
across navigation. It is browser-visible, so it must not contain secrets.
`howl check` traces `.client` routes through layouts and local imports; use
`//howl:server` on packages that must never enter that graph.

## `templ Head()`

A page may declare a reserved `Head` component, merged into the document `<head>`:

```templ
templ Head() {
	<title>Metrics — and more</title>
	<meta name="description" content="…"/>
	<link rel="canonical" href="…"/>
}
```

Three rules fall out of making this work:

- **Exactly one `<title>`.** Two title elements and the browser keeps the first, so the page would silently lose to the shell's fallback. The framework splits the title out of the head fragment and the shell emits it once.
- **A fragment has no `<head>`.** SPA responses carry it in an inert `<template data-head>` for the client to merge. Without that, every navigation after the first would keep the first page's title and canonical.
- **Page tags are removed on leaving, not stacked.**

## Static export

```bash
./app -static ./dist
```

Writes every static route to disk. Dynamic routes are skipped and reported — they need a parameter source the filesystem cannot provide.
