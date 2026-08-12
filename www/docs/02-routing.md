# Routing

Routes come from the directory tree. `core/cmd/fsroutes` walks `client/pages` and generates a table that the HTTP mux, the layout chain, the static export and the wasm renderer all read. Adding a page is adding a file.

## The tree

```
client/pages/
  app.templ                      document shell — reserved, never a route
  index.templ                    /
  about/index.templ              /about
  blog/index.templ               /blog
  blog/article_id.dyn.templ      /blog/{article_id}
  dashboard/layout.templ         wraps /dashboard and everything below
  dashboard/index.client.templ   /dashboard, also rendered in the browser
```

Every `.templ` file is a route except three reserved names:

| name | meaning |
|---|---|
| `app.templ` | the document shell, root only |
| `layout.templ` | wraps its directory and everything below it |
| `*.component.templ` | colocated markup, never a route |

## Modifiers

Behaviour is encoded in dot-separated suffixes on the file name:

| file | route | meaning |
|---|---|---|
| `index.templ` | `/dir` | server-rendered |
| `index.client.templ` | `/dir` | also rendered in the browser by wasm |
| `article_id.dyn.templ` | `/dir/{article_id}` | path parameter |
| `article_id.dyn.client.templ` | `/dir/{article_id}` | both, either order |
| `print.bare.templ` | `/dir/print` | no layouts, keeps the document shell |
| `embed.raw.templ` | `/dir/embed` | no layouts, no shell — the page is the whole reply |

An unknown modifier is a hard error — a silently ignored typo is a route that quietly loses a capability.

`.bare` drops the layout chain at generation time, so it costs nothing at runtime: the route simply has no layouts. Use it for a print view or a checkout step that should not inherit the app chrome.

`.raw` drops the shell as well. The response is the component's markup and nothing else — no `<html>`, no `<head>`, no title merging. That is what you want for an embed, an email preview, or a fragment fetched by something that is not this framework's client runtime.

Read a parameter with `router.Param(ctx, "article_id")`.

## Why suffixes and not `_layout` or `[id]`

Because the Go toolchain rejects both. See [Toolchain constraints](/docs/constraints).

## Components

A route's component is whichever zero-argument `templ Name()` its file declares first, so files sharing a directory just give their components different names. Two names are reserved: `Head` and `Page` behaviour is described in [Rendering](/docs/rendering) and [Lifecycle](/docs/lifecycle).

## Layouts

`layout.templ` wraps its directory and everything beneath it. The chain is resolved at build time by walking up the tree, so a nested page never imports its parent — which is what keeps the generated table free of import cycles.

```templ
templ Layout() {
	<nav class="tabs">
		for _, r := range router.Children(router.Routes(ctx), "/dashboard") {
			@ui.Tab(r.Pattern, r.Label)
		}
	</nav>
	{ children... }
}
```

## Navigation is derived

`router.Nav(routes)` returns the top-level static routes. There is no `nav` flag: a nested route reaches the user through its layout's tabs, and a route with a parameter has no single URL to link to.

## Overriding a pattern

When the filesystem cannot express what you need:

```go
//howl:route /custom/path
templ Page() { … }
```
