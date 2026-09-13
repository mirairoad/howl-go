package observe

import (
	"context"
	"errors"
	"testing"
)

// The zero state costs nothing and panics on nothing: a framework that calls
// Start on every request must be able to do so in a program that never
// installed a tracer.
func TestNoopIsSafeEverywhere(t *testing.T) {
	SetDefault(nil)
	ctx, span := Start(context.Background(), "x")
	span.Set("k", 1)
	span.Event("e")
	span.End(errors.New("boom"))
	Current(ctx).End(nil)
	if _, ok := Default().(Noop); !ok {
		t.Fatalf("nil installs Noop, got %T", Default())
	}
}

// A context tracer beats the default, and spans nest through the context —
// which is the whole reason Start returns one.
func TestContextTracerAndNesting(t *testing.T) {
	rec := &Recorder{}
	ctx := With(context.Background(), rec)
	ctx, outer := Start(ctx, "request")
	Current(ctx).Set("http.route", "/x")
	ctx2, inner := Start(ctx, "db.get users")
	inner.Set(Kind, "db")
	inner.End(nil)
	outer.End(errors.New("late"))

	spans := rec.Spans()
	if len(spans) != 2 || spans[0].Name != "request" || spans[1].Name != "db.get users" {
		t.Fatalf("spans %+v", spans)
	}
	if spans[1].Parent != spans[0] {
		t.Error("the inner span did not nest under the outer one")
	}
	if spans[0].Attrs["http.route"] != "/x" {
		t.Error("Current did not return the span on ctx")
	}
	if spans[0].Err == nil || !spans[1].Ended {
		t.Error("End did not record")
	}
	if Current(ctx2) != spans[1] {
		t.Error("Current on the inner context is not the inner span")
	}
}
