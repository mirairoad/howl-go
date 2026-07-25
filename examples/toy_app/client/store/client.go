package store

import "github.com/mirairoad/howl-go/core/signal"

// The browser's store, exposed reactively.
//
// On the server a Store is per-process and read through the request context —
// two requests must never see each other's state. In the browser there is one
// user and one tab, so package-level signals are the right shape: any Mount,
// Unmount or event handler can read them, and anything derived from them
// updates itself.
var client = New()

// Client is the browser-side store. Mutating it publishes to the signals below.
func Client() *Store { return client }

var (
	// Todos is the reactive list. Slices are not comparable, so it carries an
	// explicit equality test — without one, a re-hydrate that changed nothing
	// would still wake every dependent.
	Todos = signal.WithEq([]Todo(nil), sameTodos)

	// TodoCount is derived. DeriveEq means a mutation that leaves the count
	// alone — editing an item's text, say — does not wake anything that only
	// reads the count.
	TodoCount = signal.DeriveEq(func() int { return len(Todos.Get()) })
)

func sameTodos(a, b []Todo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// publish mirrors a mutation into the signals. Only the browser's instance does
// this: the server's Store is a different pointer, so concurrent requests never
// write these package-level variables.
func (s *Store) publish() {
	if s == client {
		Todos.Set(s.List())
	}
}
