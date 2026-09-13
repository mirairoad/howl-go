// The otel module over the real application: the request span is named by the
// route the framework matched, the render and the endpoint nest under it, and
// a fragment is marked as one. No browser here — the spans are the server's,
// and an in-memory exporter sees exactly what a collector would.
package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mirairoad/howl-go/examples/toy_app/boot"
	"github.com/mirairoad/howl-go/otel"
	sdk "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestTracesNameTheRouteAndNestTheWork(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	shutdown, err := otel.Setup(context.Background(), otel.WithServiceName("e2e"), otel.WithSpanExporter(exp), otel.WithoutMetrics())
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(context.Background())

	_, h := boot.New()
	srv := httptest.NewServer(otel.HTTP()(h))
	defer srv.Close()

	// A fragment navigation, as app.js makes it.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/dashboard/metrics", nil)
	req.Header.Set("X-Partial", "1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get(otel.HeaderTraceID) == "" {
		t.Fatalf("fragment: %d, X-Trace-Id %q", res.StatusCode, res.Header.Get(otel.HeaderTraceID))
	}
	// An endpoint.
	if res, err := http.Get(srv.URL + "/api/metrics"); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("endpoint: %v %v", res, err)
	}
	if err := sdk.GetTracerProvider().(*sdktrace.TracerProvider).ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}

	spans := map[string]tracetest.SpanStub{}
	for _, s := range exp.GetSpans() {
		spans[s.Name] = s
	}
	page, ok := spans["GET /dashboard/metrics"]
	if !ok {
		t.Fatalf("no request span named by the page route; have %v", keys(spans))
	}
	if !has(page, "howl.partial", "true") {
		t.Errorf("the fragment request is not marked: %v", page.Attributes)
	}
	render, ok := spans["howl.render /dashboard/metrics"]
	if !ok || render.Parent.SpanID() != page.SpanContext.SpanID() {
		t.Errorf("render span missing or not under its request; have %v", keys(spans))
	}
	if !has(render, "howl.mode", "fragment") || !has(render, "howl.client", "true") {
		t.Errorf("render attributes: %v", render.Attributes)
	}
	api, ok := spans["GET /api/metrics"]
	if !ok {
		t.Fatalf("no request span named by the endpoint route; have %v", keys(spans))
	}
	call, ok := spans["api Metrics"]
	if !ok || call.Parent.SpanID() != api.SpanContext.SpanID() {
		t.Errorf("endpoint span missing or not under its request; have %v", keys(spans))
	}
}

func has(s tracetest.SpanStub, key, value string) bool {
	for _, kv := range s.Attributes {
		if kv.Key == attribute.Key(key) && kv.Value.Emit() == value {
			return true
		}
	}
	return false
}

func keys(m map[string]tracetest.SpanStub) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Writes, not just reads. A server-first application's main write path is a
// form post or a fragment endpoint, and the toy app has one of each kind:
// a generated endpoint (POST /api/todos/sync), two hand-written mux handlers
// (POST /api/todos, DELETE /api/todos/{id}), and a page path reached with a
// method no page route owns.
func TestEveryMethodIsTracedAndNamed(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	shutdown, err := otel.Setup(context.Background(), otel.WithServiceName("e2e"), otel.WithSpanExporter(exp), otel.WithoutMetrics())
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(context.Background())

	a, mux := boot.New()
	// Routes wraps the mux directly; Use — and the request span with it — goes
	// around the outside, which is what a.Wrap does.
	srv := httptest.NewServer(a.Wrap(otel.HTTP()(otel.Routes(mux))))
	defer srv.Close()

	do := func(method, path, contentType, body string) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	do(http.MethodPost, "/api/todos/sync", "application/json", `{"ops":[]}`)
	do(http.MethodPost, "/api/todos", "application/x-www-form-urlencoded", "text=traced")
	do(http.MethodDelete, "/api/todos/1", "", "")
	do(http.MethodPost, "/dashboard", "text/plain", "") // no page route owns POST

	if err := sdk.GetTracerProvider().(*sdktrace.TracerProvider).ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := map[string]tracetest.SpanStub{}
	for _, s := range exp.GetSpans() {
		spans[s.Name] = s
	}
	for _, want := range []string{
		"POST /api/todos/sync",   // generated endpoint
		"api Sync Todos",         // its endpoint span, under the request
		"POST /api/todos",        // hand-written handler
		"DELETE /api/todos/{id}", // hand-written, with a path parameter
		"POST /",                 // the catch-all: a page path, a method it does not serve
		"howl.render 404",        // and the render that answered it, named for what it is
	} {
		if _, ok := spans[want]; !ok {
			t.Errorf("no span %q; have %v", want, keys(spans))
		}
	}
	if call, ok := spans["api Sync Todos"]; ok {
		if req := spans["POST /api/todos/sync"]; call.Parent.SpanID() != req.SpanContext.SpanID() {
			t.Error("the endpoint span is not under its request")
		}
	}
	// A write went through the store: db spans are not a read-only affair.
	if _, ok := spans["db.create todos"]; !ok {
		t.Logf("no db.create span — the toy app's fragment API writes to an in-memory store, not db.Service")
	}
}
