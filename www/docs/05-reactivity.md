# Reactivity

`core/signal` is fine-grained reactivity in the shape Vue's `ref`/`computed`/`watch` and Preact's signals popularised. Twelve names, and that is the whole package.

```go
Todos     = signal.WithEq([]Todo(nil), sameTodos)   // signal
TodoCount = signal.DeriveEq(func() int { … })       // computed
signal.Effect(repaint)                              // auto-tracked effect
signal.Watch(TodoCount.Get, func(now, before int) { … })
signal.Batch(func() { a.Set(1); b.Set(2) })         // one repaint, not two
```

| | |
|---|---|
| construct | `Of` (comparable), `New` (anything), `WithEq` (your equality) |
| read and write | `Get`, `Peek`, `Set`, `Update` |
| derive | `Derive`, `DeriveEq` |
| react | `Effect`, `Watch` |
| control | `Batch`, `Untrack` |
| lifetime | `Scope`, `OnCleanup` |

## There is no dependency array

React needs `useEffect(fn, [a, b, c])` because **JavaScript cannot observe reads**. React has no way to know what your function looked at, so you declare it by hand — and a wrong declaration is the classic stale-closure bug.

Here reads *are* observed. While an effect runs it is installed as the current computation, and every `Get()` registers the dependency. So the list cannot be wrong, because there isn't one:

```go
signal.Effect(func() {
	title := store.Article.Get().Title   // both become dependencies
	n := store.TodoCount.Get()           // simply by being read
})
```

That is also how you watch several values at once. There is no `WatchAny`; one `Effect` reading several signals is the form.

## Watching a transition

`Watch` fires only when the value changes, and gives you both sides:

```go
signal.Watch(
	func() string { return store.Article.Get().Title },
	func(now, before string) { … },
)
```

The callback runs untracked, so signals it reads do not silently become dependencies of the watcher.

## Writes are batched

A `Set` does not run its dependents on the spot. It queues them, and the queue drains when the outermost write returns. Three consequences, each of which used to be a bug:

- Two writes in one handler cost one repaint. `Batch` widens that window over any number of writes.
- A `Set` made inside an effect does not recurse into the effect that is running; it queues, and runs after.
- The diamond — an effect reading both a signal and a value derived from it — runs once per change, with the derived value already recomputed. Creation order is flush order, and a derived value is necessarily created before anything that reads it.

## Equality is what makes it fine-grained

| constructor | use |
|---|---|
| `signal.Of[T comparable]` | skips notification when the value is unchanged |
| `signal.WithEq` | slices, maps, anything `==` will not compile on |
| `signal.DeriveEq` | an unchanged recompute stops propagating |

A store holding a slice needs `WithEq`, or re-hydrating identical data wakes every dependent for nothing.

## Release

Re-running an effect detaches all of its previous dependencies first. Without that, a conditional branch that stops reading a signal keeps a permanent subscription to it.

Releasing the effect itself is the scope's job. A page's `Mount` runs inside one, and every `Effect`, `Watch` and `Derive` created there — and every `dom` listener — is released when the page leaves. The stop func is still returned for code outside a scope, where it is the only handle. `signal.Scope(fn)` opens one for a lifetime that is not a page's, and `signal.OnCleanup(fn)` attaches teardown to whichever scope is open. See [Lifecycle](/docs/lifecycle).

## Server safety

Signals are package-level, which would be a data race if server request handlers wrote them. They must not. The pattern used in the example app is to mirror into signals only for the browser's store instance:

```go
func (s *Store) publish() {
	if s == client {   // the server's Store is a different pointer
		Todos.Set(s.List())
	}
}
```

`howl check` reports server code that imports `core/signal` at all, because the mistake is invisible until it is concurrent: one request writes, another renders, and the bug arrives as one user seeing another's data under load.

## Which mechanism

Signals are the heaviest of the three ways to make something change on screen, and reaching for them by default is the most common mess. Decide by what the state *is*:

| the state is | use | cost |
|---|---|---|
| nothing — a form that posts and re-renders | `<form method="post">` and an endpoint | 0 |
| local to one widget, JS-shaped (dropdown, filter, sort) | an island | a few lines of vanilla |
| domain state the server also owns, mutated by the user | a `.client` page + a store + signals | the wasm binary, 1.71 MB gzipped |

The third one earns its cost when the *same rules* have to run on both sides, and you would otherwise write them twice.

## The shape

A store is two files because it is two rules:

```
client/store/todos.go          domain — compiles for the server AND for GOOS=js
client/store/todos_client.go   signals — package-level, therefore browser-only
```

The domain half holds the types, the mutation methods, `Snapshot` (the wire format), `Op` (one mutation) and the `WithTodos`/`TodosFrom` context pair the server render reads. Nothing server-shaped may enter it — `net/http`, `database/sql`, `os` — because pages import it and pages compile to wasm. `howl check` reports it if something does.

Then three wires, in the order they run:

1. **SSR** — `Config.Data` puts the data on the context, the page renders from `store.TodosFrom(ctx)`. This is the first paint, and it needs no JavaScript.
2. **Hydrate** — the page serialises the snapshot it rendered from with `@templ.JSONScript("todos", store.SnapshotFrom(ctx))`; `Mount` reads it back with `dom.Embedded` and calls `Restore`, which publishes to the signal. No request, and the browser store starts with exactly what is on screen.
3. **Local** — a mutation calls `store.Commit(op, send)`: `Apply` runs now, publishes, wakes the effect, which re-renders the same templ component the server used; `send` tells the server from a goroutine. One sender goroutine sends in commit order. If the server refuses (a 4xx), that one op is rolled back — the confirmed snapshot plus the ops still waiting — and `store.Rejected` carries the reason for the page to show. If the server is unreachable nothing is dropped: `store.Offline` goes true, `store.Queued` counts the wait, and the sender retries with backoff until the op lands.

The one rule that fails silently: **read through the signal, not the store**. `store.Todos.Get()` registers the effect as a dependent; `store.TodosClient().List()` returns the same data, subscribes to nothing, and the page simply stops updating.

Both files, and a page already wired to them, come out of `howl_scaffold` — `kind: "store"`, then `kind: "page"` with `client: true, store: "todos"`. See [Lifecycle](/docs/lifecycle) for the `Mount` it writes.

## Hidden markup still costs

Build an overlay from a checkbox and a sibling selector — no JavaScript, keyboard-operable, nothing to re-hydrate after a navigation. Then gate whatever fills it.

There is no component tree here, so a script finds its work by asking whether the markup is present: `if (qs("[data-rows]")) refresh()`. A closed panel is `display:none`, and `display:none` markup **is still present**. It keeps answering yes, so anything on a timer rebuilds it forever, at full cost, for nobody. A virtual DOM would have said the panel was not mounted; nothing here will.

```js
const open = () => { const t = document.getElementById("panel"); return !t || t.checked; };
if (open()) host.replaceChildren(...rows.map(render));
document.addEventListener("change", (e) => {
  if (e.target.id === "panel" && e.target.checked) draw();   // never open onto stale content
});
```

`!t || t.checked` is deliberate: a page with no panel is one where that markup *is* the page, and must always draw.
