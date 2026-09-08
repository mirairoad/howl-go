package store

import (
	"sync"
	"time"

	"github.com/mirairoad/howl-go/core/api"
	"github.com/mirairoad/howl-go/core/signal"
)

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

	// Offline is true while the server cannot be reached. Queued is how many
	// ops are applied locally and not yet confirmed — the number that will be
	// sent, in order, when it can be reached again.
	Offline = signal.Of(false)
	Queued  = signal.Of(0)
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
// Optimistic commits, the queue behind them, and taking one back.
//
// A mutation is applied locally first — the user never waits — and the server
// is told afterwards, in order, by one sender. The store keeps the last
// snapshot the server agreed to and every op applied since. Three outcomes:
//
//   - confirmed: the op folds into the confirmed snapshot.
//   - refused (a 4xx): the op is dropped and the visible state rebuilt from
//     the confirmed snapshot plus the ops still waiting, so a later op that
//     the server will still see is kept, and — ops being deterministic — an
//     "add" after a refused "add" ends up with the id the server gives it.
//   - unreachable (a network failure, a 5xx): nothing is dropped. Offline goes
//     true, the sender backs off and retries the same op, and everything
//     committed meanwhile waits its turn behind it. Order is the whole point
//     of one sender: two goroutines racing would let a later op land first.
// ---------------------------------------------------------------------------

var (
	confirmed Snapshot   // what the server last agreed to
	pending   []inflight // applied locally since, oldest first; pending[0] is being sent
	seq       int

	kick   = make(chan struct{}, 1)
	sender sync.Once
)

type inflight struct {
	seq  int
	op   Op
	send func(Op) error
}

// Hydrate installs the server's snapshot as the confirmed state. Ops still
// waiting to be sent are replayed on top, so navigating away and back while
// offline does not make queued changes vanish from the screen.
func Hydrate(sn Snapshot) {
	confirmed = sn
	view := sn
	for _, p := range pending {
		view = replay(view, p.op)
	}
	client.Restore(view)
}

// Commit applies op now and queues it. send is called from the sender
// goroutine, in commit order, and returns the server's verdict; a refusal
// rolls the op back and sets Rejected, anything else is retried. The page
// passes the network call in, because the store cannot import the generated
// client without a cycle — the client imports the store's types.
func Commit(op Op, send func(Op) error) {
	seq++
	pending = append(pending, inflight{seq: seq, op: op, send: send})
	client.Apply(op)
	Queued.Set(len(pending))
	sender.Do(func() { go drain() })
	select {
	case kick <- struct{}{}:
	default: // the sender is already awake
	}
}

func drain() {
	backoff := time.Second
	for range kick {
		for len(pending) > 0 {
			e := pending[0]
			err := e.send(e.op)
			switch {
			case err == nil:
				confirm(e)
				backoff = time.Second
			case api.Refused(err):
				reject(e, err)
				backoff = time.Second
			default:
				Offline.Set(true)
				time.Sleep(backoff)
				if backoff < 5*time.Second {
					backoff *= 2
				}
				// pending[0] is still e: try it again.
			}
		}
	}
}

func confirm(e inflight) {
	if !drop(e.seq) {
		return
	}
	confirmed = replay(confirmed, e.op)
	signal.Batch(func() {
		Queued.Set(len(pending))
		Offline.Set(false)
		if !Rejected.Peek().Empty() {
			Rejected.Set(Rejection{})
		}
	})
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
		Queued.Set(len(pending))
		Offline.Set(false)
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
