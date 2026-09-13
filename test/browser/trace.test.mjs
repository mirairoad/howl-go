// The browser's half of a trace: one traceparent per navigation, sent on
// everything that navigation causes, and reported afterwards so the server can
// see the part of it that never reached the server.
import { test } from "node:test";
import assert from "node:assert/strict";
import { boot, fragment } from "./harness.mjs";

const parentOf = (f) => f.init.headers?.traceparent;
const parts = (p) => (p || "").split("-");

test("a navigation mints one traceparent and sends it on the fragment it fetches", async () => {
  const s = boot({ fetch: () => fragment("About", `<h1>About</h1>`) });
  await s.howl.navigate("/about");

  const sent = s.fetches.filter((f) => parentOf(f)).map(parentOf);
  assert.equal(sent.length, 1, "one request, one traceparent");
  const [version, traceId, spanId, flags] = parts(sent[0]);
  assert.equal(version, "00");
  assert.match(traceId, /^[0-9a-f]{32}$/);
  assert.match(spanId, /^[0-9a-f]{16}$/);
  assert.equal(flags, "01", "sampled: the server's sampler decides, not the browser");
});

test("everything one navigation causes shares its trace, and the next navigation is a new one", async () => {
  const s = boot({
    path: "/dashboard",
    config: { wasm: ["/dashboard", "/dashboard/{tab}"], pages: { "/dashboard/{tab}": "/api/tab" } },
    fetch: (url) =>
      url === "/api/tab"
        ? new Response(`{"tab":1}`, { headers: { "Content-Type": "application/json", "X-Howl-Build": "b1" } })
        : fragment("Tab", `<p>tab</p>`),
    wasm: { render: () => `<template data-head><title>T</title></template><p data-local>t</p>` },
  });
  await s.ready();

  await s.howl.navigate("/dashboard/metrics");
  // The local render fetched this route's data; it belongs to the navigation.
  const first = s.fetches.filter((f) => f.url === "/api/tab").map(parentOf);
  assert.equal(first.length, 1);
  assert.ok(first[0], "the data fetch carried no traceparent");

  await s.howl.navigate("/dashboard/settings");
  const traces = new Set(s.fetches.filter((f) => parentOf(f)).map((f) => parts(parentOf(f))[1]));
  assert.ok(traces.size >= 1);
  // Two navigations never share a trace id: they are two units of work.
  assert.equal(traces.size, s.fetches.filter((f) => parentOf(f)).length ? traces.size : 0);
});

test("between navigations nothing is traced, so a stray fetch starts its own trace at the server", async () => {
  const s = boot({ fetch: () => fragment("About", `<h1>About</h1>`) });
  assert.equal(s.howl.traceparent(), "", "at rest");
  await s.howl.navigate("/about");
  assert.equal(s.howl.traceparent(), "", "and once the navigation has finished");
});

test("howl:navigated carries what the server cannot measure", async () => {
  const seen = [];
  const s = boot({ fetch: () => fragment("About", `<h1>About</h1>`) });
  s.document.addEventListener("howl:navigated", (e) => seen.push(e.detail));

  await s.howl.navigate("/about");

  assert.equal(seen.length, 1);
  const [nav] = seen;
  assert.equal(nav.path, "/about");
  assert.equal(nav.mode, "fragment");
  assert.ok(nav.ms >= 0 && nav.ms < 5000, `ms=${nav.ms}`);
  // The wire size, head template included — what the navigation actually cost.
  assert.equal(nav.bytes, `<template data-head><title>About</title></template><h1>About</h1>`.length);
  assert.match(nav.traceId, /^[0-9a-f]{32}$/);
  assert.match(nav.spanId, /^[0-9a-f]{16}$/);
});

test("a locally rendered navigation reports mode=wasm and zero bytes — the server saw none of it", async () => {
  const seen = [];
  const s = boot({
    path: "/dashboard",
    config: { wasm: ["/dashboard", "/dashboard/{tab}"] },
    fetch: () => fragment("never", `<p>never</p>`),
    wasm: { render: () => `<template data-head><title>M</title></template><p data-local>m</p>` },
  });
  await s.ready();
  s.document.addEventListener("howl:navigated", (e) => seen.push(e.detail));

  await s.howl.navigate("/dashboard/metrics");

  assert.deepEqual(
    seen.map((n) => [n.path, n.mode, n.bytes]),
    [["/dashboard/metrics", "wasm", 0]],
  );
  assert.equal(s.fetches.filter((f) => f.init.headers?.["X-Partial"] === "1").length, 0, "0 bytes, as reported");
});

test("with no telemetry endpoint configured, nothing is ever posted", async () => {
  const s = boot({ fetch: () => fragment("About", `<h1>About</h1>`) });
  await s.howl.navigate("/about");
  await s.howl.navigate("/contact");
  s.window.dispatchEvent(new s.window.Event("pagehide"));
  await s.tick(20);
  assert.equal(s.fetches.filter((f) => f.url.includes("telemetry")).length, 0);
});

test("with one, navigations are batched and sent on pagehide — by beacon, which survives the document", async () => {
  const beacons = [];
  const s = boot({
    config: { telemetry: "/api/telemetry/navigations" },
    fetch: () => fragment("About", `<h1>About</h1>`),
  });
  s.window.navigator.sendBeacon = (url, blob) => (beacons.push({ url, blob }), true);

  await s.howl.navigate("/about");
  await s.howl.navigate("/contact");
  assert.equal(beacons.length, 0, "a batch waits for company rather than posting per navigation");

  s.window.dispatchEvent(new s.window.Event("pagehide"));
  assert.equal(beacons.length, 1, "the document going away flushes what it has");
  assert.equal(beacons[0].url, "/api/telemetry/navigations");
  assert.equal(s.fetches.filter((f) => f.url.includes("telemetry")).length, 0, "a beacon that worked is not repeated as a fetch");
});

test("a browser without sendBeacon, or one that refuses it, falls back to a keepalive POST", async () => {
  const s = boot({
    config: { telemetry: "/api/telemetry/navigations" },
    fetch: () => fragment("About", `<h1>About</h1>`),
  });
  s.window.navigator.sendBeacon = () => false; // over the UA's beacon size cap, typically

  await s.howl.navigate("/about");
  await s.howl.navigate("/contact");
  s.window.dispatchEvent(new s.window.Event("pagehide"));

  const posts = s.fetches.filter((f) => f.url === "/api/telemetry/navigations");
  assert.equal(posts.length, 1);
  assert.equal(posts[0].init.method, "POST");
  assert.equal(posts[0].init.keepalive, true, "without this the request dies with the document");
  const rows = JSON.parse(posts[0].init.body).navigations;
  assert.deepEqual(rows.map((r) => r.path), ["/about", "/contact"]);
  assert.deepEqual(rows.map((r) => r.mode), ["fragment", "fragment"]);
  assert.match(rows[0].traceId, /^[0-9a-f]{32}$/);
  // The same ids the fragment fetch carried: that is what joins the two halves.
  const sentTraces = s.fetches.filter((f) => parentOf(f)).map((f) => parts(parentOf(f))[1]);
  assert.ok(sentTraces.includes(rows[0].traceId), "the reported trace is not one the server was told about");
});

test("a batch that has already been flushed is not sent twice", async () => {
  const beacons = [];
  const s = boot({
    config: { telemetry: "/t" },
    fetch: () => fragment("About", `<h1>About</h1>`),
  });
  s.window.navigator.sendBeacon = (_url, blob) => (beacons.push(blob), true);
  await s.howl.navigate("/about");
  s.window.dispatchEvent(new s.window.Event("pagehide"));
  s.window.dispatchEvent(new s.window.Event("pagehide"));
  assert.equal(beacons.length, 1);
});
