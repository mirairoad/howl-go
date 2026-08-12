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
