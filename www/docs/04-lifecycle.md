# Lifecycle

templ has no lifecycle and cannot have one: a component renders once to a writer and is finished. There is no mount, no effect, no re-render. So "do something on first paint" is a plain Go function in the page file.

```go
func Mount()   { … }   // after this page's markup is in the DOM
func Unmount() { … }   // just before its markup is replaced
```

Both are reserved names, both optional, both run in the browser — on the cold load and again after every client-side navigation to that route.

## Touching the DOM

Through `core/dom`, not `syscall/js`:

```go
func Mount() {
	root := dom.Root()
	dom.Log("rows:", len(root.QueryAll("[data-rows] tr")))

	root.Query("[data-add]").On("click", func() {
		store.Client().Add("from a Go click handler")
	})
}
```

This matters more than it looks. A `.templ` file's generated Go is compiled for **both** targets, so importing `syscall/js` there would break the server build. `core/dom` splits the platform once — `dom_js.go` is real, `dom_stub.go` is no-ops — and the page stays neutral, needs no build tag, and compiles into the server binary as dead code that is never called.

## Fetching

Go's wasm `net/http` transport **is** the browser's Fetch API. No htmx, no JS glue, and the response decodes into ordinary structs:

```go
go func() {
	res, err := http.Get("/api/metrics")
	…
}()
```

The goroutine is mandatory. Blocking the JS callback deadlocks the Go scheduler, because the fetch can only resolve once control returns to the event loop.

## Why `Unmount` exists

Anything `Mount` registers must be released, or it outlives its DOM and keeps firing against nodes that were thrown away — and every visit adds another one.

```go
var stopEffect func()

func Mount()   { stopEffect = signal.Effect(repaint) }
func Unmount() { stopEffect(); stopEffect = nil }
```

Measured: three away-and-back navigations then one mutation logs **once**. Without `Unmount` it logs four times.

## Why not `templ Mount()`

A `templ` block compiles to a function that writes HTML. It has no way to express "run this, return nothing". Hence the convention:

> **`templ` for anything that produces markup, `func` for anything that does something.**

Both live in the same page file.
