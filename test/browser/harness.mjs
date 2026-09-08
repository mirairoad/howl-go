// Boots the real core/runtime/app.js inside jsdom.
//
// Node is a dependency of this directory only. The framework's stated
// non-goal — no Node, no npm — is about what an application needs to build
// and run, and nothing here changes that: go.mod is untouched, `make` never
// enters this directory, and `make test-browser` is opt-in. The trade is one
// dev-time tool for tests of the code that can only be tested in a browser.
//
// What is stubbed is what jsdom lacks and app.js touches at load: matchMedia,
// scrollTo, fetch. Everything else — history, CustomEvent, template parsing,
// requestAnimationFrame under pretendToBeVisual — is jsdom's own.
import { JSDOM, VirtualConsole } from "jsdom";
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const appJS = readFileSync(path.join(here, "../../core/runtime/app.js"), "utf8");

// boot returns a window with app.js running in it.
//
//   path    the document's URL path and #outlet's data-route
//   config  merged into the howl-client JSON the shell embeds
//   outlet  the outlet's initial markup
//   fetch   (url, init) => Response, for fragments and data
//
// Full-document navigations — the build-drift reload, a .raw route — are the
// one thing jsdom refuses to do; it reports them as "not implemented" errors,
// which the harness collects in `navigations` instead of printing.
export function boot({ path: p = "/", config = {}, outlet = "<p>home</p>", fetch: handler } = {}) {
  const navigations = [];
  const errors = [];
  const vc = new VirtualConsole();
  vc.on("jsdomError", (e) => {
    if (/navigation/i.test(e.message)) navigations.push(e.message);
    else errors.push(e);
  });
  vc.sendTo(console, { omitJSDOMErrors: true });

  const client = JSON.stringify({ build: "b1", ...config });
  const html = `<!doctype html><html><head><title>home</title>
<script type="application/json" id="howl-client">${client}</script>
</head><body><main id="outlet" data-route="${p}">${outlet}</main></body></html>`;
  const dom = new JSDOM(html, {
    url: "http://localhost" + p,
    runScripts: "outside-only",
    pretendToBeVisual: true,
    virtualConsole: vc,
  });
  const { window } = dom;
  window.matchMedia = () => ({
    matches: false,
    addEventListener() {},
    removeEventListener() {},
    addListener() {},
    removeListener() {},
  });
  window.scrollTo = () => {};
  window.fetch = async (url, init) => {
    if (!handler) throw new Error("no fetch handler for " + url);
    return handler(String(url), init || {});
  };
  window.eval(appJS);
  return {
    window,
    document: window.document,
    howl: window.howl,
    navigations,
    errors,
    tick: (ms) => new Promise((r) => setTimeout(r, ms)),
  };
}

// fragment builds the body app.js expects from a fragment response: the head
// in an inert template, then the markup.
export function fragment(title, body, headers = {}) {
  return new Response(`<template data-head><title>${title}</title></template>${body}`, {
    headers: { "Content-Type": "text/html", "X-Howl-Build": "b1", ...headers },
  });
}
