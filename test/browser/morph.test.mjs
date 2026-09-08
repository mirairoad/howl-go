// howl.morph: what Element.Render in Go calls. The contract, in order of how
// much a page depends on it: unchanged nodes are the same nodes afterwards, a
// keyed row is moved rather than rebuilt, the field being typed into keeps
// its value, checked follows the markup, and leftovers go.
import { test } from "node:test";
import assert from "node:assert/strict";
import { boot } from "./harness.mjs";

const list = (html) => boot({ outlet: `<ul id="l">${html}</ul>` });

test("a keyed row that moved is the same node, updated in place", () => {
  const { document, howl } = list(`<li data-key="1">a</li><li data-key="2">b</li>`);
  const ul = document.getElementById("l");
  const [a, b] = ul.children;
  howl.morph(ul, `<li data-key="2">b</li><li data-key="1">a2</li>`);
  assert.deepEqual([...ul.children].map((li) => li.dataset.key), ["2", "1"]);
  assert.equal(ul.children[0], b, "row 2 moved, not rebuilt");
  assert.equal(ul.children[1], a, "row 1 moved, not rebuilt");
  assert.equal(a.textContent, "a2", "and its text was updated");
});

test("an unkeyed node of the same kind at the same position is updated in place", () => {
  const { document, howl } = list(`<li>x</li>`);
  const ul = document.getElementById("l");
  const li = ul.firstElementChild;
  howl.morph(ul, `<li class="done">y</li>`);
  assert.equal(ul.firstElementChild, li);
  assert.equal(li.textContent, "y");
  assert.equal(li.className, "done");
  howl.morph(ul, `<li>z</li>`);
  assert.equal(li.className, "", "an attribute the markup dropped is removed");
});

test("the field being typed into keeps its value; a blurred one follows the markup", () => {
  const { document, howl } = boot({ outlet: `<div id="f"><input id="i" value=""/></div>` });
  const f = document.getElementById("f");
  const i = document.getElementById("i");
  i.value = "typing";
  i.focus();
  assert.equal(document.activeElement, i);
  howl.morph(f, `<input id="i" value="server"/>`);
  assert.equal(document.getElementById("i"), i, "same node");
  assert.equal(i.value, "typing", "not overwritten while focused");
  i.blur();
  howl.morph(f, `<input id="i" value="server"/>`);
  assert.equal(i.value, "server", "synced once the user is not in it");
});

test("checked follows the markup, even on the focused checkbox", () => {
  const { document, howl } = boot({ outlet: `<div id="f"><input id="c" type="checkbox"/></div>` });
  const f = document.getElementById("f");
  const c = document.getElementById("c");
  c.focus();
  howl.morph(f, `<input id="c" type="checkbox" checked/>`);
  assert.equal(c.checked, true);
  howl.morph(f, `<input id="c" type="checkbox"/>`);
  assert.equal(c.checked, false, "a rollback un-ticks it");
});

test("nodes the markup no longer has are removed; new ones are inserted where they belong", () => {
  const { document, howl } = list(`<li data-key="1">a</li><li data-key="2">b</li><li data-key="3">c</li>`);
  const ul = document.getElementById("l");
  const b = ul.children[1];
  howl.morph(ul, `<li data-key="0">new</li><li data-key="2">b</li>`);
  assert.deepEqual([...ul.children].map((li) => li.dataset.key), ["0", "2"]);
  assert.equal(ul.children[1], b);
});

test("text nodes are updated, not replaced", () => {
  const { document, howl } = boot({ outlet: `<p id="p">before</p>` });
  const p = document.getElementById("p");
  const text = p.firstChild;
  howl.morph(p, "after");
  assert.equal(p.firstChild, text);
  assert.equal(p.textContent, "after");
});

// Row transitions: opt-in per list, two attributes, and the diff must not see
// a row that is on its way out.
test("data-animate-rows: an inserted row enters, a removed row leaves, and a leaving row is invisible to the diff", async () => {
  const { document, howl, tick } = boot({ outlet: `<ul id="l" data-animate-rows><li data-key="1">a</li></ul>` });
  const ul = document.getElementById("l");
  const a = ul.firstElementChild;

  howl.morph(ul, `<li data-key="1">a</li><li data-key="2">b</li>`);
  const b = ul.children[1];
  assert.equal(b.hasAttribute("data-entering"), true, "marked on insert");
  await tick(150);
  assert.equal(b.hasAttribute("data-entering"), false, "unmarked within the timer even with no frames");

  howl.morph(ul, `<li data-key="2">b</li>`);
  assert.equal(a.isConnected, true, "still in the document while leaving");
  assert.equal(a.hasAttribute("data-leaving"), true);
  assert.equal(ul.children.length, 2);

  // Re-added while still fading: a new row beside the fading one, not the
  // fading one resurrected.
  howl.morph(ul, `<li data-key="2">b</li><li data-key="1">a again</li>`);
  assert.equal(ul.children.length, 3);
  assert.notEqual(ul.children[2], a);
  assert.equal(a.hasAttribute("data-leaving"), true, "the fading one was not touched");

  await tick(450);
  assert.equal(a.isConnected, false, "gone at the timeout with no transitionend");
  assert.equal(ul.children.length, 2);
});

test("without data-animate-rows a removed row is simply removed", () => {
  const { document, howl } = list(`<li data-key="1">a</li>`);
  const ul = document.getElementById("l");
  const a = ul.firstElementChild;
  howl.morph(ul, ``);
  assert.equal(a.isConnected, false);
  assert.equal(ul.children.length, 0);
});
