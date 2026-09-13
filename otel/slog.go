package otel

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// SlogHandler adds trace_id and span_id to every record logged with a context
// that carries a span — mw.LogWith's "http" line, an endpoint's slog.InfoContext,
// anything that passed r.Context() along. Two keys, and a log line in the
// backend joins the trace it belongs to.
//
//	console.Setup(console.Options{})
//	slog.SetDefault(slog.New(otel.SlogHandler(slog.Default().Handler())))
//
// Records logged with context.Background() are passed through untouched.
// Exporting the records themselves over OTLP is the application's decision;
// this keeps them wherever they already go.
func SlogHandler(next slog.Handler) slog.Handler { return &handler{next: next} }

type handler struct{ next slog.Handler }

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.next.Handle(ctx, r)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handler{next: h.next.WithAttrs(attrs)}
}

func (h *handler) WithGroup(name string) slog.Handler { return &handler{next: h.next.WithGroup(name)} }
