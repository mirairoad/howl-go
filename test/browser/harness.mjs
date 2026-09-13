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
//   wasm    { render, mount, unmount, routes }: stand in for views.wasm. app.js
//           only ever sees the three globals Go's main() sets, so a stub that
//           sets them is the whole contract from its side. What loading costs
//           and what the Go half renders are tested elsewhere — the real
//           binary in test/e2e, the render context in wasm/render.
//
// Every fetch app.js makes is recorded in `fetches` as { url, init }, whether
// the handler or the harness answered it.
//
// Full-document navigations — the build-drift reload, a .raw route — are the
// one thing jsdom refuses to do; it reports them as "not implemented" errors,
// which the harness collects in `navigations` instead of printing.
export function boot({ path: p = "/", config = {}, outlet = "<p>home</p>", fetch: handler, wasm } = {}) {
  const navigations = [];
  const errors = [];
  const fetches = [];
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
    url = String(url);
    fetches.push({ url, init: init || {} });
    // The binary is bytes app.js never looks at: instantiateStreaming below
    // takes whatever this resolves to.
    if (wasm && url.endsWith("views.wasm")) return new Response(new Uint8Array(0));
    if (!handler) throw new Error("no fetch handler for " + url);
    return handler(url, init || {});
  };
  if (wasm) fakeWasmLoader(window, wasm);
  window.eval(appJS);
  const tick = (ms) => new Promise((r) => setTimeout(r, ms));
  return {
    window,
    document: window.document,
    howl: window.howl,
    navigations,
    errors,
    fetches,
    tick,
    // Resolves once app.js will render locally: the globals exist and the
    // shared data endpoint, if any, has been fetched — the two things
    // loadWasm waits for before it declares the renderer ready.
    async ready(timeout = 2000) {
      const t0 = Date.now();
      while (typeof window.howlRender !== "function") {
        if (Date.now() - t0 > timeout) throw new Error("wasm never became ready");
        await tick(5);
      }
      await tick(10);
    },
  };
}

// fakeWasmLoader gives loadWasm() the three things it touches, in the order it
// touches them: a <script> for wasm_exec.js that "loads", a WebAssembly that
// instantiates, and a Go whose run() installs the globals — exactly where the
// real binary's main() installs them, so a test that asserts on when they
// appear is asserting on the real sequence.
function fakeWasmLoader(window, wasm) {
  const head = window.document.head;
  const append = head.appendChild.bind(head);
  head.appendChild = (node) => {
    if (node.tagName === "SCRIPT" && node.src) {
      setTimeout(() => node.onload?.(), 0);
      return node;
    }
    return append(node);
  };
  window.WebAssembly = {
    instantiateStreaming: async (source) => {
      await source;
      return { instance: {} };
    },
  };
  window.Go = class {
    constructor() {
      this.importObject = {};
    }
    run() {
      window.howlRender = (route, payloadJSON) => wasm.render(route, JSON.parse(payloadJSON));
      window.howlMount = (path, el) => wasm.mount?.(path, el);
      window.howlUnmount = (path) => wasm.unmount?.(path);
      window.howlRoutes = wasm.routes ?? [];
    }
  };
}

// fragment builds the body app.js expects from a fragment response: the head
// in an inert template, then the markup.
export function fragment(title, body, headers = {}) {
  return new Response(`<template data-head><title>${title}</title></template>${body}`, {
    headers: { "Content-Type": "text/html", "X-Howl-Build": "b1", ...headers },
  });
}
