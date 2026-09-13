// The dev client's one connection. A document that leaves gives its
// EventSource back; a document that returns from the back/forward cache opens
// a new one. Without this a tab freezes on the sixth full document load — the
// origin's six HTTP/1.1 sockets all held by documents nobody can see.
import { test } from "node:test";
import assert from "node:assert/strict";
import { JSDOM, VirtualConsole } from "jsdom";
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const aliveJS = readFileSync(path.join(here, "../../core/cmd/howl/alive.js"), "utf8");

function bootDev() {
  const vc = new VirtualConsole();
  vc.on("jsdomError", () => {});
  const { window } = new JSDOM(`<!doctype html><html><head></head><body></body></html>`, {
    url: "http://localhost/",
    runScripts: "outside-only",
    virtualConsole: vc,
  });
  const streams = [];
  window.EventSource = class {
    constructor(url) {
      this.url = url;
      this.closed = false;
      this.listeners = {};
      streams.push(this);
    }
    addEventListener(type, fn) {
      (this.listeners[type] ??= []).push(fn);
    }
    close() {
      this.closed = true;
    }
  };
  // The routes widget asks for the table at load; an empty one keeps it out of
  // the DOM, which is not what this file is about.
  window.fetch = async () => new Response(JSON.stringify({ routes: [] }), { headers: { "Content-Type": "application/json" } });
  window.eval(aliveJS);
  const transition = (type, persisted) => {
    const e = new window.Event(type);
    Object.defineProperty(e, "persisted", { value: persisted });
    window.dispatchEvent(e);
  };
  return { window, streams, transition };
}

test("the stream opens once at load and closes when the document is hidden", () => {
  const { streams, transition } = bootDev();
  assert.equal(streams.length, 1);
  assert.equal(streams[0].url, "/_howl/alive");
  transition("pagehide", false);
  assert.equal(streams[0].closed, true, "the socket is handed back");
  assert.equal(streams.length, 1, "and nothing replaced it");
});

test("a document restored from the back/forward cache reconnects; a fresh show does not double up", () => {
  const { streams, transition } = bootDev();
  transition("pagehide", true);
  transition("pageshow", true);
  assert.equal(streams.length, 2, "one new stream for the restored document");
  assert.equal(streams[1].closed, false);
  transition("pageshow", false);
  assert.equal(streams.length, 2, "a pageshow that is not a restore opens nothing: the load already did");
  transition("pageshow", true);
  assert.equal(streams.length, 2, "and a restore while connected opens nothing either");
});
