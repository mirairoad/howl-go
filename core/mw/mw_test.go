package mw

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func ok(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(body)) //nolint:errcheck
	})
}

func TestChainRunsOutermostFirst(t *testing.T) {
	var order []string
	mark := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := Chain(ok("x"), mark("a"), mark("b"), mark("c"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	if got := strings.Join(order, ""); got != "abc" {
		t.Fatalf("order = %q, want abc", got)
	}
}

func TestCompressSkipsSmallBodies(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")

	Compress{}.Handler(ok("tiny")).ServeHTTP(rec, r)

	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("Content-Encoding = %q, want none — a 4 byte body gets bigger gzipped", enc)
	}
	if rec.Body.String() != "tiny" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestCompressGzipsLargeBodies(t *testing.T) {
	body := strings.Repeat("howl", 1000)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")

	Compress{}.Handler(ok(body)).ServeHTTP(rec, r)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("large compressible body was not gzipped")
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatal("Vary: Accept-Encoding missing — a shared cache could serve gzip to a client that cannot read it")
	}
	// A gzip frame around uncompressed data is valid, decodes correctly, and is
	// slightly LARGER than the input — which is what a pooled zero-value
	// gzip.Writer (level 0) silently produces. Assert the ratio, not just that
	// it round-trips.
	if rec.Body.Len() > len(body)/2 {
		t.Fatalf("gzip produced %d bytes from %d — that is not compression", rec.Body.Len(), len(body))
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != body {
		t.Fatalf("round trip lost %d bytes", len(body)-len(got))
	}
}

func TestCompressLeavesBinaryAlone(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(bytes.Repeat([]byte{0x89}, 4000)) //nolint:errcheck
	})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")

	Compress{}.Handler(h).ServeHTTP(rec, r)

	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatal("png was gzipped; it is already compressed")
	}
}

func TestCORSPreflight(t *testing.T) {
	h := CORS{Origins: []string{"https://app.example.com"}, Methods: []string{"GET", "POST"}}.Handler(ok("x"))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("OPTIONS", "/api", nil)
	r.Header.Set("Origin", "https://app.example.com")
	r.Header.Set("Access-Control-Request-Method", "POST")
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("allow-origin = %q", got)
	}

	rec = httptest.NewRecorder()
	r = httptest.NewRequest("OPTIONS", "/api", nil)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Access-Control-Request-Method", "POST")
	h.ServeHTTP(rec, r)

	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("an unlisted origin was allowed")
	}
}

func TestCSRFRejectsWithoutToken(t *testing.T) {
	h := CSRF{}.Handler(ok("x"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/save", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestCSRFAcceptsMatchingToken(t *testing.T) {
	var token string
	h := CSRF{}.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = CSRFToken(r.Context())
	}))

	// A GET mints the token and sets the cookie.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/form", nil))
	if token == "" {
		t.Fatal("no token on the context")
	}
	cookie := rec.Result().Cookies()[0]

	// The form posts it back.
	rec = httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/save", strings.NewReader("csrf_token="+token))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://"+r.Host)
	r.AddCookie(cookie)
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestCSRFRejectsForeignOrigin(t *testing.T) {
	h := CSRF{}.Handler(ok("x"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/form", nil))
	cookie := rec.Result().Cookies()[0]

	rec = httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/save", nil)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("X-CSRF-Token", cookie.Value)
	r.AddCookie(cookie)
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — the token alone is not enough", rec.Code)
	}
}

func TestCoalesceRunsHandlerOnce(t *testing.T) {
	var runs atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if runs.Add(1) == 1 {
			close(started)
		}
		<-release
		w.Write([]byte("rendered")) //nolint:errcheck
	})

	c := &Coalesce{}
	h := c.Handler(slow)

	const n = 8
	var wg sync.WaitGroup
	bodies := make([]string, n)
	call := func(i int) {
		defer wg.Done()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/page", nil))
		bodies[i] = rec.Body.String()
	}

	// The leader first, and only once it is inside the handler do the others
	// arrive — otherwise two of them race to be the leader and the test is
	// measuring the scheduler rather than the middleware.
	wg.Add(1)
	go call(0)
	<-started
	for i := 1; i < n; i++ {
		wg.Add(1)
		go call(i)
	}
	// The followers register before they block, but not instantly; give the
	// leader's answer a moment to be the thing they wait on.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := runs.Load(); got != 1 {
		t.Fatalf("handler ran %d times, want 1", got)
	}
	for i, b := range bodies {
		if b != "rendered" {
			t.Fatalf("waiter %d got %q", i, b)
		}
	}
}

func TestCoalesceSkipsCredentialedRequests(t *testing.T) {
	var runs atomic.Int32
	c := &Coalesce{}
	h := c.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runs.Add(1)
	}))

	for range 3 {
		r := httptest.NewRequest("GET", "/me", nil)
		r.Header.Set("Cookie", "session=abc")
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if got := runs.Load(); got != 3 {
		t.Fatalf("handler ran %d times, want 3 — per-user responses must never be shared", got)
	}
}

func TestRequestIDRejectsUnsafeInboundValue(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = ID(r.Context())
	}))
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(HeaderRequestID, "bad\nX-Injected: 1")
	h.ServeHTTP(httptest.NewRecorder(), r)

	if strings.Contains(seen, "\n") || seen == "" {
		t.Fatalf("id = %q, want a generated one", seen)
	}
}

// "Who is hitting my API" is the question this answers. Your own pages send a
// same-origin Referer or Sec-Fetch-Site; a program does not.
func TestLoggerIdentifiesOutsideCallers(t *testing.T) {
	logged := func(setup func(*http.Request)) string {
		var buf bytes.Buffer
		l := slog.New(slog.NewTextHandler(&buf, nil))
		h := LogWith(LogOptions{Logger: l, Callers: true})(ok("x"))
		r := httptest.NewRequest("GET", "/api/summary", nil)
		r.RemoteAddr = "203.0.113.9:51000"
		r.Header.Set("User-Agent", "otel-collector/1.2")
		setup(r)
		h.ServeHTTP(httptest.NewRecorder(), r)
		return buf.String()
	}

	external := logged(func(r *http.Request) {})
	if !strings.Contains(external, "ip=203.0.113.9") || !strings.Contains(external, "ua=otel-collector/1.2") {
		t.Fatalf("outside caller not identified: %s", external)
	}

	ownPage := logged(func(r *http.Request) {
		r.Header.Set("Referer", "http://"+r.Host+"/logs")
	})
	if strings.Contains(ownPage, "ip=") {
		t.Fatalf("our own page's fetch was tagged as an outside caller: %s", ownPage)
	}

	fetchMeta := logged(func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") })
	if strings.Contains(fetchMeta, "ip=") {
		t.Fatalf("same-origin fetch was tagged as an outside caller: %s", fetchMeta)
	}

	crossSite := logged(func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Referer", "http://"+r.Host+"/spoof")
	})
	if !strings.Contains(crossSite, "ip=") {
		t.Fatalf("cross-site request went unlogged despite a forged Referer: %s", crossSite)
	}
}

func TestLoggerSkipsNoise(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	h := LogWith(LogOptions{Logger: l, Skip: SkipNoise})(ok("x"))

	for _, p := range []string{"/static/app.js", "/healthz", "/favicon.ico"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
	}
	if buf.Len() != 0 {
		t.Fatalf("noise reached the log: %s", buf.String())
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/docs", nil))
	if !strings.Contains(buf.String(), "/docs") {
		t.Fatalf("a real request was skipped: %s", buf.String())
	}
}

func TestClientIPIgnoresForwardedHeaderUnlessTrusted(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:4000"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")

	if got := clientIP(r, false); got != "10.0.0.5" {
		t.Fatalf("untrusted = %q — anyone can send X-Forwarded-For", got)
	}
	if got := clientIP(r, true); got != "1.2.3.4" {
		t.Fatalf("trusted = %q", got)
	}
}

func TestRecoverAnswers500(t *testing.T) {
	h := Recover(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestCoalesceLeavesEventStreamsAlone(t *testing.T) {
	// An event stream never ends, so recording one means the browser sees
	// nothing at all. Two concurrent listeners must each get their own handler
	// call rather than one being replayed the other's buffered output.
	var calls atomic.Int64
	c := &Coalesce{}
	handler := c.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: hello\n\n")) //nolint:errcheck
	}))

	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			request := httptest.NewRequest(http.MethodGet, "/stream", nil)
			request.Header.Set("Accept", "text/event-stream")
			handler.ServeHTTP(httptest.NewRecorder(), request)
		}()
	}
	wait.Wait()
	if calls.Load() != 2 {
		t.Fatalf("an event stream was shared between listeners: %d calls", calls.Load())
	}
}
