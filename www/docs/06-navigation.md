# Navigation and prefetching

The client runtime (`core/runtime/app.js`, ~8 KB gzipped) intercepts same-origin links, swaps `#outlet`, and calls `pushState`. Any failure degrades to `location.href` — a plain multi-page app.

## Prefetch on intent

Modelled on Turbo Drive. Hovering a link arms a **100 ms timer** and cancels it on `pointerleave`; keyboard focus and `pointerdown` fire immediately, because those are already commitment.

The delay is the whole point. Firing on the first `pointerover` means a mouse crossing a five-link header issues five server renders nobody asked for.

> Measured: a fast sweep across every nav link produces **0** fetches. Lingering 400 ms on one produces exactly **1**.

Nothing is warmed on page load. Prefetching is skipped under `navigator.connection.saveData` or a 2G `effectiveType`, and per link with `data-no-prefetch`.

## Navigating from code

A link is not always the right control. `window.howl` is the client runtime's whole public API:

```js
howl.navigate("/dashboard");                    // same path a click takes
howl.navigate("/", { replace: true });          // no new history entry
howl.navigate("/reports", { transition: "slide-left", scroll: 0 });
howl.prefetch("/reports/2024");                 // warm it now
howl.island("counter", (el, props) => { … });   // register an island
```

From Go — in `Mount`, in a click handler, anywhere the wasm build runs:

```go
import "github.com/mirairoad/howl-go/core/dom"

dom.Navigate("/dashboard")
dom.Navigate("/", dom.Replace())                       // after a login
dom.Navigate("/reports", dom.Transition("slide-left"))
dom.Prefetch("/reports/2024")
```

`Replace` is for the navigation that should not be in the back stack: submitting a form, finishing a login, correcting a URL. Before the runtime is up, `dom.Navigate` falls back to a full page load rather than doing nothing.

A programmatic navigation into a `.client` route also starts the wasm download if it has not begun. Hovering a link warms it for a mouse user; a keyboard user, a tap and a `navigate()` call all arrive without a `pointerover` ever firing.

## Islands are the application's, not the framework's

`app.js` ships the registry; your app ships the islands.

```html
<script src="/static/app.js" type="module"></script>
<script src="/static/islands.js" type="module"></script>
```

```js
// static/islands.js
howl.island("counter", (el, props) => { … });
```

A registration that arrives after boot hydrates immediately, so load order does not matter. Islands *outside* `#outlet` keep their state across navigation; islands *inside* re-hydrate. That boundary is the SSR/SPA seam.

## What the shell publishes

The client hardcodes no route prefix and no endpoint. The shell hands it everything, derived from the same generated table the server uses:

```templ
@templ.JSONScript("howl-client", router.ClientConfig(ctx))
```

```json
{ "wasm": ["/dashboard", "/todos"], "data": "/api/metrics" }
```

`wasm` comes from `router.NeedsWasm` — every `.client` route and every route with a lifecycle hook. `data` is `Config.ClientData`, fetched once before the first local render and handed to the renderer. Leave it empty and no fetch happens at all; a failed fetch logs a warning and the renderer still starts.

## Two separate questions

`spaTarget(a)` decides whether the router handles a click. `shouldPrefetch(url)` decides whether to warm it.

Conflating them is a nasty bug: a link you decline to *prefetch* is still a link you must *intercept*. One shared helper returning null skips `preventDefault()`, and the browser does a full page load — so the SPA behaviour silently disappears on exactly the routes you optimised.

## Scroll restoration

`history.scrollRestoration` is set to `manual`. The outgoing offset is written to its own history entry before `pushState` and replayed from `popstate`; new navigations start at the top.

The restore must run *inside* the swap callback. `startViewTransition` defers that callback, so scrolling outside it targets the old DOM and the browser clamps the offset to 0.

## Transitions

Declared in markup, styled in CSS. Nothing animates unless something asks for it.

```html
<a href="/x" data-transition-slide-left>   <!-- one link            -->
<html data-transition-fade-up>             <!-- every navigation    -->
<a href="/y" data-transition-none>         <!-- opt this one out    -->
```

The style is `fade` or `slide`, the direction is `left`, `right`, `up` or `down`, and the direction may be omitted (`data-transition-fade`). The nearest declaration wins, so a default on `<html>` stays overridable per link. Back and forward play the same transition reversed — `slide-left` out, `slide-right` back.

Two things reach CSS: a view transition `type`, and `data-howl-transition` on `<html>` for browsers that shipped view transitions before types. Both name the same string.

```css
html[data-howl-transition="slide-left"]::view-transition-old(howl-outlet) { … }
```

`/static/transitions.css` is an opt-in stylesheet implementing the four directions on `#outlet` alone, so persistent chrome does not slide with the page. It is tuned by variable, not by forking:

```css
:root { --howl-duration: 320ms; --howl-slide: 100%; }
```

An app that serves its own `static/transitions.css` replaces the framework's outright.

**`prefers-reduced-motion` wins over every declaration above.** The API does not honour that media query on its own, so the runtime checks it and starts no transition at all — a stylesheet that only neutralises the animation still pays for the snapshot.

Firefox has no View Transitions API; it swaps instantly and everything else behaves identically.

## Progress

A bar appears only after 500 ms. One that flashes on every fast navigation reads as jank; one that never appears makes a slow link feel broken.

## Prefetching is not precompiling

Prefetching moves *when* you pay, not whether. The server still renders the HTML, so a route nobody hovered still costs a round-trip, and changed data costs another.

Marking a route `.client` ships the renderer instead. See [Rendering](/docs/rendering).

## Head merging, and the flash it avoids

A fragment carries its page's head in an inert `<template data-head>`. The shell's own tags were marked once at boot, so page tags swap without touching them.

The order matters more than it looks. The obvious sequence — remove the old page's tags, add the new ones, swap the body — paints the incoming markup *before* its stylesheet has loaded and *after* the outgoing one was thrown away. For one or more frames the new content wears no page CSS at all, which reads as "the content is instant but the layout arrives late". The navigation being fast is what makes it visible.

So the incoming stylesheets are loaded first, with the outgoing page still on screen and still styled, and only then is the old head removed and the body swapped:

1. Parse the incoming head; separate `<link rel="stylesheet">` from everything else.
2. Append the stylesheets and wait for `load` — capped at 300 ms, so a broken one degrades to the old behaviour instead of hanging.
3. Remove the previous page's head, apply the rest, swap `#outlet`, set the title. One visual step.

A page with no stylesheet of its own — most pages — skips steps 2 and 3's wait entirely and the swap stays synchronous. And because a prefetched fragment already carries its head, hover-prefetch also issues a `<link rel="preload" as="style">` for anything it finds, so by click time the wait is usually already over.
