// The dev client. Served by `howl dev` at /_howl/alive.js and imported by
// core/runtime/app.js only when the shell says the dev server is in front —
// so a production build ships none of this, not even the check for it.

const source = new EventSource("/_howl/alive");

// One message both greets a new connection and reloads an existing one: the
// server sends a monotonic revision on connect and again on every rebuild.
// Remember the first, reload on anything higher.
//
// A plain "reload" event cannot do this. An event fired while the browser was
// not connected is simply gone, so a tab that slept through a rebuild, or one
// open across a dev-server restart, would sit there stale until you noticed.
// Comparing a number on every greeting makes a missed rebuild self-correcting.
//
// Reloading is also the honest answer to a rebuilt binary: Go cannot hot-swap a
// linked one, so new markup, new head and new behaviour all arrive together.
let revision = null;
source.addEventListener("alive", (e) => {
  const next = Number(e.data);
  if (!Number.isFinite(next)) return;
  if (revision === null) {
    revision = next;
    return;
  }
  if (next > revision) location.reload();
});

// A stylesheet edit needs no reload, and reloading for one would throw away
// scroll position, focus, and every open dropdown. Swap the element instead,
// and only remove the old one once the new one has painted — otherwise the
// page flashes unstyled between the two.
source.addEventListener("css", () => {
  for (const link of document.querySelectorAll('link[rel="stylesheet"]')) {
    const url = new URL(link.href, location.href);
    url.searchParams.set("howl", String(Date.now()));
    const next = link.cloneNode();
    next.href = url.href;
    next.addEventListener("load", () => link.remove(), { once: true });
    link.after(next);
  }
});

source.addEventListener("build-error", (e) => overlay(e.data));
source.addEventListener("build-ok", () => overlay(null));

// EventSource reconnects on its own, and the revision check above turns that
// reconnection into a reload whenever anything changed while it was away — so
// there is nothing to do here.

function overlay(message) {
  const id = "howl-dev-overlay";
  document.getElementById(id)?.remove();
  if (!message) return;

  const el = document.createElement("div");
  el.id = id;
  el.style.cssText = `position:fixed;inset:0;z-index:2147483647;overflow:auto;
    background:rgba(10,10,12,.96);color:#f3f3f3;padding:2.5rem 3rem;
    font:13px/1.6 ui-monospace,SFMono-Regular,Menlo,monospace`;

  const title = document.createElement("p");
  title.textContent = "build failed";
  title.style.cssText = "margin:0 0 1rem;color:#ff6b6b;font-weight:700;letter-spacing:.12em;text-transform:uppercase";

  const body = document.createElement("pre");
  body.textContent = message; // textContent, not innerHTML: compiler output is not markup
  body.style.cssText = "margin:0;white-space:pre-wrap;word-break:break-word";

  const hint = document.createElement("p");
  hint.textContent = "fix it and save — this closes itself. esc to dismiss.";
  hint.style.cssText = "margin:1.5rem 0 0;color:#8a8a94";

  el.append(title, body, hint);
  document.body.appendChild(el);
  addEventListener("keydown", function esc(ev) {
    if (ev.key === "Escape") {
      el.remove();
      removeEventListener("keydown", esc);
    }
  });
}

// ---------------------------------------------------------------------------
// The routes widget
//
// Every route the application serves, one click away, with the current one
// marked — the thing you actually want while building: jump to a page without
// typing a URL, and see at a glance which routes render in the browser.
//
// Dev only. It is part of this file, which is served by `howl dev` and imported
// only when the shell publishes a live endpoint, so a production build has
// neither the panel nor the /_howl/routes.json it reads.
// ---------------------------------------------------------------------------

const POSITION = "howl.routes.position";
const OPEN = "howl.routes.open";

const panel = document.createElement("div");
panel.id = "howl-routes";
panel.style.cssText = `position:fixed;z-index:2147483646;right:1rem;bottom:1rem;
  width:16rem;max-height:70vh;display:flex;flex-direction:column;overflow:hidden;
  background:#12141a;color:#e8e8ee;border:1px solid #262a33;border-radius:12px;
  box-shadow:0 12px 40px rgba(0,0,0,.45);font:12px/1.5 ui-sans-serif,system-ui,sans-serif`;

const bar = document.createElement("div");
bar.style.cssText = `display:flex;align-items:center;gap:.5rem;padding:.5rem .7rem;
  cursor:grab;user-select:none;border-bottom:1px solid #262a33;background:#171a21`;
bar.innerHTML = `<span style="font-weight:700;letter-spacing:.1em;font-size:10px;text-transform:uppercase;color:#8b8b96">routes</span>
  <span id="howl-routes-count" style="color:#5b5f6b"></span>
  <span style="margin-left:auto;color:#5b5f6b;font-size:10px">⌃R</span>
  <button id="howl-routes-close" style="background:none;border:0;color:#8b8b96;cursor:pointer;font-size:14px;line-height:1">×</button>`;

const list = document.createElement("div");
list.style.cssText = "overflow:auto;padding:.35rem";
panel.append(bar, list);

const badge = document.createElement("button");
badge.textContent = "routes";
badge.style.cssText = `position:fixed;z-index:2147483646;right:1rem;bottom:1rem;display:none;
  padding:.4rem .7rem;border-radius:999px;border:1px solid #262a33;background:#12141a;color:#8b8b96;
  cursor:pointer;font:11px ui-sans-serif,system-ui,sans-serif;box-shadow:0 8px 24px rgba(0,0,0,.4)`;

function show(open) {
  panel.style.display = open ? "flex" : "none";
  badge.style.display = open ? "none" : "block";
  localStorage.setItem(OPEN, open ? "1" : "0");
}

// Dragging, with the position remembered — for the panel by its title bar, and
// for the collapsed pill by itself. A widget that lands on top of the thing you
// are working on, every reload, gets closed once and never reopened.
//
// The pill is a button, so a drag must not also fire its click. Four pixels of
// movement is the line between "moved it" and "pressed it" — below that a
// pointer that wobbles during a click would leave the panel shut.
function draggable(el, handle, key) {
  const saved = JSON.parse(localStorage.getItem(key) || "null");
  if (saved) place(el, saved.x, saved.y);

  let from = null;
  let moved = false;

  handle.addEventListener("pointerdown", (e) => {
    if (e.target.id === "howl-routes-close") return;
    const box = el.getBoundingClientRect();
    from = { dx: e.clientX - box.left, dy: e.clientY - box.top, x: e.clientX, y: e.clientY };
    moved = false;
    handle.setPointerCapture(e.pointerId);
    handle.style.cursor = "grabbing";
  });

  handle.addEventListener("pointermove", (e) => {
    if (!from) return;
    if (Math.abs(e.clientX - from.x) > 4 || Math.abs(e.clientY - from.y) > 4) moved = true;
    if (moved) place(el, e.clientX - from.dx, e.clientY - from.dy);
  });

  handle.addEventListener("pointerup", () => {
    if (!from) return;
    from = null;
    handle.style.cursor = "grab";
    if (!moved) return;
    const box = el.getBoundingClientRect();
    localStorage.setItem(key, JSON.stringify({ x: box.left, y: box.top }));
  });

  // A drag that ends over the pill would otherwise open the panel it just moved.
  handle.addEventListener("click", (e) => {
    if (moved) {
      e.preventDefault();
      e.stopImmediatePropagation();
      moved = false;
    }
  }, true);
}

// Clamped so a window resize, or a drag that overshoots, cannot leave the
// widget somewhere you have to clear localStorage to get it back from.
function place(el, x, y) {
  const w = el.offsetWidth || 160;
  Object.assign(el.style, {
    left: Math.max(0, Math.min(innerWidth - w, x)) + "px",
    top: Math.max(0, Math.min(innerHeight - 40, y)) + "px",
    right: "auto",
    bottom: "auto",
  });
}

function row(route) {
  const here = location.pathname;
  const active = route.pattern === here;
  const dynamic = route.pattern.includes("{");

  const el = document.createElement(dynamic ? "div" : "a");
  if (!dynamic) el.href = route.pattern;
  el.style.cssText = `display:flex;gap:.5rem;align-items:center;padding:.35rem .5rem;border-radius:7px;
    text-decoration:none;color:${active ? "#e8e8ee" : "#a6a6b2"};background:${active ? "#232833" : "transparent"};
    cursor:${dynamic ? "default" : "pointer"}`;

  const name = document.createElement("span");
  name.textContent = route.pattern;
  name.style.cssText = "font-family:ui-monospace,monospace;overflow:hidden;text-overflow:ellipsis;white-space:nowrap";
  el.append(name);

  // A dynamic route has no single URL to link to, so it gets a box to put the
  // parameter in — which is exactly the thing that is tedious to do by hand.
  if (dynamic) {
    const input = document.createElement("input");
    input.placeholder = route.pattern.match(/\{(\w+)\}/)?.[1] ?? "id";
    input.style.cssText = `margin-left:auto;width:5.5rem;background:#0d0f14;border:1px solid #262a33;
      border-radius:5px;color:#e8e8ee;padding:.15rem .35rem;font:11px ui-monospace,monospace`;
    input.addEventListener("keydown", (e) => {
      if (e.key !== "Enter" || !input.value) return;
      howl.navigate(route.pattern.replace(/\{\w+\}/, encodeURIComponent(input.value)));
    });
    el.append(input);
  } else {
    if (route.client || route.mount) {
      const tag = document.createElement("span");
      tag.textContent = route.client ? "wasm" : "mount";
      tag.style.cssText = "margin-left:auto;font-size:9px;letter-spacing:.08em;text-transform:uppercase;color:#6ba3ff";
      el.append(tag);
    }
    el.addEventListener("click", (e) => {
      e.preventDefault();
      howl.navigate(route.pattern);
      setTimeout(paint, 60);
    });
  }
  return el;
}

let routes = [];
function paint() {
  list.replaceChildren(...routes.map(row));
  document.getElementById("howl-routes-count").textContent = routes.length;
}

fetch("/_howl/routes.json")
  .then((r) => r.json())
  .then((data) => {
    routes = data.routes || [];
    if (!routes.length) return;
    document.body.append(panel, badge);
    show(localStorage.getItem(OPEN) !== "0");
    paint();
  })
  .catch(() => {});

draggable(panel, bar, POSITION);
draggable(badge, badge, POSITION + ".pill");
badge.style.cursor = "grab";

badge.addEventListener("click", () => show(true));
bar.addEventListener("click", (e) => { if (e.target.id === "howl-routes-close") show(false); });
addEventListener("keydown", (e) => {
  if (e.ctrlKey && (e.key === "r" || e.key === "R")) { e.preventDefault(); show(panel.style.display === "none"); }
});
addEventListener("popstate", () => setTimeout(paint, 60));
