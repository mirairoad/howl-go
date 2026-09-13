// The SPA path: what app.js does for a route the wasm build renders. The
// renderer itself is a stub — the contract app.js has with it is three globals
// and one JSON envelope, and that is what these tests pin down. The real
// binary is driven in test/e2e; the Go half's render context in wasm/render.
import { test } from "node:test";
import assert from "node:assert/strict";
import { boot, fragment } from "./harness.mjs";

const json = (v, headers = {}) =>
  new Response(JSON.stringify(v), { headers: { "Content-Type": "application/json", "X-Howl-Build": "b1", ...headers } });

// A wasm route table with a static and a dynamic pattern, a renderer that
// echoes the route so a test can see which one it drew, and a ledger of every
// call across the boundary.
function spa({ path = "/dashboard", config = {}, fetch: handler, render, mount, unmount, outlet } = {}) {
  const calls = { render: [], mount: [], unmount: [] };
  const b = boot({
    path,
    outlet,
    config: { wasm: ["/dashboard", "/dashboard/{tab}"], bootstrap: { viewer: "leo" }, ...config },
    fetch: handler ?? (() => fragment("server", `<p data-server>server</p>`)),
    wasm: {
      render: (route, payload) => {
        calls.render.push({ route, payload });
        return render ? render(route, payload) : `<template data-head><title>${route}</title></template><h1 data-local>${route}</h1>`;
      },
      mount: (p) => {
        calls.mount.push(p);
        mount?.(p);
      },
      unmount: (p) => {
        calls.unmount.push(p);
        unmount?.(p);
      },
    },
  });
  return { ...b, calls };
}

const partials = (fetches) => fetches.filter((f) => f.init.headers?.["X-Partial"] === "1").map((f) => f.url);

test("a navigation to a wasm route is rendered locally: no fragment fetched, head merged, event fired", async () => {
  const s = spa();
  await s.ready();
  const seen = [];
  s.document.addEventListener("howl:navigate", (e) => seen.push(e.detail.path));

  await s.howl.navigate("/dashboard/metrics");

  const outlet = s.document.getElementById("outlet");
  assert.equal(outlet.querySelector("[data-local]")?.textContent, "/dashboard/metrics");
  assert.equal(outlet.dataset.route, "/dashboard/metrics");
  assert.equal(s.document.title, "/dashboard/metrics", "the wasm head template is merged like the server's");
  assert.equal(s.window.location.pathname, "/dashboard/metrics");
  assert.deepEqual(partials(s.fetches), [], "0 bytes: the server was not asked for markup");
  assert.deepEqual(seen, ["/dashboard/metrics"]);
  assert.equal(s.navigations.length, 0);
});

test("the renderer is handed {bootstrap, routeData}: the document's bootstrap and the route's own data", async () => {
  const s = spa({
    config: { wasm: ["/dashboard", "/blog/{id}"], pages: { "/blog/{id}": "/api/article" }, data: "/api/shared" },
    fetch: (url) => (url === "/api/article" ? json({ article: 1 }) : json({ shared: 1 })),
  });
  await s.ready();

  await s.howl.navigate("/blog/hello");
  await s.howl.navigate("/blog/world");
  await s.howl.navigate("/dashboard");

  const [a, b, c] = s.calls.render;
  assert.deepEqual(a.payload, { bootstrap: { viewer: "leo" }, routeData: { article: 1 } }, "a dynamic route resolves its pages entry");
  assert.deepEqual(b.payload.routeData, { article: 1 });
  assert.deepEqual(c.payload, { bootstrap: { viewer: "leo" }, routeData: { shared: 1 } }, "a route without its own entry gets the shared data");
  const hits = (u) => s.fetches.filter((f) => f.url === u).length;
  assert.equal(hits("/api/article"), 1, "two routes on one endpoint cost one request");
  assert.equal(hits("/api/shared"), 1, "the shared endpoint was warmed once, at load");
});

test("a data endpoint that fails hands the renderer {} and is asked again next time", async () => {
  let attempts = 0;
  const s = spa({
    config: { wasm: ["/blog/{id}"], pages: { "/blog/{id}": "/api/article" } },
    path: "/blog/first",
    fetch: () => (++attempts === 1 ? new Response("boom", { status: 500 }) : json({ article: 1 })),
  });
  await s.ready();

  await s.howl.navigate("/blog/a");
  assert.deepEqual(s.calls.render.at(-1).payload.routeData, {}, "the page decides what empty means; the renderer is not taken down");
  await s.howl.navigate("/blog/b");
  assert.equal(attempts, 2, "a failure is not cached");
  assert.deepEqual(s.calls.render.at(-1).payload.routeData, { article: 1 });
});

test("a renderer that declines a route lets the server answer", async () => {
  const s = spa({ render: () => "" });
  await s.ready();
  await s.howl.navigate("/dashboard/metrics");
  assert.deepEqual(partials(s.fetches), ["/dashboard/metrics"]);
  assert.ok(s.document.getElementById("outlet").querySelector("[data-server]"), "the server fragment was swapped in");
});

test("before the renderer is loaded a wasm route is served by the server, and the load starts", async () => {
  // Booted on a plain page: nothing has loaded wasm yet, and this navigation
  // arrives without a pointerover — a keyboard user, a howl.navigate() call.
  const s = spa({ path: "/" });
  assert.equal(typeof s.window.howlRender, "undefined");

  await s.howl.navigate("/dashboard");
  assert.deepEqual(partials(s.fetches), ["/dashboard"], "served from the server");
  assert.ok(s.document.getElementById("outlet").querySelector("[data-server]"));
  await s.tick(5); // the exec shim "loads" on a timer; the binary is fetched after it
  assert.ok(s.fetches.some((f) => f.url.endsWith("views.wasm")), "and the binary download began");

  await s.ready();
  await s.howl.navigate("/dashboard/metrics");
  assert.deepEqual(partials(s.fetches), ["/dashboard"], "the next one is local");
  assert.ok(s.document.getElementById("outlet").querySelector("[data-local]"));
});

test("unmount runs on the outgoing page before its DOM goes; mount on the incoming after it arrives; once each", async () => {
  const outletAt = (s) => s.document.getElementById("outlet").textContent;
  const seen = [];
  let s;
  s = spa({
    outlet: `<p>overview</p>`,
    unmount: (p) => seen.push(["unmount", p, outletAt(s)]),
    mount: (p) => seen.push(["mount", p, outletAt(s)]),
  });
  await s.ready();
  // The cold load mounts the page it landed on, after the renderer is up.
  assert.deepEqual(seen, [["mount", "/dashboard", "overview"]]);

  await s.howl.navigate("/dashboard/metrics");
  assert.deepEqual(seen.slice(1), [
    ["unmount", "/dashboard", "overview"],
    ["mount", "/dashboard/metrics", "/dashboard/metrics"],
  ]);
});

test("a mount hook that throws does not take the navigation down", async () => {
  const s = spa({ mount: () => { throw new Error("page bug"); } });
  await s.ready();
  const done = new Promise((r) => s.document.addEventListener("howl:navigate", r, { once: true }));
  await s.howl.navigate("/dashboard/metrics");
  await done;
  assert.equal(s.document.getElementById("outlet").dataset.route, "/dashboard/metrics");
  assert.equal(s.errors.length, 0, "caught and warned, not thrown");
});

test("once local, hovering a wasm link warms its data but fetches no fragment; a plain link still prefetches", async () => {
  const s = spa({
    outlet: `<a id="w" href="/dashboard/metrics">metrics</a><a id="p" href="/about">about</a>`,
    config: { pages: { "/dashboard/{tab}": "/api/tab" } },
    fetch: (url) => (url === "/api/tab" ? json({ tab: 1 }) : fragment("About", `<h1>About</h1>`)),
  });
  await s.ready();

  // One pointer, one intent: moving to the second link cancels the first
  // link's pending timer, so each hover gets its full PREFETCH_DELAY.
  s.document.getElementById("w").dispatchEvent(new s.window.Event("pointerenter"));
  await s.tick(150);
  s.document.getElementById("p").dispatchEvent(new s.window.Event("pointerenter"));
  await s.tick(150);

  assert.deepEqual(partials(s.fetches), ["/about"], "the wasm route's HTML is pure waste now; the other is a useful head start");
  assert.equal(s.fetches.filter((f) => f.url === "/api/tab").length, 1, "its data is what hover warms");
});

test("a server-only application never asks for the binary or the exec shim", async () => {
  const s = spa({ config: { wasm: [] } });
  await s.tick(30);
  assert.equal(typeof s.window.howlRender, "undefined");
  assert.equal(s.fetches.filter((f) => /wasm/.test(f.url)).length, 0);
  assert.equal(s.document.head.querySelector("script[src]"), null);
});

test("a build id that changed lets the current local render finish, then makes the next one a full load", async () => {
  const s = spa({
    config: { pages: { "/dashboard/{tab}": "/api/tab" } },
    fetch: () => json({ tab: 1 }, { "X-Howl-Build": "b2" }),
  });
  await s.ready();

  await s.howl.navigate("/dashboard/metrics");
  assert.equal(s.navigations.length, 0, "the swap the user is looking at completes");
  assert.ok(s.document.getElementById("outlet").querySelector("[data-local]"));

  await s.howl.navigate("/dashboard/settings");
  assert.equal(s.navigations.length, 1, "the next one reloads: the binary in this tab is from the old build");
});

test("back and forward through wasm routes render locally and run the lifecycle", async () => {
  const s = spa();
  await s.ready();
  await s.howl.navigate("/dashboard/metrics");
  await s.howl.navigate("/dashboard/settings");

  const back = new Promise((r) => s.document.addEventListener("howl:navigate", r, { once: true }));
  s.window.history.back();
  await back;

  assert.equal(s.window.location.pathname, "/dashboard/metrics");
  assert.equal(s.document.getElementById("outlet").querySelector("[data-local]")?.textContent, "/dashboard/metrics");
  assert.deepEqual(partials(s.fetches), []);
  assert.deepEqual(s.calls.unmount.at(-1), "/dashboard/settings");
  assert.deepEqual(s.calls.mount.at(-1), "/dashboard/metrics");
});
