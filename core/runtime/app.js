// ---------------------------------------------------------------------------
// Island registry — the "SPA" half. Each island is a plain function that gets
// the already-server-rendered element plus its props. No virtual DOM, no
// hydration diff: the server owns the markup, the client owns the behaviour.
//
// The framework ships the registry, never the islands: an application declares
// its own with howl.island(name, setup) from its own script. A registration
// that arrives after boot hydrates immediately, so load order does not matter.
// ---------------------------------------------------------------------------

const islands = Object.create(null);

function register(name, setup) {
  islands[name] = setup;
  if (document.readyState !== "loading") hydrate(document);
}

// Hydrate every island inside `root` exactly once.
function hydrate(root) {
  for (const el of root.querySelectorAll("[data-island]")) {
    if (el.dataset.hydrated) continue;
    const setup = islands[el.dataset.island];
    if (!setup) continue;
    el.dataset.hydrated = "1";
    setup(el, JSON.parse(el.dataset.props || "{}"));
  }
}

// ---------------------------------------------------------------------------
// Client router — intercepts same-origin links, asks the server for a fragment,
// swaps #outlet. The document, its scripts, and any island outside the outlet
// are never torn down. That is what makes it feel like an SPA.
// ---------------------------------------------------------------------------

const outlet = document.getElementById("outlet");
const navLog = document.getElementById("nav-log");
let seq = 0;

// ---------------------------------------------------------------------------
// WASM renderer. The same templ components, compiled with GOOS=js GOARCH=wasm
// and shipped to the browser. Once loaded, a route change renders locally —
// nothing is fetched, not even a fragment. Only data ever crosses the network.
// ---------------------------------------------------------------------------

// Everything the client needs to know about the Go half, published by the
// shell from the same generated route table the server uses:
//
//   { "wasm": ["/dashboard", "/todos"], "data": "/api/metrics" }
//
// No hardcoded prefix, and no hardcoded endpoint either — an app whose client
// routes need no data simply omits "data" and the fetch never happens.
const CONFIG = (() => {
  const read = (id) => {
    try {
      return JSON.parse(document.getElementById(id)?.textContent || "null");
    } catch {
      return null;
    }
  };
  const c = read("howl-client");
  // The wasm URLs are content-hashed and published by the shell, so they can be
  // cached for a year: a new build is a new URL, and the browser never spends a
  // conditional request asking whether a 6 MB binary changed.
  if (c) {
    return {
      wasm: c.wasm || [],
      data: c.data || null,
      live: c.live || null,
      binary: c.binary || "/static/views.wasm",
      exec: c.exec || "/static/wasm_exec.js",
    };
  }
  return { wasm: read("howl-wasm-routes") || [], data: null, live: null,
           binary: "/static/views.wasm", exec: "/static/wasm_exec.js" }; // pre-0.2 shells
})();

// The dev client — the /_howl/alive stream, CSS swapping, the build-error
// overlay — is served by `howl dev`, not embedded here. A production build
// ships one dead `if` instead of a kilobyte of code it will never run.
if (CONFIG.live) {
  import(CONFIG.live + ".js").catch((e) => console.warn("howl: dev client unavailable:", e));
}

const WASM_ROUTES = CONFIG.wasm;

// Mirrors router.Route.Match: segment count plus {param} wildcards.
function patternMatches(pattern, path) {
  const a = pattern.split("/").filter(Boolean);
  const b = path.split("/").filter(Boolean);
  if (a.length !== b.length) return false;
  return a.every((seg, i) => (seg.startsWith("{") && seg.endsWith("}")) || seg === b[i]);
}
let wasmPromise = null;
let wasmReady = false;

// Client data, one entry per endpoint rather than one blob for the whole app.
// A route names its own with `//howl:data`; anything that does not falls back
// to CONFIG.data, which is what every route shared before this existed.
const DATA = new Map(); // endpoint URL -> Promise<JSON string>

// The endpoint for a concrete path. CONFIG.pages is keyed by route pattern, so
// a dynamic route resolves through the same matcher the wasm candidate check
// uses — /blog/hello finds the entry filed under /blog/{article_id}.
function dataURLFor(route) {
  if (CONFIG.pages) {
    for (const pattern in CONFIG.pages) {
      if (patternMatches(pattern, route)) return CONFIG.pages[pattern];
    }
  }
  return CONFIG.data || "";
}

// fetchData caches the promise, not the result, so N concurrent renders of the
// same route share one request. A failure is not cached: the entry is dropped
// so the next navigation retries, and the renderer is handed "{}" rather than
// being taken down — a page that needs data decides for itself what empty means.
function fetchData(url) {
  if (!url) return Promise.resolve("{}");
  let p = DATA.get(url);
  if (!p) {
    p = fetch(url, { credentials: "same-origin" })
      .then((r) => r.json())
      .then((v) => JSON.stringify(v))
      .catch((e) => {
        console.warn("howl: client data unavailable:", url, e);
        DATA.delete(url);
        return "{}";
      });
    DATA.set(url, p);
  }
  return p;
}

// warmData starts a route's data fetch without waiting for it — hover and
// focus fire well before the click, so the render usually finds it resolved.
const warmData = (url) => {
  if (wasmCandidate(url)) fetchData(dataURLFor(canonical(url)));
};

const canonical = (u) => (u.length > 1 && u.endsWith("/") ? u.slice(0, -1) : u);

function loadWasm() {
  if (wasmPromise) return wasmPromise;
  wasmPromise = (async () => {
    const t0 = performance.now();
    await new Promise((ok, fail) => {
      const s = document.createElement("script");
      s.src = CONFIG.exec;
      s.onload = ok;
      s.onerror = fail;
      document.head.appendChild(s);
    });
    const go = new Go();
    const { instance } = await WebAssembly.instantiateStreaming(
      fetch(CONFIG.binary),
      go.importObject,
    );
    go.run(instance); // never resolves (Go main blocks on select{}) — do not await
    if (typeof howlRender !== "function") throw new Error("howlRender missing");
    // Data is optional. A site whose client routes render from their own
    // markup (the docs site) publishes no endpoint at all. The shared fallback
    // is warmed here because it was always fetched at this point; per-route
    // endpoints are fetched when their route is first wanted.
    if (CONFIG.data) await fetchData(CONFIG.data);
    wasmReady = true;
    return performance.now() - t0;
  })().catch((e) => {
    wasmPromise = null; // fall back to server fragments
    console.warn("wasm renderer unavailable:", e);
    return null;
  });
  return wasmPromise;
}

// Warm the renderer as soon as the user is anywhere near the app section.
const wasmCandidate = (u) => WASM_ROUTES.some((p) => patternMatches(p, u));

async function renderLocally(url) {
  if (!wasmReady || typeof howlRender !== "function") return null;
  const route = canonical(url);
  // Awaited, but normally already resolved: hover, focus and touchstart all
  // warm it, and the same endpoint is fetched once however many routes share it.
  const html = howlRender(route, await fetchData(dataURLFor(route)));
  if (!html) return null; // unknown route — let the server answer (it 404s)
  // The wasm build returns the same <template data-head> prefix the server
  // sends, so head merging works identically on both paths.
  const meta = (globalThis.howlRoutes ?? []).find((r) => r.path === route);
  return { html, title: meta?.label };
}

// ---------------------------------------------------------------------------
// Prefetch cache. A fragment fetched before the click costs the user 0ms at
// click time, however far away the server is. Hover alone typically buys
// 200-800ms of head start — more than a Sydney->us-east RTT.
// ---------------------------------------------------------------------------

// Header values are bytes, and fetch() surfaces them decoded as ISO-8859-1 —
// a raw UTF-8 title would read back as "Overview â€” Guard". The server
// percent-encodes X-Title; undo that here.
function headerTitle(res) {
  const raw = res.headers.get("X-Title");
  if (!raw) return null;
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw; // malformed escape — a wrong title beats a thrown navigation
  }
}

const CACHE = new Map(); // url -> {html, title}
const INFLIGHT = new Map();
const FRESH_MS = 15000;

// Stylesheets already asked for, by href — a preload is pointless twice and a
// second <link rel=preload> for a sheet the document already has is noise in
// the network panel.
const WARMED = new Set();

// A prefetched fragment carries its page's head, so its stylesheet can be
// fetched now rather than at swap time. Hover fires 200-800ms before the click;
// that is the whole budget the swap would otherwise have to wait out.
function warmStyles(html) {
  const head = splitHead(html).head;
  if (!head) return;
  const tpl = document.createElement("template");
  tpl.innerHTML = head;
  for (const node of tpl.content.querySelectorAll('link[rel="stylesheet"][href]')) {
    const href = node.getAttribute("href");
    if (WARMED.has(href)) continue;
    WARMED.add(href);
    const pre = document.createElement("link");
    pre.rel = "preload";
    pre.as = "style";
    pre.href = href;
    document.head.appendChild(pre);
  }
}

function prefetch(url) {
  if (CACHE.has(url) || INFLIGHT.has(url)) return INFLIGHT.get(url);
  const p = fetch(url, { headers: { "X-Partial": "1" }, credentials: "same-origin" })
    .then(async (res) => {
      const entry = { html: await res.text(), title: headerTitle(res), at: performance.now() };
      CACHE.set(url, entry);
      warmStyles(entry.html);
      return entry;
    })
    .catch(() => null)
    .finally(() => INFLIGHT.delete(url));
  INFLIGHT.set(url, p);
  return p;
}

// Two SEPARATE questions. Conflating them is a bug: a link we decline to
// prefetch is still a link the router must intercept, otherwise the browser
// performs a full page load and the SPA illusion dies.

// (a) Should the client router handle this click?
const spaTarget = (a) => {
  // Boolean data attributes normally have an empty value (`data-no-spa=""`).
  // Check presence: dataset.noSpa is then an empty, falsey string.
  if (!a || a.target || a.hasAttribute("download") || a.hasAttribute("data-no-spa")) return null;
  const u = new URL(a.href, location.origin);
  if (u.origin !== location.origin || u.pathname.startsWith("/static/")) return null;
  return u.pathname + u.search;
};

// (b) Should we spend a round-trip warming it?
const shouldPrefetch = (url) => {
  if (!url || url === location.pathname + location.search) return false;
  // Once wasm can render this prefix locally, fetching its HTML is pure waste.
  // Before that it is a useful bridge, so allow it while wasm is still loading.
  if (wasmReady && wasmCandidate(new URL(url, location.origin).pathname)) return false;
  return true;
};

// ---------------------------------------------------------------------------
// Intent detection, modelled on Turbo Drive.
//
// The important part is the DELAY. Firing on the first pointerover means every
// mouse sweep across a nav bar issues requests for links the user never meant
// to open — on a 5-link header that is 5 server renders per idle mouse
// movement. Turbo waits ~100ms and cancels if the pointer leaves first, which
// filters incidental passes from actual intent.
// ---------------------------------------------------------------------------

const PREFETCH_DELAY = 100;
let hoverTimer = null;
let hoverEl = null;

function cancelIntent() {
  if (hoverTimer) clearTimeout(hoverTimer);
  hoverTimer = null;
  hoverEl = null;
}

function scheduleIntent(el, url) {
  if (hoverEl === el) return;
  cancelIntent();
  hoverEl = el;
  hoverTimer = setTimeout(() => {
    hoverTimer = null;
    warmData(url);
    prefetch(url);
  }, PREFETCH_DELAY);
}

// Don't spend someone else's data plan speculatively.
const conn = navigator.connection;
const frugal = () =>
  Boolean(conn && (conn.saveData || /(^|-)2g$/.test(conn.effectiveType || "")));

// Opt out per link or per subtree, like data-turbo-prefetch="false".
const optedOut = (a) => a.closest("[data-no-prefetch]") !== null;

function intentTarget(e) {
  const a = e.target.closest?.("a[href]");
  if (!a || optedOut(a) || frugal()) return null;
  const url = spaTarget(a);
  return shouldPrefetch(url) ? { a, url } : null;
}

document.addEventListener("pointerenter", (e) => {
  const t = intentTarget(e);
  if (t) scheduleIntent(t.a, t.url);
}, { capture: true, passive: true });

document.addEventListener("pointerleave", (e) => {
  if (hoverEl && e.target.closest?.("a[href]") === hoverEl) cancelIntent();
}, { capture: true, passive: true });

// Keyboard focus and a committed press are intent already — no delay.
for (const ev of ["focusin", "pointerdown", "touchstart"]) {
  document.addEventListener(ev, (e) => {
    const t = intentTarget(e);
    if (t) {
      cancelIntent();
      warmData(t.url);
      prefetch(t.url);
    }
  }, { passive: true });
}

// Deliberately NOT warming every nav link on load: that turns one visit into
// N server renders for pages the user may never open. Hover/focus/touchstart
// fire 200-800ms before the click, which is already more than an RTT.
// NB: requestIdleCallback's 2nd arg is IdleRequestOptions, not a delay — and it
// must stay bound to window. Getting this wrong throws at module top level and
// silently disables the whole router.
const idle = (fn) => (window.requestIdleCallback ? window.requestIdleCallback(() => fn()) : setTimeout(fn, 1));

// Pull the WASM renderer down only for users heading into /fast — it is far too
// big to put on the critical path of a content page.
idle(() => {
  if (wasmCandidate(location.pathname)) loadWasm();
});
document.addEventListener("pointerover", (e) => {
  const a = e.target.closest?.("a[href]");
  if (a && wasmCandidate(new URL(a.href, location.origin).pathname)) loadWasm();
}, { passive: true });


// ---------------------------------------------------------------------------
// Scroll restoration. The browser's own restoration assumes a document load;
// with innerHTML swaps it either does nothing or fires at the wrong moment, so
// take it over: remember the position before leaving, restore it on
// back/forward, and start at the top on a new navigation.
// ---------------------------------------------------------------------------

if ("scrollRestoration" in history) history.scrollRestoration = "manual";

const rememberScroll = () =>
  history.replaceState({ ...(history.state || {}), scroll: window.scrollY }, "");

// ---------------------------------------------------------------------------
// Progress bar, shown only after a delay. A bar that flashes on every fast
// navigation reads as jank; one that never appears makes a slow link feel
// broken. Turbo uses 500ms.
// ---------------------------------------------------------------------------

const PROGRESS_AFTER = 500;
let progressTimer = null;
const bar = document.createElement("div");
bar.className = "progress";
bar.hidden = true;
document.body.appendChild(bar);

function progressStart() {
  clearTimeout(progressTimer);
  progressTimer = setTimeout(() => {
    bar.hidden = false;
    bar.classList.add("running");
  }, PROGRESS_AFTER);
}

function progressDone() {
  clearTimeout(progressTimer);
  bar.hidden = true;
  bar.classList.remove("running");
}

// ---------------------------------------------------------------------------
// <head> merging. A fragment carries its page's head in an inert
// <template data-head>; the shell's own tags are marked once at boot so page
// tags can be swapped out without touching them.
// ---------------------------------------------------------------------------

(function markShellHead() {
  // Everything the server put between the markers belongs to the page.
  let inPage = false;
  for (const node of [...document.head.childNodes]) {
    if (node.nodeType === Node.COMMENT_NODE) {
      if (node.data === "page-head") inPage = true;
      else if (node.data === "/page-head") inPage = false;
      continue;
    }
    if (inPage && node.nodeType === Node.ELEMENT_NODE) {
      node.setAttribute("data-page-head", "");
    }
  }
})();

// The page's Go-side lifecycle hook, if it declared one. Runs after the markup
// is in the DOM, on cold load and after every client-side navigation.
function runUnmount(path) {
  if (!path) return;
  try {
    globalThis.howlUnmount?.(path);
  } catch (e) {
    console.warn("unmount hook failed:", e);
  }
}

function runMount(path) {
  try {
    globalThis.howlMount?.(path, outlet);
  } catch (e) {
    console.warn("mount hook failed:", e);
  }
}

function splitHead(html) {
  const m = html.match(/^<template data-head>([\s\S]*?)<\/template>/);
  return m ? { head: m[1], body: html.slice(m[0].length) } : { head: "", body: html };
}

// A page's head is applied in two steps, and the split is the whole point.
//
// Doing it in one — remove the old tags, add the new ones, swap the body on the
// next line — paints the incoming markup before its stylesheet has loaded, and
// after the outgoing one has been thrown away. The result is a frame of one
// page's DOM wearing another page's styles: the content is instant and the
// layout arrives late, which reads as the navigation being slow when in fact it
// was too fast.
//
// So: load the incoming stylesheets first, with the outgoing page still on
// screen and still styled, and only then remove the old head and swap. Nothing
// is ever painted unstyled, and a page with no stylesheet of its own — most
// pages — takes no extra frame at all, because preloadHead returns null and the
// swap stays synchronous.
let headGeneration = 0;

// A stylesheet that never answers must not strand the navigation. 300ms is
// longer than a cache hit (~0ms) and than a warm CDN, and short enough that a
// broken one degrades to the old flash rather than to a hang.
const HEAD_TIMEOUT = 300;

function parseHead(html) {
  const gen = String(++headGeneration);
  const out = { gen, title: null, blocking: [], rest: [] };
  if (!html) return out;
  const tpl = document.createElement("template");
  tpl.innerHTML = html;
  for (const node of [...tpl.content.children]) {
    // <title> is set as a property, never appended: a second title element in
    // the document is ignored by the browser, which keeps the first.
    if (node.tagName === "TITLE") {
      out.title = node.textContent;
      continue;
    }
    node.setAttribute("data-page-head", gen);
    const blocks = node.tagName === "LINK" && node.rel === "stylesheet";
    (blocks ? out.blocking : out.rest).push(node);
  }
  return out;
}

// Appends the incoming stylesheets and resolves when they have loaded. Returns
// null when there is nothing to wait for, so the caller can stay synchronous.
function preloadHead(incoming) {
  if (!incoming.blocking.length) return null;
  const loaded = incoming.blocking.map(
    (node) =>
      new Promise((done) => {
        node.addEventListener("load", done, { once: true });
        node.addEventListener("error", done, { once: true }); // a 404 must not block the nav
        document.head.appendChild(node);
      }),
  );
  return Promise.race([
    Promise.all(loaded),
    new Promise((r) => setTimeout(r, HEAD_TIMEOUT)),
  ]);
}

// Removes the outgoing page's head and applies the rest of the incoming one.
// Called in the same turn as the body swap, so the two are one visual step.
function commitHead(incoming) {
  for (const el of document.head.querySelectorAll("[data-page-head]")) {
    // Anything not from this navigation: the previous page's tags, and the
    // server-rendered ones marked at boot with an empty value.
    if (el.getAttribute("data-page-head") !== incoming.gen) el.remove();
  }
  for (const node of incoming.rest) document.head.appendChild(node);
  if (incoming.title !== null) document.title = incoming.title;
}

// ---------------------------------------------------------------------------
// View transitions. Declared in markup, styled in CSS, resolved here.
//
//   <a href="/x" data-transition-slide-left>   slide, per link
//   <html data-transition-fade>                a default for every navigation
//   <a href="/y" data-transition-none>         opt this one link back out
//
// Nearest declaration wins, so a global default on <html> is overridable per
// link. Nothing animates unless something asks for it: an unstyled browser
// default fade on every navigation is the framework making a design decision
// it has no business making.
//
// The name reaches CSS two ways — `types` for browsers that have it, and a
// data attribute on <html> for those that shipped view transitions first.
// ---------------------------------------------------------------------------

const REDUCE_MOTION = matchMedia("(prefers-reduced-motion: reduce)");
const OPPOSITE = { left: "right", right: "left", up: "down", down: "up" };
const PREFIX = "data-transition-";

const VT_TYPES = (() => {
  try {
    return CSS.supports("selector(:active-view-transition-type(x))");
  } catch {
    return false;
  }
})();

// "data-transition-slide-left" -> "slide-left". First one wins; declaring two
// on one element is a typo, not a composition.
function transitionOn(el) {
  for (const { name } of el.attributes) {
    if (name.startsWith(PREFIX)) return name.slice(PREFIX.length);
  }
  return null;
}

function resolveTransition(el) {
  // A reduced-motion request is not a preference to weigh against the author's
  // — it wins outright, which is why this is checked before anything else.
  if (REDUCE_MOTION.matches) return null;
  for (let n = el; n; n = n.parentElement) {
    const t = transitionOn(n);
    if (t) return t === "none" ? null : t;
  }
  return null;
}

// Back should undo what forward did, or the history stack feels like it only
// moves one way. Only the direction flips; the style stays put.
function reverse(name) {
  if (!name) return name;
  const cut = name.lastIndexOf("-");
  const dir = cut < 0 ? "" : name.slice(cut + 1);
  return OPPOSITE[dir] ? name.slice(0, cut + 1) + OPPOSITE[dir] : name;
}

function runTransition(name, swap) {
  if (!name || !document.startViewTransition) return swap();

  const root = document.documentElement;
  root.dataset.howlTransition = name;
  // Only clear what we set: a navigation that starts mid-transition has already
  // overwritten this, and clearing it then would strip the live one's styling.
  const clear = () => {
    if (root.dataset.howlTransition === name) delete root.dataset.howlTransition;
  };

  try {
    const vt = VT_TYPES
      ? document.startViewTransition({ update: swap, types: [name] })
      : document.startViewTransition(swap);
    vt.updateCallbackDone.catch(swap);
    vt.finished.then(clear, clear);
  } catch {
    clear();
    swap();
  }
}

function applyFragment(url, entry, push, restore, transition, replace) {
  // Record the outgoing page's scroll on ITS history entry before pushing the
  // new one, otherwise the offset lands on the wrong entry. A replace throws
  // the outgoing entry away, so there is nothing to remember.
  if (push) {
    if (replace) {
      history.replaceState({ url }, "", url);
    } else {
      rememberScroll();
      history.pushState({ url }, "", url);
    }
  }

  // View transitions are a nicety; the swap is the contract. A transition that
  // aborts (rapid clicks, hidden tab) never invokes its callback, which would
  // silently leave the old DOM on screen — so always have a fallback path, and
  // guard against running the swap twice.
  let done = false;
  // Waiting on a stylesheet is the one thing here that yields, so a newer
  // navigation can start mid-swap. It must not then be overwritten by this one.
  const mine = seq;

  const swap = async () => {
    if (done) return;
    done = true;

    const { head, body } = splitHead(entry.html);
    const incoming = parseHead(head);
    // Load the incoming page's stylesheets while the outgoing page is still on
    // screen and still styled. Null — no stylesheet of its own — keeps this
    // whole function synchronous, which is the common case.
    const ready = preloadHead(incoming);
    if (ready) {
      await ready;
      if (seq !== mine) return; // a newer navigation won while we waited
    }

    // Tear the outgoing page down before its DOM disappears.
    runUnmount(outlet.dataset.route);
    commitHead(incoming);
    outlet.innerHTML = body;
    outlet.dataset.route = new URL(url, location.origin).pathname;
    hydrate(outlet);
    runMount(outlet.dataset.route);
    // Again, now that the incoming markup exists: the first call updated the
    // links that survive a navigation, this one updates the links that arrived
    // with the fragment.
    markActive();
    // Must happen AFTER the content exists: startViewTransition defers this
    // callback, so scrolling outside it targets the old, possibly shorter DOM
    // and the browser clamps the offset to 0.
    window.scrollTo(0, restore ?? 0);
  };
  runTransition(transition, swap);
  if (entry.title && !/<title/i.test(entry.html)) document.title = entry.title;
  // Immediately, on the links outside #outlet: the nav should light up the
  // moment the URL changes, not when the content lands.
  markActive();
}

async function navigate(url, { push = true, restore = 0, transition = null, replace = false } = {}) {
  const mine = ++seq;
  const t0 = performance.now();

  // Local render: the component runs in the browser. Zero bytes, zero RTT, and
  // unlike the prefetch cache this works for routes never visited before.
  if (wasmCandidate(url)) {
    const local = await renderLocally(url);
    if (seq !== mine) return; // a newer navigation won the race
    if (local) {
      applyFragment(url, local, push, restore, transition, replace);
      navLog && (navLog.textContent =
        `wasm render → ${url} · ${(performance.now() - t0).toFixed(1)} ms · 0 bytes · server not contacted`);
      return;
    }
    // Not loaded yet: serve this navigation from the server and start the
    // download so the next one is local. Hovering a link warms it too, but a
    // keyboard user, a programmatic howl.navigate() and a touch tap all arrive
    // here without a pointerover ever having fired.
    loadWasm();
    warmData(url);
  }

  // Cache hit: render now, no network on the critical path.
  const hit = CACHE.get(url);
  if (hit) {
    applyFragment(url, hit, push, restore, transition, replace);
    const age = performance.now() - hit.at;
    navLog && (navLog.textContent =
      `precached nav → ${url} · ${(performance.now() - t0).toFixed(1)} ms · 0 RTT` +
      (age > FRESH_MS ? " · revalidating…" : ""));
    // Stale-while-revalidate: refresh in the background, only re-swap if the
    // user is still on this route and the server actually returned something new.
    if (age > FRESH_MS) {
      CACHE.delete(url);
      const fresh = await prefetch(url);
      // An innerHTML swap destroys focus, caret position, scroll and any typed
      // input. If the user is mid-interaction, keep the stale DOM — this is the
      // structural limit of swap-based rendering, and where a real VDOM wins.
      const busy = outlet.contains(document.activeElement) &&
        /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName);
      if (busy) {
        navLog && (navLog.textContent = `precached nav → ${url} · 0 RTT · revalidation deferred (input focused)`);
      } else if (fresh && seq === mine && location.pathname + location.search === url && fresh.html !== hit.html) {
        // Deliberately untransitioned: this is a background refresh of the page
        // the user is already looking at, and animating it would read as a
        // navigation they did not perform.
        applyFragment(url, fresh, false, window.scrollY, null);
        navLog && (navLog.textContent = `precached nav → ${url} · 0 RTT · revalidated in background`);
      }
    }
    return;
  }

  document.body.classList.add("loading");
  progressStart();
  try {
    const entry = await prefetch(url);
    if (seq !== mine) return; // a newer navigation won the race
    if (!entry) throw new Error("fetch failed");
    applyFragment(url, entry, push, restore, transition, replace);
    navLog && (navLog.textContent =
      `cold nav → ${url} · fragment ${entry.html.length} B · ${Math.round(performance.now() - t0)} ms (paid the RTT)`);
  } catch {
    location.href = url; // any failure degrades to a normal page load
  } finally {
    if (seq === mine) {
      document.body.classList.remove("loading");
      progressDone();
    }
  }
}

// Any same-origin link in the document, not just the header — a sidebar or a
// breadcrumb lives outside #outlet, so nothing re-renders it on navigation and
// its active state would otherwise stay frozen on the first page visited.
//
// Two things are set, because two different stylesheets want different things:
// the `.active` class for plain CSS, and `aria-current="page"` for utility CSS
// that targets it (Tailwind's `aria-[current=page]:` variant) — and which is
// also what a screen reader announces. The server should render the same
// attribute on a cold load so the first paint agrees with every paint after it.
//
// A link is current when the path matches OR when the page sits underneath it,
// so a sidebar entry for /logs stays lit on /logs/42. That mirrors
// router.Under(ctx, prefix), which is what the server uses — the two halves
// disagreeing about which link is current is a bug you only see mid-navigation.
// "/" is excluded from the prefix rule, or it would be current everywhere.
function markActive() {
  const here = location.pathname;
  for (const a of document.querySelectorAll("a[href]")) {
    let path;
    try {
      const u = new URL(a.href, location.origin);
      if (u.origin !== location.origin) continue;
      path = u.pathname;
    } catch {
      continue;
    }
    const on = path === here || (path !== "/" && here.startsWith(path.replace(/\/$/, "") + "/"));
    a.classList.toggle("active", on);
    if (on) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
}

document.addEventListener("click", (e) => {
  if (e.defaultPrevented || e.button !== 0) return;
  if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  const a = e.target.closest("a[href]");
  const href = spaTarget(a);
  if (!href) return;
  e.preventDefault();
  if (new URL(href, location.origin).href === location.href) return;
  cancelIntent();
  navigate(href, { transition: resolveTransition(a) });
});

addEventListener("popstate", (e) => {
  cancelIntent();
  // No link to read the intent from, so a back/forward step takes the document
  // default — played backwards.
  navigate(location.pathname + location.search, {
    push: false,
    restore: e.state?.scroll ?? 0,
    transition: reverse(resolveTransition(document.documentElement)),
  });
});

// ---------------------------------------------------------------------------
// Public API. Everything an application — or Go running in wasm, through
// core/dom — is allowed to call. Anything above this line is internal.
//
//   howl.navigate("/dashboard", { replace: true })
//   howl.prefetch("/reports/2024")
//   howl.island("counter", (el, props) => { … })
// ---------------------------------------------------------------------------

globalThis.howl = {
  navigate(url, opts = {}) {
    return navigate(String(url), {
      transition: opts.transition || null,
      replace: Boolean(opts.replace),
      restore: typeof opts.scroll === "number" ? opts.scroll : 0,
    });
  },
  prefetch(url) {
    prefetch(String(url));
  },
  island: register,
  hydrate,
  config: CONFIG,
};

hydrate(document);

// Also at boot, not only after a navigation: a shell that renders its own
// active state and one that leaves it to the client should look identical on
// the first paint.
markActive();

// Only routes marked client or declaring a lifecycle hook need Go in the
// browser. Server-only applications publish an empty WASM_ROUTES list and must
// not pay for (or 404 on) wasm_exec.js and views.wasm.
//
// On a qualifying cold load the binary may still be downloading when the DOM
// is ready, so the mount hook waits for the shared promise.
if (wasmCandidate(location.pathname)) {
  // Compiling Go's WASM runtime is substantial work. Let the cold SSR page
  // paint and remain responsive first; pointer intent still starts it sooner
  // when the user heads toward a client route.
  idle(() => loadWasm().then(() => runMount(location.pathname.replace(/(.)\/$/, "$1"))));
}
