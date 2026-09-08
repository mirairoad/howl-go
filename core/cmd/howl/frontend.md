# Frontend recipes

The browser half of howl-go, as recipes. Every topic has the same five parts: **When** to use it, the **Rules**, a complete **Example** that compiles, what goes **Wrong** when it is written from JS-framework habits, and the **Check** that proves it works. Read the checklist first; then the topic you are about to write. The examples are lifted from `examples/toy_app` — `/todos` and `/lab` — where they run.

Three facts everything below follows from. There is no component tree: a templ component renders once to a writer and is finished, so "update the screen" is *render it again into the same element*. There is no virtual DOM: `Render` is `innerHTML`, so the element it replaces loses focus, and anything you bind to a node inside it is gone after the next repaint. There is one browser, one user and one tab: state is package-level signals, and any handler can reach them.

## checklist

**When** — before writing any code that runs in the browser.

**Rules**

1. State is a signal. Not a DOM attribute, not a class name, not a Go variable read by a handler. If two things need to agree, they read the same signal.
2. The DOM is written only by effects. A handler writes a signal; an effect reads it and touches the DOM. Never both in one place.
3. Everything a repaint replaces is listened to with `Delegate` on the root. `On` only on elements outside the repainted region.
4. Inputs the user types into live outside the element a repaint renders.
5. A page that has a store embeds the server's snapshot in its markup and restores it in `Mount`. No fetch to get the state the server just rendered.
6. `Mount` registers; the scope releases. `Unmount` only for a goroutine or a timer.
7. Two writes in one handler go in `signal.Batch`.
8. Fetch in a goroutine, never in the callback. A goroutine writes signals and registers nothing.
9. No `syscall/js` in a page. `core/dom` only.
10. Run `howl check` after editing.

**Example** — the shape of every interactive page:

```go
func Mount() {
	root := dom.Root()
	var sn store.Snapshot                       // 5: hydrate from the document
	if err := dom.Embedded("todos", &sn); err == nil {
		store.Client().Restore(sn)
	}
	root.Delegate("click", "[data-del]", func(e dom.Event) {   // 3: delegated
		mutate(store.Op{Kind: "del", ID: atoi(e.Target().Attr("data-del"))})
	})
	root.Query("[data-form]").On("submit", func(e dom.Event) { // outside the list: On is fine
		add(e.Target().Query("input").Value())
	})
	signal.Effect(func() {                                     // 2: the only DOM writer
		root.Query("[data-list]").Render(ui.TodoList(store.Todos.Get()))
	})
}
```

**Wrong** — `root.Query("[data-del]").On(...)` inside `repaint` (rebinds per row, and `howl check` warns); a handler that calls `SetHTML` directly (the next effect run overwrites it); a `var open bool` toggled by a click and read by another handler (nothing observes it, nothing updates); a goroutine in `Mount` that calls `signal.Effect` (outside the scope, leaks per visit).

**Check** — navigate away and back three times, do one action, count its effect once. `howl check` clean.

## page

**When** — a route that changes on screen after the first paint.

**Rules**

- The file is `client/pages/<dir>/index.templ` (server-rendered fragment on navigation, `Mount` runs after) or `index.client.templ` (also rendered by the wasm build). Both get `Mount`; both are plain `func`, not `templ`.
- `Mount` runs in a scope. Register everything in it, synchronously.
- `Unmount` is optional. Write one only to cancel a goroutine or timer that `Mount` started.
- Read state through the signal, not through the store: `store.Todos.Get()` subscribes, `store.Client().List()` does not.

**Example**

```go
package counter

import (
	"strconv"

	"github.com/mirairoad/howl-go/core/dom"
	"github.com/mirairoad/howl-go/core/signal"
)

var count = signal.Of(0)

templ Page() {
	<section>
		<button data-inc>+1</button>
		<output data-count>0</output>
	</section>
}

func Mount() {
	dom.Root().Delegate("click", "[data-inc]", func(dom.Event) {
		count.Update(func(n int) int { return n + 1 })
	})
	signal.Effect(func() {
		dom.Root().Query("[data-count]").SetText(strconv.Itoa(count.Get()))
	})
}
```

**Wrong** — `templ Mount()` (a templ block writes HTML, it cannot "do"); a `release` slice and `dom.Off` in `Unmount` (the scope did it); `stop := signal.Effect(...)` kept in a package var (harmless, pointless); reading `count` in the click handler and writing the DOM there (works once, then the effect and the handler disagree).

**Check** — the `howl_scaffold` page with `client: true` is this file. Diff against it.

## store

**When** — state the server also owns, mutated by the user, rendered by real components. This is the default for any list, table or record a page edits. A page-local signal is only for state no other page reads.

**Rules**

- Two files: `client/store/<name>.go` (domain: types, `Snapshot`, `Op`, `Apply`, the context pair; compiles for wasm) and `client/store/<name>_client.go` (the package-level signals and `publish`).
- The server renders from the context: `Config.Data` calls `store.WithTodos(ctx, srv.Snapshot())`, the page reads `store.TodosFrom(ctx)`.
- **The page serialises the snapshot it rendered from**: `@templ.JSONScript("todos", store.SnapshotFrom(ctx))` in the markup. **`Mount` restores it**: `dom.Embedded("todos", &sn)` then `store.Client().Restore(sn)`. This is mandatory for a store page: the browser store must start with what is on screen, and no request is made to get there.
- A mutation is `store.Client().Apply(op)` — local first, instant repaint — then the same `op` is sent to the server from a goroutine. The server runs the same `Apply`.
- The server never writes a signal. `publish` guards on `s == client`.

**Example** — the whole handoff, in order:

```go
// server, Config.Data
ctx = store.WithTodos(ctx, db.Snapshot())

// page markup
<ul data-list>@ui.TodoList(store.TodosFrom(ctx))</ul>
@templ.JSONScript("todos", store.SnapshotFrom(ctx))

// page Mount
var sn store.Snapshot
if err := dom.Embedded("todos", &sn); err != nil {
	dom.Warn("[todos] no embedded snapshot:", err.Error())
} else {
	store.Client().Restore(sn)               // publishes; the effect below renders from it
}
signal.Effect(func() {
	dom.Root().Query("[data-list]").Render(ui.TodoList(store.Todos.Get()))
})

// a mutation
func mutate(op store.Op) {
	store.Client().Apply(op)                 // repaints now
	go func() {                              // tells the server after; never blocks the user
		if _, err := apiclient.New("").SyncTodos(context.Background(), []store.Op{op}); err != nil {
			dom.Warn("[todos] sync deferred:", err.Error())
		}
	}()
}
```

**Wrong** — fetching the snapshot in `Mount` from an endpoint (a request, an empty-then-full flash, and `howl check` warns `store-not-embedded`); rendering the page from `store.Client().List()` on the server (the server's store is per-process, the page must read ctx); `Todos.Set(...)` in a server handler (a data race across requests); a store that imports `net/http` or `database/sql` (pages import it, pages compile to wasm).

**Check** — `howl_scaffold kind:"store"` then `kind:"page"` with `client: true, store: "<name>"` writes both files and the page already wired. Network tab on load: no request for the store's data.

## events

**When** — anything the user does.

**Rules**

- `root.Delegate(event, selector, fn)` for anything inside a region a repaint replaces. One listener on the root, matched by selector, covers rows that do not exist yet.
- `el.On(event, fn)` only for elements outside every repainted region: the page's form, a filter box, a modal's field.
- The handler gets `dom.Event`: `Target()` is the matched element, `Value()` its value, `Key()` for keyboard events, `PreventDefault()` if you must.
- `submit` and clicks on links are prevented for you. Nothing else is: checkboxes tick, keys type.
- Only bubbling events delegate. `focusin`/`focusout`, not `focus`/`blur`. `change` for checkboxes and selects, `input` for text as it is typed.
- A handler writes signals. It does not write the DOM.

**Example**

```go
box := root.Query("[data-filter]")                              // outside the list
box.On("input", func(e dom.Event) { filter.Set(e.Value()) })
box.On("keydown", func(e dom.Event) {
	switch e.Key() {
	case "Enter":
		signal.Batch(func() { addItem(e.Value()); filter.Set("") })
		e.Target().SetValue("")
	case "Escape":
		e.Target().SetValue("")
		filter.Set("")
	}
})
root.Delegate("change", "[data-toggle]", func(e dom.Event) {      // inside the list
	toggle(atoi(e.Target().Attr("data-toggle")))
})
```

**Wrong** — `for _, btn := range root.QueryAll("[data-del]") { btn.On("click", ...) }` after a repaint (per-row, per-repaint; `howl check` warns); `On("click")` on a row's button and wondering why it stops working after the first edit (the node was replaced); reading `box.Value()` inside a `keydown` for the *previous* character (use `input`, or read on the next event); `Delegate("focus", ...)` (does not bubble).

**Check** — after a repaint, the action still fires. Type in the box: focus stays.

## list

**When** — a collection rendered from a signal, with per-row actions.

**Rules**

- One templ component for the rows, called by the server for the first paint and by the effect for every one after.
- The effect calls `Render(component)` on the container. That is the entire repaint.
- Per-row actions carry their id in a `data-` attribute and are delegated on the root.
- Equality on the slice signal (`signal.WithEq`) so a restore that changed nothing does not repaint.
- Derived counts with `DeriveEq`, read by their own small effect, so a rename does not rewrite the count.

**Example**

```go
templ Items(items []Item) {
	for _, it := range items {
		<li>
			<span>{ it.Text }</span>
			<button data-del={ strconv.Itoa(it.ID) }>×</button>
		</li>
	}
}

func Mount() {
	root := dom.Root()
	root.Delegate("click", "[data-del]", func(e dom.Event) { del(atoi(e.Target().Attr("data-del"))) })
	signal.Effect(func() { root.Query("[data-list]").Render(Items(items.Get())) })
	signal.Effect(func() { root.Query("[data-count]").SetText(strconv.Itoa(count.Get())) })
}
```

**Wrong** — building the rows with string concatenation in Go (the server's markup and the browser's drift); `SetHTML` from the click handler (overwritten by the next effect run); one effect that renders the list *and* fills a form field (every keystroke in the list re-fills the field).

**Check** — delete the last row: count and list agree. Restore the same snapshot: zero repaints.

## form

**When** — the user enters something.

**Rules**

- Prefer a real `<form>`; `submit` is prevented for you, and Enter works.
- The form is outside the repainted region. Its `On("submit")` reads the fields through `e.Target().Query(...)`.
- Local first: `Apply(op)`, then the server in a goroutine.
- Clear the field from the handler after the write. That is not "writing the DOM": the field is not state, it is input.
- Keep a `method="post" action="/api/..."` on the form so it still works before the wasm loads.

**Example**

```go
templ Page() {
	<form method="post" action="/api/todos" data-form>
		<input name="text" required autocomplete="off" data-input/>
		<button type="submit">Add</button>
	</form>
	<ul data-list>@ui.TodoList(store.TodosFrom(ctx))</ul>
}

root.Query("[data-form]").On("submit", func(e dom.Event) {
	input := e.Target().Query("[data-input]")
	text := strings.TrimSpace(input.Value())
	if text == "" {
		return
	}
	mutate(store.Op{Kind: "add", Text: text})
	input.SetValue("")
})
```

**Wrong** — `Delegate("click", "button[type=submit]", ...)` (misses Enter, and the form still posts); the form inside the `Render`ed element (typing loses focus on the first repaint); `e.PreventDefault()` on `submit` (already done — harmless, but a sign the author is fighting the runtime).

**Check** — press Enter in the field: a row appears, the document does not reload, the field is empty.

## modal

**When** — a dialog, drawer or panel that opens from a button. The single most common thing to get wrong.

**Rules**

- The modal's open state is one signal. The id being edited, or a bool. **Every** way in and out — button, backdrop, Escape, save, cancel — writes that signal and nothing else.
- One effect owns the modal's DOM: it reads the signal, toggles `hidden`, fills the fields on open, moves focus. It is the only code that touches the dialog.
- The modal markup is in the page, outside every repainted region, rendered `hidden` by the server.
- The buttons that open it are inside the list, so they are delegated. The modal's own field is outside, so it gets `On`.
- Save is a `Batch`: write the record, close the modal, one repaint.
- Read the record with `Peek` inside the effect when filling the field, so editing the list underneath does not re-fill what the user is typing.
- No `showModal()`, no `<dialog>` API, no class toggling from a handler. If it needs no data, a `<details>` or a checkbox with a sibling selector needs no code at all.

**Example** — from `/lab`:

```go
// markup, outside the list
<div class="modal" data-modal hidden>
	<div class="modal-backdrop" data-close></div>
	<div class="modal-card" role="dialog" aria-modal="true">
		<input data-edit-text autocomplete="off"/>
		<button data-save>save</button>
		<button data-close>cancel</button>
	</div>
</div>

// state
var editing = signal.Of(0) // the id being edited, 0 when closed

// Mount
modal := root.Query("[data-modal]")
field := modal.Query("[data-edit-text]")
root.Delegate("click", "[data-edit]", func(e dom.Event) { editing.Set(atoi(e.Target().Attr("data-edit"))) })
root.Delegate("click", "[data-close]", func(dom.Event) { editing.Set(0) })
root.Delegate("click", "[data-save]", func(dom.Event) { saveEdit(field.Value()) })
field.On("keydown", func(e dom.Event) {
	switch e.Key() {
	case "Enter":
		saveEdit(e.Value())
	case "Escape":
		editing.Set(0)
	}
})
signal.Effect(func() {                     // the only code that touches the modal
	id := editing.Get()
	modal.Hide(id == 0)
	if id == 0 {
		return
	}
	for _, it := range items.Peek() {      // Peek: fill on open, not on every list change
		if it.ID == id {
			field.SetValue(it.Text)
		}
	}
	field.Focus()
})

func saveEdit(text string) {
	id := editing.Peek()
	if id == 0 || strings.TrimSpace(text) == "" {
		return
	}
	signal.Batch(func() {
		items.Update(func(all []Item) []Item { /* set Text on id */ return all })
		editing.Set(0)
	})
}
```

**Wrong** — the open button calls `modal.Hide(false)` directly and the close button calls `modal.Hide(true)` (two writers, and the next repaint of the page around them does not know it is open); the modal inside the `Render`ed list (it vanishes on the first repaint); `items.Get()` in the modal effect (every edit of the list re-fills the field mid-typing); a `visible` bool in a Go var (nothing observes it).

**Check** — open, type, toggle a checkbox in the list behind it: the field keeps its text and focus. Escape closes. Open again: the field shows the current text.

## filter

**When** — search, sort, paginate a list from a signal.

**Rules**

- The filter is a signal. The derived view is computed inside the render effect, or with `Derive` if two effects need it.
- The input is outside the list.
- Filtering is a pure function over the items: `visible(items, filter, sort)`. The same function runs on the server if the page takes a query parameter.

**Example**

```go
var filter = signal.Of("")

box.On("input", func(e dom.Event) { filter.Set(e.Value()) })
signal.Effect(func() {
	root.Query("[data-list]").Render(Items(visible(items.Get(), filter.Get())))
})
```

**Wrong** — hiding rows by toggling a class per row from the `input` handler (state in the DOM, lost on the next repaint); filtering with a fetch per keystroke when the items are already in the store.

**Check** — type, then delete an item: the filter still applies.

## navigation

**When** — moving between routes, or re-rendering after a write.

**Rules**

- A link is a link. `app.js` intercepts it, fetches the fragment or renders it locally, swaps `#outlet`, runs `Unmount` then `Mount`.
- Programmatic: `dom.Navigate("/path")`, `dom.Navigate("/", dom.Replace())`. `dom.Prefetch` warms one.
- After a write on the current page, the store already repainted; there is nothing to navigate to. If the page is server-rendered only, `howl.navigate(url, {fresh: true})` from JS re-fetches past the prefetch cache.
- Anything outside `#outlet` survives navigation. Anything inside is replaced, and its page's scope is disposed.
- A deploy while the tab is open: the next navigation after the server's build id changes is a full load. Nothing to do.

**Wrong** — `location.href = ...` from Go (loses the SPA, the scope, the prefetch cache); keeping page state in a package var and expecting it to reset per visit (it does not; reset it in `Mount` or keep it in the store).

**Check** — navigate away and back: `Mount` logged once per visit, the previous visit's effects did not fire.

## animation

**When** — something moves every frame: a progress bar, a countdown, a canvas, a transition that CSS cannot express.

**Rules**

- `dom.Frame(func(t float64) bool)` runs once per paint until it returns false. Released with the scope.
- Set width, transform, text directly from the frame callback. Do **not** write a signal per frame: an effect is a repaint, and sixty repaints a second of a list is the cost this framework has no virtual DOM to absorb.
- For enter/leave of a whole page, use the view transitions `data-transition-*` attributes, not code.
- A timer that ticks a signal once a second is fine: `time.NewTicker` in a goroutine, cancelled in `Unmount`.

**Example** — from `/lab`:

```go
bar := root.Query("[data-bar]")
dom.Frame(func(t float64) bool {
	pct := int(t) % 1000 / 10
	bar.SetAttr("style", "width: "+strconv.Itoa(pct)+"%")
	return true
})
```

**Wrong** — `ticks.Set(...)` at 60 Hz driving a list effect; `time.Sleep(16 * time.Millisecond)` in a loop (fights the display, and blocks nothing usefully); a frame loop that survives the page (return false, or let the scope stop it).

**Check** — leave the page: the loop stops. The repaint counter does not move while the bar animates.

## islands

**When** — a widget that is JS-shaped and local: a dropdown, a colour picker, a code editor, something that must work before or without the wasm. Or persistent chrome outside `#outlet`.

**Rules**

- `data-island="name" data-props='{…}'` in the markup, `howl.island("name", (el, props) => { … })` in your own script. The framework ships no islands.
- An island owns its element and nothing else. It does not reach into a Go page's DOM.
- If it needs the store's state, that state is not an island's; make it a page.

**Wrong** — an island and a `Mount` both writing the same element (they race); an island holding domain state (it is not on the server, not in the snapshot, not restored).

**Check** — the island works with `views.wasm` deleted.
