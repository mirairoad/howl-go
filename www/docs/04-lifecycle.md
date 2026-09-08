# Lifecycle

templ has no lifecycle and cannot have one: a component renders once to a writer and is finished. There is no mount, no effect, no re-render. So "do something on first paint" is a plain Go function in the page file.

```go
func Mount()   { … }   // after this page's markup is in the DOM
func Unmount() { … }   // optional: teardown that is not a registration
```

Both are reserved names, both optional, both run in the browser — on the cold load and again after every client-side navigation to that route.

## Touching the DOM

Through `core/dom`, not `syscall/js`:

```go
func Mount() {
	root := dom.Root()
	dom.Log("rows:", len(root.QueryAll("[data-rows] tr")))

	root.Delegate("click", "[data-add]", func(dom.Event) {
		store.Client().Add("from a Go click handler")
	})
}
```

This matters more than it looks. A `.templ` file's generated Go is compiled for **both** targets, so importing `syscall/js` there would break the server build. `core/dom` splits the platform once — `dom_js.go` is real, `dom_stub.go` is no-ops — and the page stays neutral, needs no build tag, and compiles into the server binary as dead code that is never called.

## Mount runs in a scope

Everything `Mount` registers — a `signal.Effect`, a `signal.Watch`, a `dom` listener — is released when the page is swapped out. The runtime opens a scope around `Mount` and disposes it before the next page's markup goes in. There is no stop func to keep, no release slice, and nothing to write in `Unmount` for them.

Measured, on the example todos page: three away-and-back navigations then one mutation logs its watcher **once**. Before the scope existed the same page needed eleven lines of bookkeeping to get the same number, and forgetting one of them logged four times.

The scope is lexical: it closes when `Mount` returns. A goroutine started in `Mount` — the hydrate fetch — finishes later, so anything it registers is outside the scope and lives for the tab. Goroutines write signals; the effects registered in `Mount` react. That is the shape hydration already has.

`Unmount` is for what is not a registration: a timer to cancel, a draft to save, a WebSocket to close.

## Listeners: `On` and `Delegate`

```go
root.Query("[data-form]").On("submit", func(e dom.Event) { add(e.Target().Query("input").Value()) })
root.Delegate("click", "[data-del]", func(e dom.Event) { del(e.Target().Attr("data-del")) })
```

`On` binds to one element. `Delegate` binds once to the root and matches every descendant that fits the selector — the ones in the markup now and the ones a later repaint renders. Inside a region a repaint replaces, `Delegate` is the only one that keeps working, because `Render` throws away the node an `On` was bound to. `howl check` warns on a listener bound per row inside a loop, which is the habit `Delegate` replaces.

The handler receives the event: `Target()` is the matched element, `Value()` its value, `Key()` the key for a keyboard event, `PreventDefault()` when you need it. A `submit`, and a `click` that lands on a link, are prevented before the handler runs — the browser would otherwise navigate away mid-handler. Nothing else is: a click on a checkbox still toggles it, a keydown still types.

Outside a scope — an app-shell control registered once at startup — both return the release func, and it is the only handle to the `js.Func` underneath. `dom.Off(release...)` takes several.

## The repaint

```go
func repaint() {
	items := store.Todos.Get()
	dom.Root().Query("[data-list]").Render(ui.TodoList(items))
}
```

`Render` takes any templ component and draws it into the element — the same component the server rendered, rendered again in the browser. On an element the navigation has already thrown away it is a no-op, which is the ordinary end of an effect's life.

The swap is a morph, done by `app.js`: the element's existing nodes are reconciled to the new markup. A row with a `data-key` (or an `id`) is moved rather than rebuilt when it changes position, an unchanged node stays the same node, a checkbox mid-toggle keeps its state, and the field being typed into keeps its value and focus. Give rows a key. Measured on `/lab`: after a toggle, a reverse, a delete and a filter, the untouched `<li>` elements are the same objects they were before. There is still no state above the DOM — a listener bound with `On` to a node the morph replaced is gone — so `Delegate` remains the listener for anything inside a repainted region.

## Hydrating

A page with a store does not fetch its state: the server already rendered from it, so the page serialises that snapshot into its own markup and `Mount` reads it back.

```go
// markup
@templ.JSONScript("todos", store.SnapshotFrom(ctx))

// Mount
var sn store.Snapshot
if err := dom.Embedded("todos", &sn); err == nil {
	store.Client().Restore(sn) // publishes to the signal; the effect renders from it
}
```

The fragment carries the script, so it works on a cold load and after a client-side navigation alike, and the browser store starts with exactly what is on screen. `howl check` warns on a `Restore` with no `Embedded`.

## Fetching

Use `dom.GetJSON` / `dom.PostJSON`, or the generated typed client, for what the page does not already have — telling the server about a mutation, mostly. Both are the browser's own `fetch()`; `net/http` under wasm links a TLS stack the browser already has, and costs 2 MB gzipped for it.

```go
go func() {
	if _, err := apiclient.New("").SyncTodos(context.Background(), []store.Op{op}); err != nil {
		dom.Warn("[todos] sync deferred:", err.Error())
	}
}()
```

The goroutine is mandatory. Blocking the JS callback deadlocks the Go scheduler, because the fetch can only resolve once control returns to the event loop.

## Per-frame animation

`dom.Frame(func(t float64) bool)` runs once per paint until it returns false, and is released with the scope like a listener. Set width, transform or text directly from it; do not write a signal per frame, because an effect is a repaint and sixty repaints a second of a list is the cost this framework has no virtual DOM to absorb. The bar on `/lab` is eleven lines and moves the repaint counter by zero.

## Why not `templ Mount()`

A `templ` block compiles to a function that writes HTML. It has no way to express "run this, return nothing". Hence the convention:

> **`templ` for anything that produces markup, `func` for anything that does something.**

Both live in the same page file.
