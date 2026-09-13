package otel

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"time"

	sdk "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Navigations receives what the browser measured and the server could not.
//
//	mux.Handle("POST /api/telemetry/navigations", otel.Navigations())
//	app.Config{Telemetry: "/api/telemetry/navigations"}
//
// app.js posts a small batch on a timer and on pagehide, each row carrying the
// navigation's own trace and span ids — the same ones it put in the
// traceparent header of the fetch the server already traced. The span this
// creates takes those ids exactly, so the request span that arrived as its
// child nests underneath it, and a navigation reads top to bottom:
//
//	howl.navigate /dashboard/metrics        220 ms   mode=fragment
//	└─ GET /dashboard/metrics                18 ms
//	   └─ howl.render /dashboard/metrics      3 ms
//
// A navigation the browser rendered itself has no child at all, and that is
// the point: mode=wasm is the framework's fastest path and the only evidence
// it ever happened is this row.
//
// The endpoint is deliberately not an api.Define: it takes a beacon, answers
// 204, and belongs in no OpenAPI document a client should generate against.
func Navigations() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A beacon body is small by construction. The cap is what stops this
		// from being a way to make the process allocate.
		var batch struct {
			Navigations []navigation `json:"navigations"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&batch); err != nil {
			http.Error(w, "bad batch", http.StatusBadRequest)
			return
		}
		now := time.Now()
		for _, nav := range batch.Navigations {
			nav.record(r.Context(), now)
		}
		// Nothing to say, and the sender is a beacon that is not listening.
		w.WriteHeader(http.StatusNoContent)
	})
}

// navigation is one row as app.js sends it.
type navigation struct {
	Path    string  `json:"path"`
	Mode    string  `json:"mode"` // wasm | cached | fragment
	Ms      float64 `json:"ms"`
	Bytes   int     `json:"bytes"`
	TraceID string  `json:"traceId"`
	SpanID  string  `json:"spanId"`
}

// record turns the row into a span. The timings are relative — the browser's
// clock is not ours and cannot be trusted to agree — so the span is placed by
// its duration ending now, which is within a batch interval of the truth and
// never in the future.
func (n navigation) record(ctx context.Context, now time.Time) {
	if n.Path == "" || n.Ms < 0 || n.Ms > float64(time.Hour/time.Millisecond) {
		return
	}
	traceID, err := trace.TraceIDFromHex(n.TraceID)
	if err != nil {
		return
	}
	spanID, err := trace.SpanIDFromHex(n.SpanID)
	if err != nil {
		return
	}
	took := time.Duration(n.Ms * float64(time.Millisecond))
	// The browser's span id, claimed rather than generated: see clientIDs.
	ctx = withSpanID(ctx, spanID)
	ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	}))
	_, span := sdk.Tracer(instrumentation).Start(ctx, "howl.navigate "+n.Path,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithTimestamp(now.Add(-took)),
		trace.WithAttributes(
			attribute.String("howl.route", n.Path),
			attribute.String("howl.mode", n.Mode),
			attribute.Int("howl.bytes", n.Bytes),
			attribute.Bool("howl.client_measured", true),
		))
	span.End(trace.WithTimestamp(now))
}

// ---------------------------------------------------------------------------
// Claiming the browser's span id
//
// A span's id is the SDK's to generate, and for every span but this one that
// is right. This one already has an id: the browser minted it, put it in a
// traceparent header, and the server recorded a request span as its child
// before this row was ever sent. Generating a new one would leave that request
// parented to a span that never arrives — the trace would hold both halves of
// the navigation and show neither inside the other.
//
// So the provider gets an ID generator that answers with the id on the
// context when there is one, and delegates otherwise. It is consulted for
// every span in the process; only the ones started by record carry the key.
// ---------------------------------------------------------------------------

type spanIDKey struct{}

func withSpanID(ctx context.Context, id trace.SpanID) context.Context {
	return context.WithValue(ctx, spanIDKey{}, id)
}

type clientIDs struct{ next sdktrace.IDGenerator }

func (g clientIDs) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	return g.next.NewIDs(ctx)
}

func (g clientIDs) NewSpanID(ctx context.Context, traceID trace.TraceID) trace.SpanID {
	if id, ok := ctx.Value(spanIDKey{}).(trace.SpanID); ok && id.IsValid() {
		return id
	}
	return g.next.NewSpanID(ctx, traceID)
}

// randomIDs is what clientIDs falls back to for every other span. The SDK's
// own generator is unexported, and this is the whole of its contract: ids that
// are random, non-zero and the right width.
type randomIDs struct{}

func (randomIDs) NewIDs(context.Context) (trace.TraceID, trace.SpanID) {
	var t trace.TraceID
	var s trace.SpanID
	fill(t[:])
	fill(s[:])
	return t, s
}

func (randomIDs) NewSpanID(context.Context, trace.TraceID) trace.SpanID {
	var s trace.SpanID
	fill(s[:])
	return s
}

// fill refuses to hand back an all-zero id, which the SDK treats as invalid
// and drops the span for. crypto/rand.Read does not fail on any platform this
// runs on, but "the span vanished" is not a failure anyone would diagnose.
func fill(b []byte) {
	for {
		if _, err := rand.Read(b); err != nil {
			continue
		}
		for _, x := range b {
			if x != 0 {
				return
			}
		}
	}
}

const instrumentation = "github.com/mirairoad/howl-go/otel"
