// Package state carries typed per-request values on the context.
//
// howl's TypeScript side has `ctx.state`, a bag declared once and typed
// everywhere. Go already has the bag — context.Context — and only lacks the
// typing, because the usual `context.WithValue(ctx, myKey{}, v)` dance needs a
// key type, an exported getter and a type assertion for every value.
//
// Generics remove all three. The key IS the type:
//
//	type User struct{ ID string }
//
//	ctx = state.With(ctx, User{ID: "u_1"})   // in middleware
//	u := state.Get[User](ctx)                // in a page, typed
//
// One value per type. Two things of the same underlying type that mean
// different things want different named types — which is what you would want
// anyway.
package state

import "context"

// key is generic, so key[User]{} and key[Session]{} are distinct types and
// therefore distinct, uncollidable context keys.
type key[T any] struct{}

// With returns a context carrying v, replacing any previous value of type T.
func With[T any](ctx context.Context, v T) context.Context {
	return context.WithValue(ctx, key[T]{}, v)
}

// From returns the value of type T and whether it was there.
func From[T any](ctx context.Context) (T, bool) {
	v, ok := ctx.Value(key[T]{}).(T)
	return v, ok
}

// Get returns the value of type T, or its zero value. Templates read state
// this way: a missing user renders as a logged-out page rather than a panic.
func Get[T any](ctx context.Context) T {
	v, _ := From[T](ctx)
	return v
}
