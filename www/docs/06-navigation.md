# Navigation and prefetching

The client runtime (`core/runtime/app.js`, ~7 KB gzipped) intercepts same-origin links, swaps `#outlet`, and calls `pushState`. Any failure degrades to `location.href` — a plain multi-page app.

## Prefetch on intent

Modelled on Turbo Drive. Hovering a link arms a **100 ms timer** and cancels it on `pointerleave`; keyboard focus and `pointerdown` fire immediately, because those are already commitment.

The delay is the whole point. Firing on the first `pointerover` means a mouse crossing a five-link header issues five server renders nobody asked for.

> Measured: a fast sweep across every nav link produces **0** fetches. Lingering 400 ms on one produces exactly **1**.

Nothing is warmed on page load. Prefetching is skipped under `navigator.connection.saveData` or a 2G `effectiveType`, and per link with `data-no-prefetch`.

## Two separate questions

`spaTarget(a)` decides whether the router handles a click. `shouldPrefetch(url)` decides whether to warm it.

Conflating them is a nasty bug: a link you decline to *prefetch* is still a link you must *intercept*. One shared helper returning null skips `preventDefault()`, and the browser does a full page load — so the SPA behaviour silently disappears on exactly the routes you optimised.

## Scroll restoration

`history.scrollRestoration` is set to `manual`. The outgoing offset is written to its own history entry before `pushState` and replayed from `popstate`; new navigations start at the top.

The restore must run *inside* the swap callback. `startViewTransition` defers that callback, so scrolling outside it targets the old DOM and the browser clamps the offset to 0.

## Progress

A bar appears only after 500 ms. One that flashes on every fast navigation reads as jank; one that never appears makes a slow link feel broken.

## Prefetching is not precompiling

Prefetching moves *when* you pay, not whether. The server still renders the HTML, so a route nobody hovered still costs a round-trip, and changed data costs another.

Marking a route `.client` ships the renderer instead. See [Rendering](/docs/rendering).
