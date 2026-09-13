// Package observe is the framework's vocabulary for "something is happening",
// with nothing behind it.
//
// Every layer already has one place a request or an operation passes through —
// App.render, the endpoint pipeline, db.Service, a middleware in Use — and
// those places call Start. What a span becomes is decided once, at startup, by
// whatever SetDefault was handed: the otel module's tracer, a test recorder,
// or nothing at all. The zero state is a no-op that allocates nothing, so a
// program that never heard of tracing pays a nil check per operation.
//
// This is the only thing core/ knows about telemetry. The OpenTelemetry SDK
// is some twenty modules and lives in the otel/ module, which implements
// Tracer; core/'s go.mod keeps its single dependency.
package observe

import "context"

// Kind is the attribute the framework sets on every span it opens — "render",
// "api", "db", "middleware" — so a tracer can turn a span into the right metric
// on End without parsing its name.
const Kind = "howl.kind"

// Span is one unit of work. Set adds an attribute (string, bool, int, int64,
// float64 and time.Duration are the values a tracer must accept), Event marks
// an instant inside it, and End closes it, recording err when there is one.
// Methods on the zero Span are safe; nothing checks for nil.
type Span interface {
	Set(key string, value any)
	Event(name string)
	End(err error)
}

// Tracer opens spans. Start returns a context carrying the new span, so work
// done with that context nests under it. Current returns the span already on
// ctx — a request span another library opened, typically — or a no-op.
type Tracer interface {
	Start(ctx context.Context, name string) (context.Context, Span)
	Current(ctx context.Context) Span
}

var def Tracer = Noop{}

// SetDefault installs the process tracer, the way slog.SetDefault does. Call
// it once, before Listen; it is startup state, not per-request state.
func SetDefault(t Tracer) {
	if t == nil {
		t = Noop{}
	}
	def = t
}

// Default is the process tracer, or Noop.
func Default() Tracer { return def }

type ctxKey struct{}

// With returns a context whose spans open on t rather than the default. Tests
// use it to record without touching process state; an application can use it
// to trace one request differently.
func With(ctx context.Context, t Tracer) context.Context {
	return context.WithValue(ctx, ctxKey{}, t)
}

func tracer(ctx context.Context) Tracer {
	if t, ok := ctx.Value(ctxKey{}).(Tracer); ok && t != nil {
		return t
	}
	return def
}

// Start opens a span on the context's tracer, or the default.
func Start(ctx context.Context, name string) (context.Context, Span) {
	return tracer(ctx).Start(ctx, name)
}

// Current is the span already on ctx, or a no-op.
func Current(ctx context.Context) Span {
	return tracer(ctx).Current(ctx)
}

// Noop is the tracer nobody installed.
type Noop struct{}

func (Noop) Start(ctx context.Context, _ string) (context.Context, Span) { return ctx, noop{} }
func (Noop) Current(context.Context) Span                                { return noop{} }

type noop struct{}

func (noop) Set(string, any) {}
func (noop) Event(string)    {}
func (noop) End(error)       {}
