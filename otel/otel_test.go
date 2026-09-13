package otel

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mirairoad/howl-go/core/mw"
	"github.com/mirairoad/howl-go/core/observe"
	"github.com/mirairoad/howl-go/db"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// setup installs the SDK with an in-memory exporter and a manual reader, so a
// test sees exactly the spans and metrics a collector would.
func setup(t *testing.T) (*tracetest.InMemoryExporter, *sdkmetric.ManualReader, func()) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	reader := sdkmetric.NewManualReader()
	shutdown, err := Setup(context.Background(), WithServiceName("test"), WithSpanExporter(exp), WithMetricReader(reader))
	if err != nil {
		t.Fatal(err)
	}
	return exp, reader, func() { _ = shutdown(context.Background()) }
}

// A handler doing what App.render does: name the route on the request span,
// open a render span under it, read a document under that.
func pageLike(w http.ResponseWriter, r *http.Request) {
	observe.Current(r.Context()).Set("http.route", "/orders/{id}")
	ctx, render := observe.Start(r.Context(), "howl.render /orders/{id}")
	render.Set(observe.Kind, "render")
	render.Set("howl.route", "/orders/{id}")
	render.Set("howl.mode", "fragment")
	_, get := observe.Start(ctx, "db.get orders")
	get.Set(observe.Kind, "db")
	get.Set("db.collection.name", "orders")
	get.Set("db.operation.name", "get")
	get.Event("cache.miss")
	get.End(nil)
	render.End(nil)
	w.Header().Set(mw.HeaderRequestID, "req-1")
	w.WriteHeader(http.StatusOK)
}

func TestRequestSpanIsNamedByRouteAndChildrenNestUnderIt(t *testing.T) {
	exp, reader, stop := setup(t)
	defer stop()

	h := mw.Chain(http.HandlerFunc(pageLike), HTTP())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/orders/42", nil)
	req.Header.Set("X-Partial", "1")
	h.ServeHTTP(rec, req)
	if err := forceFlush(); err != nil {
		t.Fatal(err)
	}

	byName := map[string]tracetest.SpanStub{}
	for _, s := range exp.GetSpans() {
		byName[s.Name] = s
	}
	request, ok := byName["GET /orders/{id}"]
	if !ok {
		t.Fatalf("no request span named by route; have %v", names(exp))
	}
	render, ok := byName["howl.render /orders/{id}"]
	if !ok || render.Parent.SpanID() != request.SpanContext.SpanID() {
		t.Fatalf("render span missing or not under the request; have %v", names(exp))
	}
	get := byName["db.get orders"]
	if get.Parent.SpanID() != render.SpanContext.SpanID() {
		t.Error("the document read is not under the render")
	}
	if !hasAttr(request, "howl.partial", "true") || !hasAttr(request, "howl.request_id", "req-1") {
		t.Errorf("request span attributes: %v", request.Attributes)
	}
	if rec.Header().Get(HeaderTraceID) != request.SpanContext.TraceID().String() {
		t.Error("X-Trace-Id is not the trace's id")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := metricNames(rm)
	for _, want := range []string{"http.server.request.duration", "howl.render.duration", "db.client.operation.duration", "howl.cache"} {
		if !got[want] {
			t.Errorf("metric %s not recorded; have %v", want, got)
		}
	}
}

func TestEndpointFailureRecordsStageAndCounts(t *testing.T) {
	exp, reader, stop := setup(t)
	defer stop()

	_, span := observe.Start(context.Background(), "api Metrics")
	span.Set(observe.Kind, "api")
	span.Set("howl.endpoint", "Metrics")
	span.Set("howl.api.stage", "validate")
	span.End(errors.New("limit must be positive"))
	if err := forceFlush(); err != nil {
		t.Fatal(err)
	}
	s := exp.GetSpans()[0]
	if s.Status.Code.String() != "Error" || !hasAttr(s, "howl.api.stage", "validate") {
		t.Errorf("span %+v", s)
	}
	var rm metricdata.ResourceMetrics
	_ = reader.Collect(context.Background(), &rm)
	if got := metricNames(rm); !got["howl.api.errors"] || !got["howl.api.duration"] {
		t.Errorf("metrics %v", got)
	}
}

func TestCurrentDoesNotEndABorrowedSpan(t *testing.T) {
	exp, _, stop := setup(t)
	defer stop()
	ctx, own := observe.Start(context.Background(), "outer")
	observe.Current(ctx).End(errors.New("noted"))
	if err := forceFlush(); err != nil {
		t.Fatal(err)
	}
	if len(exp.GetSpans()) != 0 {
		t.Fatal("Current().End() ended a span it did not open")
	}
	own.End(nil)
	_ = forceFlush()
	if s := exp.GetSpans(); len(s) != 1 || s[0].Status.Code.String() != "Error" {
		t.Fatalf("the error was not recorded on the borrowed span: %+v", s)
	}
}

func TestSlogHandlerJoinsALogLineToItsTrace(t *testing.T) {
	_, _, stop := setup(t)
	defer stop()
	var buf bytes.Buffer
	log := slog.New(SlogHandler(slog.NewJSONHandler(&buf, nil)))
	ctx, span := observe.Start(context.Background(), "x")
	log.InfoContext(ctx, "hello")
	span.End(nil)
	log.Info("outside")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if !strings.Contains(lines[0], `"trace_id":"`+TraceID(ctx)+`"`) || !strings.Contains(lines[0], `"span_id"`) {
		t.Errorf("traced line: %s", lines[0])
	}
	if strings.Contains(lines[1], "trace_id") {
		t.Errorf("untraced line carries ids: %s", lines[1])
	}
}

func TestSetupRefusesAProtocolItCannotSpeak(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	if _, err := Setup(context.Background(), WithSpanExporter(tracetest.NewInMemoryExporter()), WithoutMetrics()); err == nil {
		t.Fatal("grpc was accepted; nothing would have been exported")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	shutdown, err := Setup(context.Background(), WithSpanExporter(tracetest.NewInMemoryExporter()), WithoutMetrics())
	if err != nil {
		t.Fatal(err)
	}
	_ = shutdown(context.Background())
}

func TestServiceNameComesFromTheEnvironmentFirst(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "pack")
	exp := tracetest.NewInMemoryExporter()
	shutdown, err := Setup(context.Background(), WithServiceName("binary"), WithSpanExporter(exp), WithoutMetrics())
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(context.Background())
	_, s := observe.Start(context.Background(), "x")
	s.End(nil)
	_ = forceFlush()
	res := exp.GetSpans()[0].Resource
	found := false
	for _, kv := range res.Attributes() {
		if string(kv.Key) == "service.name" {
			found = kv.Value.AsString() == "pack"
		}
	}
	if !found {
		t.Errorf("service.name: %v", res.Attributes())
	}
	os.Unsetenv("OTEL_SERVICE_NAME")
}

// ---------------------------------------------------------------------------

func hasAttr(s tracetest.SpanStub, key, value string) bool {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key && kv.Value.Emit() == value {
			return true
		}
	}
	return false
}

func names(exp *tracetest.InMemoryExporter) []string {
	var out []string
	for _, s := range exp.GetSpans() {
		out = append(out, s.Name)
	}
	return out
}

func metricNames(rm metricdata.ResourceMetrics) map[string]bool {
	out := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = true
		}
	}
	return out
}

// Every method, not just the ones the framework routes itself: a handler put
// on the mux by hand — a form post, a fragment endpoint, a webhook — is named
// by the pattern that served it, and keeps its name through the middleware
// that clones the request on the way in.
func TestRoutesNamesEveryMethodIncludingHandWrittenHandlers(t *testing.T) {
	exp, _, stop := setup(t)
	defer stop()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/todos", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("DELETE /api/todos/{id}", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })

	// mw.RequestID between the span and the mux, as a real Use list has it:
	// it clones the request, which is exactly what Routes has to survive.
	h := mw.Chain(Routes(mux), HTTP(), mw.RequestID)

	for _, tc := range []struct{ method, url, want string }{
		{http.MethodPost, "/api/todos", "POST /api/todos"},
		{http.MethodDelete, "/api/todos/42", "DELETE /api/todos/{id}"},
		{http.MethodPut, "/api/settings", "PUT /api/settings"},
		{http.MethodPatch, "/nope", "PATCH /"}, // the catch-all is a pattern too
	} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.url, nil))
	}
	if err := forceFlush(); err != nil {
		t.Fatal(err)
	}

	got := names(exp)
	for i, tc := range []string{"POST /api/todos", "DELETE /api/todos/{id}", "PUT /api/settings", "PATCH /"} {
		if i >= len(got) || got[i] != tc {
			t.Errorf("span %d = %q, want %q (all: %v)", i, at(got, i), tc, got)
		}
	}
	// The path template, not the URL: one series for every id, not one each.
	for _, s := range exp.GetSpans() {
		if s.Name == "DELETE /api/todos/{id}" && !hasAttr(s, "http.route", "/api/todos/{id}") {
			t.Errorf("http.route is not the template: %v", s.Attributes)
		}
	}
}

func at(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "<missing>"
}

// The db layer's counts and its storage spans, as metrics: the operation, the
// backend call inside it, and the cache lookups it made on the way.
func TestDatabaseSpansBecomeMetrics(t *testing.T) {
	_, reader, stop := setup(t)
	defer stop()

	ctx, op := observe.Start(context.Background(), "db.get_many users")
	op.Set(observe.Kind, "db")
	op.Set("db.system.name", "sql")
	op.Set("db.collection.name", "users")
	op.Set("db.operation.name", "get_many")
	op.Set("howl.cache.hits", 30)
	op.Set("howl.cache.misses", 20)
	_, storage := observe.Start(ctx, "sql.find_many users")
	storage.Set(observe.Kind, "db.storage")
	storage.Set("db.collection.name", "users")
	storage.End(nil)
	op.End(nil)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := metricNames(rm)
	for _, want := range []string{"db.client.operation.duration", "howl.db.storage.duration", "howl.cache"} {
		if !got[want] {
			t.Errorf("metric %s not recorded; have %v", want, got)
		}
	}
	// Fifty lookups, counted as fifty — not as one event apiece, and not as one.
	hits, misses := cacheCounts(rm)
	if hits != 30 || misses != 20 {
		t.Errorf("howl.cache: %d hits, %d misses; want 30 and 20", hits, misses)
	}
}

// The two outcomes a span cannot show, read from the service's own counters.
func TestWatchCacheReportsWhatSpansCannot(t *testing.T) {
	_, reader, stop := setup(t)
	defer stop()

	if err := WatchCache(fakeCollection{name: "users", stats: db.CacheStats{Bypassed: 4, TooLarge: 2}}); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"howl.db.cache.bypassed": 4, "howl.db.cache.too_large": 2}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if expect, ok := want[m.Name]; ok {
				sum, _ := m.Data.(metricdata.Sum[int64])
				if len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != expect {
					t.Errorf("%s = %+v, want %d", m.Name, sum.DataPoints, expect)
				}
				delete(want, m.Name)
			}
		}
	}
	if len(want) != 0 {
		t.Errorf("not reported: %v", want)
	}
}

type fakeCollection struct {
	name  string
	stats db.CacheStats
}

func (f fakeCollection) Collection() string        { return f.name }
func (f fakeCollection) CacheStats() db.CacheStats { return f.stats }

func cacheCounts(rm metricdata.ResourceMetrics) (hits, misses int64) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "howl.cache" {
				continue
			}
			sum, _ := m.Data.(metricdata.Sum[int64])
			for _, dp := range sum.DataPoints {
				result, _ := dp.Attributes.Value("result")
				switch result.Emit() {
				case "hit":
					hits += dp.Value
				case "miss":
					misses += dp.Value
				}
			}
		}
	}
	return hits, misses
}

// The browser's half of a navigation, joined to the server's. app.js mints the
// ids, sends them as a traceparent on the fragment fetch, and posts the timing
// afterwards; the request the server already traced has to end up inside the
// navigation span rather than beside it.
func TestNavigationsClaimTheBrowsersSpanAndParentTheRequest(t *testing.T) {
	exp, _, stop := setup(t)
	defer stop()

	const traceID, spanID = "0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331"

	// The fragment request, as the browser makes it: traceparent naming the
	// navigation span that does not exist yet.
	srv := httptest.NewServer(mw.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observe.Current(r.Context()).Set("http.route", "/dashboard/metrics")
	}), HTTP()))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/dashboard/metrics", nil)
	req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	req.Header.Set("X-Partial", "1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	// And the beacon that follows it.
	rec := httptest.NewRecorder()
	body := `{"navigations":[{"path":"/dashboard/metrics","mode":"fragment","ms":220.5,"bytes":1834,` +
		`"traceId":"` + traceID + `","spanId":"` + spanID + `"}]}`
	Navigations().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/telemetry/navigations", strings.NewReader(body)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("beacon answered %d", rec.Code)
	}
	if err := forceFlush(); err != nil {
		t.Fatal(err)
	}

	var nav, request tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		switch s.Name {
		case "howl.navigate /dashboard/metrics":
			nav = s
		case "GET /dashboard/metrics":
			request = s
		}
	}
	if nav.Name == "" || request.Name == "" {
		t.Fatalf("missing a span; have %v", names(exp))
	}
	if got := nav.SpanContext.SpanID().String(); got != spanID {
		t.Errorf("the navigation span id is %s, not the browser's %s — the request will hang off nothing", got, spanID)
	}
	if got := request.Parent.SpanID().String(); got != spanID {
		t.Errorf("the request is parented to %s, not the navigation", got)
	}
	if nav.SpanContext.TraceID().String() != traceID || request.SpanContext.TraceID().String() != traceID {
		t.Error("the two halves are in different traces")
	}
	if !hasAttr(nav, "howl.mode", "fragment") || !hasAttr(nav, "howl.bytes", "1834") {
		t.Errorf("navigation attributes: %v", nav.Attributes)
	}
	if took := nav.EndTime.Sub(nav.StartTime); took < 220*time.Millisecond || took > 221*time.Millisecond {
		t.Errorf("navigation lasted %s, the browser said 220.5ms", took)
	}
}

// A locally rendered navigation never contacts the server, so the beacon is
// the only evidence it happened — and it has no child by construction.
func TestNavigationsRecordAWasmRenderTheServerNeverSaw(t *testing.T) {
	exp, _, stop := setup(t)
	defer stop()

	body := `{"navigations":[
		{"path":"/dashboard","mode":"wasm","ms":0.08,"bytes":0,"traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"b7ad6b7169203331"},
		{"path":"/nope","mode":"fragment","ms":10,"bytes":1,"traceId":"not-hex","spanId":"b7ad6b7169203331"},
		{"path":"","mode":"fragment","ms":10,"bytes":1,"traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"b7ad6b7169203331"}
	]}`
	rec := httptest.NewRecorder()
	Navigations().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body)))
	if err := forceFlush(); err != nil {
		t.Fatal(err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("%d spans for one valid row and two malformed ones: %v", len(spans), names(exp))
	}
	if !hasAttr(spans[0], "howl.mode", "wasm") || !hasAttr(spans[0], "howl.client_measured", "true") {
		t.Errorf("attributes: %v", spans[0].Attributes)
	}
}

// A batch is a beacon: it must not become a way to make the process allocate.
func TestNavigationsRefusesRubbish(t *testing.T) {
	_, _, stop := setup(t)
	defer stop()
	for _, body := range []string{"", "{", `{"navigations":"no"}`, strings.Repeat("x", 70<<10)} {
		rec := httptest.NewRecorder()
		Navigations().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %.20q answered %d, want 400", body, rec.Code)
		}
	}
}
