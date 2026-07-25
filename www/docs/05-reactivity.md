# Reactivity

`core/signal` is fine-grained reactivity in the shape Vue's `ref`/`computed`/`watch` and Preact's signals popularised.

```go
Todos     = signal.WithEq([]Todo(nil), sameTodos)   // signal
TodoCount = signal.DeriveEq(func() int { … })       // computed
stop      = signal.Effect(repaint)                  // auto-tracked effect
```

## There is no dependency array

React needs `useEffect(fn, [a, b, c])` because **JavaScript cannot observe reads**. React has no way to know what your function looked at, so you declare it by hand — and a wrong declaration is the classic stale-closure bug.

Here reads *are* observed. While an effect runs it is installed as the current computation, and every `Get()` registers the dependency. So the list cannot be wrong, because there isn't one:

```go
signal.Effect(func() {
	title := store.Article.Get().Title   // both become dependencies
	n := store.TodoCount.Get()           // simply by being read
})
```

That is also how you watch several values at once. `WatchAny(cb, srcs...)` exists if you want them named explicitly, but `Effect` is the idiomatic form.

## Watching a transition

`Watch` fires only when the value changes, and gives you both sides:

```go
stop := signal.Watch(
	func() string { return store.Article.Get().Title },
	func(now, before string) { … },
)
```

The callback runs untracked, so signals it reads do not silently become dependencies of the watcher.

## Equality is what makes it fine-grained

| constructor | use |
|---|---|
| `signal.Of[T comparable]` | skips notification when the value is unchanged |
| `signal.WithEq` | slices, maps, anything `==` will not compile on |
| `signal.DeriveEq` | an unchanged recompute stops propagating |

A store holding a slice needs `WithEq`, or re-hydrating identical data wakes every dependent for nothing.

## Detaching

Re-running an effect detaches all of its previous dependencies first. Without that, a conditional branch that stops reading a signal keeps a permanent subscription to it.

Always pair `Effect`/`Watch` in `Mount` with their `stop` in `Unmount`. See [Lifecycle](/docs/lifecycle).

## Server safety

Signals are package-level, which would be a data race if server request handlers wrote them. They must not. The pattern used in the example app is to mirror into signals only for the browser's store instance:

```go
func (s *Store) publish() {
	if s == client {   // the server's Store is a different pointer
		Todos.Set(s.List())
	}
}
```
