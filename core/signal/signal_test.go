package signal

import "testing"

func TestEffectTracksReads(t *testing.T) {
	a := Of(1)
	runs := 0
	stop := Effect(func() { a.Get(); runs++ })
	a.Set(2)
	a.Set(2) // equal write: no wake
	if runs != 2 {
		t.Fatalf("runs = %d, want 2", runs)
	}
	stop()
	a.Set(3)
	if runs != 2 {
		t.Fatalf("stopped effect ran: runs = %d", runs)
	}
}

// Two writes in one handler used to mean two repaints. Batched they cost one,
// and the effect sees both values current.
func TestBatchRunsDependentsOnce(t *testing.T) {
	a, b := Of(0), Of(0)
	runs, seen := 0, 0
	Effect(func() { seen = a.Get() + b.Get(); runs++ })
	Batch(func() {
		a.Set(1)
		b.Set(2)
	})
	if runs != 2 || seen != 3 {
		t.Fatalf("runs = %d seen = %d, want 2 and 3", runs, seen)
	}
}

// The diamond: an effect reads a signal and a value derived from it. Creation
// order is flush order, so the derived value recomputes before the reader
// runs, and the reader runs once with both inputs current.
func TestDiamondRunsOnce(t *testing.T) {
	a := Of(1)
	double := DeriveEq(func() int { return a.Get() * 2 })
	runs := 0
	var got [2]int
	Effect(func() { got = [2]int{a.Get(), double.Get()}; runs++ })
	a.Set(5)
	if runs != 2 {
		t.Fatalf("runs = %d, want 2 (diamond ran twice)", runs)
	}
	if got != [2]int{5, 10} {
		t.Fatalf("effect saw %v, want [5 10]", got)
	}
}

// A Set made from inside an effect must not recurse into the running effect;
// it queues and runs after, once.
func TestSetInsideEffectQueues(t *testing.T) {
	a, b := Of(0), Of(0)
	Effect(func() {
		if a.Get() > 0 {
			b.Set(a.Peek() * 10)
		}
	})
	seen := 0
	Effect(func() { seen = b.Get() })
	a.Set(3)
	if seen != 30 {
		t.Fatalf("seen = %d, want 30", seen)
	}
}

func TestScopeReleasesEverything(t *testing.T) {
	a := Of(0)
	runs, watched, cleaned := 0, 0, false
	dispose := Scope(func() {
		Effect(func() { a.Get(); runs++ })
		Watch(a.Get, func(_, _ int) { watched++ })
		OnCleanup(func() { cleaned = true })
	})
	a.Set(1)
	if runs != 2 || watched != 1 {
		t.Fatalf("before dispose: runs = %d watched = %d", runs, watched)
	}
	dispose()
	dispose() // twice is safe
	a.Set(2)
	if runs != 2 || watched != 1 || !cleaned {
		t.Fatalf("after dispose: runs = %d watched = %d cleaned = %v", runs, watched, cleaned)
	}
}

// A registration made outside any scope is not swept by a later one.
func TestScopeDoesNotCaptureOutsiders(t *testing.T) {
	a := Of(0)
	runs := 0
	Effect(func() { a.Get(); runs++ })
	dispose := Scope(func() {})
	dispose()
	a.Set(1)
	if runs != 2 {
		t.Fatalf("outsider effect was released: runs = %d", runs)
	}
}

func TestWatchSkipsInitialAndEqualWrites(t *testing.T) {
	a := Of("x")
	var log []string
	Watch(a.Get, func(now, before string) { log = append(log, before+"->"+now) })
	a.Set("x")
	a.Set("y")
	if len(log) != 1 || log[0] != "x->y" {
		t.Fatalf("log = %v", log)
	}
}

func TestConditionalBranchDetaches(t *testing.T) {
	gate, a := Of(true), Of(0)
	runs := 0
	Effect(func() {
		runs++
		if gate.Get() {
			a.Get()
		}
	})
	gate.Set(false)
	a.Set(1) // no longer read: must not wake
	if runs != 2 {
		t.Fatalf("runs = %d, want 2", runs)
	}
}
