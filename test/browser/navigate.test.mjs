// The client router: a fragment swap, the head merge, the event chrome
// listens for, and the two cases that must become a full document load —
// a build id that changed under the tab, and a .raw route.
import { test } from "node:test";
import assert from "node:assert/strict";
import { boot, fragment } from "./harness.mjs";

test("a navigation swaps the fragment into #outlet, merges the head, and announces itself", async () => {
  const seen = [];
  const { document, howl, window, navigations } = boot({
    fetch: (url, init) => {
      assert.equal(init.headers["X-Partial"], "1", "asks for a fragment, not a document");
      return fragment("About — t", `<h1>About</h1>`);
    },
  });
  document.addEventListener("howl:navigate", (e) => seen.push(e.detail.path));

  await howl.navigate("/about");

  const outlet = document.getElementById("outlet");
  assert.equal(outlet.querySelector("h1")?.textContent, "About");
  assert.equal(outlet.dataset.route, "/about");
  assert.equal(document.title, "About — t");
  assert.equal(window.location.pathname, "/about", "pushState moved the URL");
  assert.deepEqual(seen, ["/about"]);
  assert.equal(navigations.length, 0, "no full load");
});

test("a page-owned head tag is removed on leaving instead of stacking", async () => {
  const { document, howl } = boot({
    fetch: (url) =>
      url.startsWith("/a")
        ? fragment("A", `<p>a</p>`).text().then((t) =>
            new Response(t.replace("</template>", `<meta name="description" content="a"/></template>`), {
              headers: { "X-Howl-Build": "b1" },
            }),
          )
        : fragment("B", `<p>b</p>`),
  });
  await howl.navigate("/a");
  assert.equal(document.head.querySelectorAll('meta[name="description"]').length, 1);
  await howl.navigate("/b");
  assert.equal(document.head.querySelectorAll('meta[name="description"]').length, 0);
});

test("a fragment from a newer build turns the navigation into a full document load", async () => {
  const { document, howl, navigations } = boot({
    fetch: () => fragment("About", `<h1>About</h1>`, { "X-Howl-Build": "b2" }),
  });
  await howl.navigate("/about");
  assert.equal(navigations.length, 1, "location.href was assigned");
  assert.equal(document.getElementById("outlet").querySelector("h1"), null, "the stale runtime did not swap it in");
});

test("a prefetch that sees a newer build marks the tab stale for the next navigation", async () => {
  // Hover warms a fragment long before the click. The id on that response
  // counts too: the next navigation, whatever it fetches, is a full load.
  const { howl, navigations } = boot({
    fetch: (url) => fragment("X", `<p>x</p>`, { "X-Howl-Build": url === "/hovered" ? "b2" : "b1" }),
  });
  await howl.prefetch("/hovered");
  assert.equal(navigations.length, 0, "a prefetch never reloads on its own");
  await howl.navigate("/elsewhere");
  assert.equal(navigations.length, 1);
});

test("a .raw route is its own document and is never swapped in", async () => {
  const { howl, navigations } = boot({
    config: { raw: ["/print"] },
    fetch: () => fragment("never", `<p>never</p>`),
  });
  await howl.navigate("/print");
  assert.equal(navigations.length, 1);
});

test("a click on an in-app link is intercepted; a data-no-spa link is not", async () => {
  const { document, window, navigations } = boot({
    outlet: `<a id="in" href="/about">about</a><a id="out" href="/legacy" data-no-spa>legacy</a>`,
    fetch: () => fragment("About", `<h1>About</h1>`),
  });
  // The opted-out link first: the swap that follows the other click replaces
  // the outlet, and this link with it.
  const out = new window.MouseEvent("click", { bubbles: true, cancelable: true, button: 0 });
  document.getElementById("out").dispatchEvent(out);
  assert.equal(out.defaultPrevented, false, "left to the browser");

  const inApp = new window.MouseEvent("click", { bubbles: true, cancelable: true, button: 0 });
  document.getElementById("in").dispatchEvent(inApp);
  assert.equal(inApp.defaultPrevented, true, "intercepted");
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(window.location.pathname, "/about");
  assert.equal(document.getElementById("outlet").querySelector("h1")?.textContent, "About");
  // Exactly one full navigation, and it is the browser following the
  // opted-out link — the intercepted one never reached it.
  assert.equal(navigations.length, 1);
});
