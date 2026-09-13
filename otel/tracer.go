package otel

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mirairoad/howl-go/core/observe"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// tracer is core/observe's Tracer over the SDK. Besides opening spans it is
// where the framework's metrics come from: a span the framework opened says
// what it is in howl.kind, and End turns its duration into the histogram for
// that kind. No second set of hooks, and the trace and the metric cannot
// disagree about what happened.
type tracer struct {
	t trace.Tracer
	m *metrics
}

type metrics struct {
	render  metric.Float64Histogram
	api     metric.Float64Histogram
	apiErrs metric.Int64Counter
	db      metric.Float64Histogram
	storage metric.Float64Histogram
	cache   metric.Int64Counter
	limited metric.Int64Counter
}

func newTracer(t trace.Tracer, meter metric.Meter) (*tracer, error) {
	m := &metrics{}
	var err error
	seconds := metric.WithUnit("s")
	if m.render, err = meter.Float64Histogram("howl.render.duration", seconds,
		metric.WithDescription("Time to render a page and its layouts, by route and mode.")); err != nil {
		return nil, fmt.Errorf("otel: %w", err)
	}
	if m.api, err = meter.Float64Histogram("howl.api.duration", seconds,
		metric.WithDescription("Time in an endpoint from decode to the handler's return.")); err != nil {
		return nil, fmt.Errorf("otel: %w", err)
	}
	if m.apiErrs, err = meter.Int64Counter("howl.api.errors",
		metric.WithDescription("Endpoint calls that failed, by endpoint and the stage they failed at.")); err != nil {
		return nil, fmt.Errorf("otel: %w", err)
	}
	if m.db, err = meter.Float64Histogram("db.client.operation.duration", seconds,
		metric.WithDescription("Time in one document operation, cache and decoding included.")); err != nil {
		return nil, fmt.Errorf("otel: %w", err)
	}
	// The same operation minus everything the service does around it. The gap
	// between the two is validation, key building, JSON and waiting on another
	// caller's query — which is where a "slow database" usually turns out not
	// to be the database.
	if m.storage, err = meter.Float64Histogram("howl.db.storage.duration", seconds,
		metric.WithDescription("Time inside the backend for one document operation.")); err != nil {
		return nil, fmt.Errorf("otel: %w", err)
	}
	if m.cache, err = meter.Int64Counter("howl.cache",
		metric.WithDescription("Cache decisions — responses and documents — by result.")); err != nil {
		return nil, fmt.Errorf("otel: %w", err)
	}
	if m.limited, err = meter.Int64Counter("howl.ratelimit.refused",
		metric.WithDescription("Requests refused by a rate limit.")); err != nil {
		return nil, fmt.Errorf("otel: %w", err)
	}
	return &tracer{t: t, m: m}, nil
}

func (x *tracer) Start(ctx context.Context, name string) (context.Context, observe.Span) {
	ctx, s := x.t.Start(ctx, name)
	return ctx, &span{s: s, m: x.m, ctx: ctx, start: time.Now(), attrs: map[string]string{}}
}

// Current wraps whatever span is on ctx — otelhttp's request span, usually.
// It is not this wrapper's to end, so End on it records the error and stops.
func (x *tracer) Current(ctx context.Context) observe.Span {
	return &span{s: trace.SpanFromContext(ctx), m: x.m, ctx: ctx, attrs: map[string]string{}, borrowed: true}
}

type span struct {
	s        trace.Span
	m        *metrics
	ctx      context.Context
	start    time.Time
	attrs    map[string]string // the attributes a metric is labelled by
	counts   map[string]int64  // the counts a metric is incremented by
	borrowed bool
}

// The attributes worth keeping as metric labels. Anything else goes on the
// span only: a label with unbounded values (an id, a byte count, a row count)
// is how a metrics backend runs out of memory.
var labelled = map[string]bool{
	observe.Kind: true, "howl.route": true, "howl.mode": true, "howl.endpoint": true, "howl.api.stage": true,
	"db.system.name": true, "db.collection.name": true, "db.operation.name": true,
}

// Counts the framework reports as attributes rather than as events, because
// one operation can make many of them: GetMany of fifty ids does fifty cache
// lookups, and fifty span events is not a readable trace. They become the
// cache counter when the span ends.
var counted = map[string]string{
	"howl.cache.hits":   "hit",
	"howl.cache.misses": "miss",
}

func (x *span) Set(key string, v any) {
	x.s.SetAttributes(kv(key, v))
	if labelled[key] {
		x.attrs[key] = fmt.Sprint(v)
	}
	if _, ok := counted[key]; ok {
		if n, ok := asInt(v); ok {
			if x.counts == nil {
				x.counts = map[string]int64{}
			}
			x.counts[key] = n
		}
	}
	if key == "http.route" {
		route := fmt.Sprint(v)
		// The request span was named by method alone, because the route is
		// not known until the mux has matched. Now it is: "GET /orders/{id}",
		// as the conventions want, and the route on the request metric too.
		if ri, ok := x.ctx.Value(routeKey{}).(*routeInfo); ok {
			x.s.SetName(ri.method + " " + route)
		}
		if l, ok := otelhttp.LabelerFromContext(x.ctx); ok {
			l.Add(attribute.String("http.route", route))
		}
	}
}

func (x *span) Event(name string) {
	x.s.AddEvent(name)
	kind := x.attrs[observe.Kind]
	if kind == "" {
		kind = "http"
	}
	switch {
	case strings.HasPrefix(name, "cache."):
		x.m.cache.Add(x.ctx, 1, metric.WithAttributes(
			attribute.String("howl.kind", kind), attribute.String("result", strings.TrimPrefix(name, "cache."))))
	case name == "ratelimit.refused":
		x.m.limited.Add(x.ctx, 1)
	}
}

func (x *span) End(err error) {
	if err != nil {
		x.s.RecordError(err)
		x.s.SetStatus(codes.Error, err.Error())
	}
	if x.borrowed {
		return
	}
	x.s.End()
	x.record(time.Since(x.start).Seconds(), err)
	for key, result := range counted {
		if n := x.counts[key]; n > 0 {
			x.m.cache.Add(x.ctx, n, metric.WithAttributes(
				attribute.String("howl.kind", x.attrs[observe.Kind]),
				attribute.String("result", result),
				attribute.String("db.collection.name", x.attrs["db.collection.name"])))
		}
	}
}

func (x *span) record(seconds float64, err error) {
	label := func(keys ...string) metric.MeasurementOption {
		out := make([]attribute.KeyValue, 0, len(keys)+1)
		for _, k := range keys {
			if v, ok := x.attrs[k]; ok {
				out = append(out, attribute.String(k, v))
			}
		}
		return metric.WithAttributes(append(out, attribute.Bool("error", err != nil))...)
	}
	switch x.attrs[observe.Kind] {
	case "render":
		x.m.render.Record(x.ctx, seconds, label("howl.route", "howl.mode"))
	case "api":
		x.m.api.Record(x.ctx, seconds, label("howl.endpoint"))
		if err != nil {
			x.m.apiErrs.Add(x.ctx, 1, label("howl.endpoint", "howl.api.stage"))
		}
	case "db":
		x.m.db.Record(x.ctx, seconds, label("db.system.name", "db.collection.name", "db.operation.name"))
	case "db.storage":
		x.m.storage.Record(x.ctx, seconds, label("db.system.name", "db.collection.name", "db.operation.name"))
	}
}

// asInt is the counted attributes' value, whatever width the caller used.
func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	default:
		return 0, false
	}
}

func kv(key string, v any) attribute.KeyValue {
	switch x := v.(type) {
	case string:
		return attribute.String(key, x)
	case bool:
		return attribute.Bool(key, x)
	case int:
		return attribute.Int(key, x)
	case int64:
		return attribute.Int64(key, x)
	case float64:
		return attribute.Float64(key, x)
	case time.Duration:
		return attribute.String(key, x.String())
	default:
		return attribute.String(key, fmt.Sprint(x))
	}
}
