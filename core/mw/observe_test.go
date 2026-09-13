package mw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mirairoad/howl-go/core/observe"
)

// A named middleware's span covers what it did before handing over, and
// records the answer when it did not hand over at all.
func TestNamedSpansTheGuardNotThePage(t *testing.T) {
	rec := &observe.Recorder{}
	var pageSawSpanEnded bool
	page := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pageSawSpanEnded = rec.Named("guard.session")[0].Ended
		w.WriteHeader(http.StatusOK)
	})
	guard := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Cookie") == "" {
				http.Redirect(w, r, "/sign-in", http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	h := Named("guard.session", guard)(page)
	ctx := observe.With(context.Background(), rec)

	// Let through: the span ended before the page ran.
	r := httptest.NewRequest(http.MethodGet, "/dashboard", nil).WithContext(ctx)
	r.Header.Set("Cookie", "session=1")
	h.ServeHTTP(httptest.NewRecorder(), r)
	passed := rec.Named("guard.session")[0]
	if !pageSawSpanEnded || passed.Attrs["howl.passed"] != true {
		t.Errorf("passed: ended-before-page=%v attrs=%v", pageSawSpanEnded, passed.Attrs)
	}

	// Bounced: the span carries the redirect and the status.
	rec2 := &observe.Recorder{}
	r = httptest.NewRequest(http.MethodGet, "/dashboard", nil).WithContext(observe.With(context.Background(), rec2))
	h.ServeHTTP(httptest.NewRecorder(), r)
	bounced := rec2.Named("guard.session")[0]
	if bounced.Attrs["howl.passed"] != false || bounced.Attrs["howl.redirect"] != "/sign-in" || bounced.Attrs["http.response.status_code"] != http.StatusSeeOther {
		t.Errorf("bounced: %v", bounced.Attrs)
	}
	if !bounced.Ended {
		t.Error("the bounced span never ended")
	}
}

// The cache and the limiter report their decisions as events on whatever
// span the request is in — the request span, in practice.
func TestCacheAndRateLimitReportDecisions(t *testing.T) {
	rec := &observe.Recorder{}
	ctx, request := rec.Start(context.Background(), "request")
	ctx = observe.With(ctx, rec)

	served := Cache{TTL: 60_000_000_000}.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("body")) //nolint:errcheck
	}))
	for range 2 {
		served.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/page", nil).WithContext(ctx))
	}
	limited := RateLimit{Requests: 1, Window: 60_000_000_000}.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for range 2 {
		limited.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil).WithContext(ctx))
	}
	request.End(nil)

	got := rec.Spans()[0].Events
	want := []string{"cache.miss", "cache.hit", "ratelimit.refused"}
	if len(got) != len(want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events %v, want %v", got, want)
		}
	}
}
