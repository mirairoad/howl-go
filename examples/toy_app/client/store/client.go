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

	// Rejected is the last op the server refused, and why. Zero when there is
	// none. A page shows it; the next confirmed commit clears it.
	Rejected = signal.Of(Rejection{})
)

type Rejection struct {
	Op  Op
	Err string
}

func (r Rejection) Empty() bool { return r.Err == "" }

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

// ---------------------------------------------------------------------------
// Optimistic commits, and taking them back.
//
// A mutation is applied locally first — the user never waits — and the server
// is told afterwards. Most of the time it agrees. When it does not, the page
// is showing something that never happened, and "log a warning" is not an
// answer. So the store keeps two things: the last snapshot the server agreed
// to, and the ops applied since, in order. A refusal drops that one op and
// rebuilds the visible state from the confirmed snapshot plus the ops still in
// flight — so an op that succeeded after the failed one is kept, and because
// ops are deterministic, an "add" that came after a rejected "add" ends up
// with the id the server gave it.
// ---------------------------------------------------------------------------

var (
	confirmed Snapshot   // what the server last agreed to
	pending   []inflight // applied locally since, oldest first
	seq       int
)

type inflight struct {
	seq int
	op  Op
}

// Hydrate installs the server's snapshot as both the visible state and the
// confirmed one. Mount calls it with what dom.Embedded read from the page.
func Hydrate(sn Snapshot) {
	confirmed = sn
	pending = nil
	client.Restore(sn)
}

// Commit applies op now, then sends it. send runs in a goroutine and returns
// the server's verdict; on an error the op is rolled back and Rejected is set.
// The page passes the network call in, because the store cannot import the
// generated client without a cycle — the client imports the store's types.
func Commit(op Op, send func(Op) error) {
	seq++
	entry := inflight{seq: seq, op: op}
	pending = append(pending, entry)
	client.Apply(op)
	go func() {
		if err := send(op); err != nil {
			reject(entry, err)
			return
		}
		confirm(entry)
	}()
}

func confirm(e inflight) {
	if !drop(e.seq) {
		return
	}
	confirmed = replay(confirmed, e.op)
	if !Rejected.Peek().Empty() {
		Rejected.Set(Rejection{})
	}
}

func reject(e inflight, err error) {
	if !drop(e.seq) {
		return
	}
	sn := confirmed
	for _, p := range pending {
		sn = replay(sn, p.op)
	}
	// One repaint for the rollback and the message together.
	signal.Batch(func() {
		client.Restore(sn)
		Rejected.Set(Rejection{Op: e.op, Err: err.Error()})
	})
}

func drop(seq int) bool {
	for i, p := range pending {
		if p.seq == seq {
			pending = append(pending[:i], pending[i+1:]...)
			return true
		}
	}
	return false
}

// replay applies ops to a copy of sn. A scratch Store, so publish — which is
// guarded on the client instance — stays silent.
func replay(sn Snapshot, ops ...Op) Snapshot {
	tmp := New()
	tmp.Restore(sn)
	for _, op := range ops {
		tmp.Apply(op)
	}
	return tmp.Snapshot()
}
