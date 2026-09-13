package otel

import (
	"context"
	"net/http"
	"strings"

	"github.com/mirairoad/howl-go/core/mw"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// HeaderTraceID is set on every traced response, so the trace behind a
// screenshot of a network tab can be found.
const HeaderTraceID = "X-Trace-Id"

type routeKey struct{}

type routeInfo struct{ method string }

// HTTP is the request span, as mw.Middleware: otelhttp's server
// instrumentation, which is also where http.server.request.duration comes
// from. Put it first in Use so every other middleware, the render and the
// endpoint are inside it.
//
// A span opens named by the method alone — the mux has not matched yet — and
// is renamed "GET /orders/{id}" the moment the framework learns the route, in
// App.render or the endpoint pipeline. A request that reaches neither (a
// static file, a 404) keeps the bare method, which is what the conventions say
// to do when there is no route.
//
// The dev server's own endpoints under /_howl/ are not traced: the reload
// stream is one request that lasts the whole session.
func HTTP() mw.Middleware {
	return func(next http.Handler) http.Handler {
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), routeKey{}, &routeInfo{method: r.Method})
			span := trace.SpanFromContext(ctx)
			if sc := span.SpanContext(); sc.IsValid() {
				w.Header().Set(HeaderTraceID, sc.TraceID().String())
			}
			// A fragment and a document for the same route are different
			// work; without this the SPA path is invisible — the same reason
			// mw.LogWith logs it.
			if r.Header.Get("X-Partial") == "1" {
				span.SetAttributes(attribute.Bool("howl.partial", true))
			}
			next.ServeHTTP(w, r.WithContext(ctx))
			// mw.RequestID runs inside this middleware, so its id is read from
			// the response it set rather than the context it did not have yet.
			if id := w.Header().Get(mw.HeaderRequestID); id != "" {
				span.SetAttributes(attribute.String("howl.request_id", id))
			}
		})
		return otelhttp.NewHandler(inner, "http",
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method }),
			otelhttp.WithFilter(func(r *http.Request) bool { return !strings.HasPrefix(r.URL.Path, "/_howl/") }),
		)
	}
}

// RecordError is for api.Config.OnError: the failure is recorded on the
// request span, which is the one a 4xx or 5xx is filed under.
func RecordError(r *http.Request, err error) {
	if err == nil {
		return
	}
	span := trace.SpanFromContext(r.Context())
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
