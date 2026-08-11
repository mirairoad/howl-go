# Navigation and prefetching

The client runtime (`core/runtime/app.js`, ~8 KB gzipped) intercepts same-origin links, swaps `#outlet`, and calls `pushState`. Any failure degrades to `location.href` — a plain multi-page app.

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
