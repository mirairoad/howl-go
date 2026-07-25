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

The cost is the payload: a Go wasm binary carries the runtime and GC whether you use them or not. Load it lazily, for the routes that need it.

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
