package mw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func served(runs *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runs.Add(1)
		w.Write([]byte("ok")) //nolint:errcheck
	})
}

// from sends a request with a chosen client address, since that is what the
// default Key reads.
func from(h http.Handler, addr string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/api/things", nil)
	r.RemoteAddr = addr + ":51000"
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRateLimitRefusesPastTheBurst(t *testing.T) {
	var runs atomic.Int64
	h := RateLimit{Requests: 3, Window: time.Minute}.Handler(served(&runs))

	for i := 1; i <= 3; i++ {
		if res := from(h, "203.0.113.7"); res.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, res.Code)
		}
	}
	res := from(h, "203.0.113.7")
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("fourth request: status = %d, want 429", res.Code)
	}
	if runs.Load() != 3 {
		t.Fatalf("handler ran %d times, want 3 — the refusal reached it", runs.Load())
	}
	// 3 per minute is one every 20 s, so the next token is 20 s away. Never 0:
	// a client that believes Retry-After: 0 retries into another refusal.
	if got := res.Header().Get("Retry-After"); got != "20" {
		t.Fatalf("Retry-After = %q, want 20", got)
	}
}

func TestRateLimitSendsTheHeadersOnEveryResponse(t *testing.T) {
	var runs atomic.Int64
	h := RateLimit{Requests: 2, Window: time.Minute}.Handler(served(&runs))

	first := from(h, "203.0.113.7")
	second := from(h, "203.0.113.7")

	if got := first.Header().Get(HeaderRateLimit); got != "2" {
		t.Fatalf("%s = %q, want 2", HeaderRateLimit, got)
	}
	if got := first.Header().Get(HeaderRateLimitRemaining); got != "1" {
		t.Fatalf("first %s = %q, want 1", HeaderRateLimitRemaining, got)
	}
	if got := second.Header().Get(HeaderRateLimitRemaining); got != "0" {
		t.Fatalf("second %s = %q, want 0", HeaderRateLimitRemaining, got)
	}
	// Reset is an epoch second, in the future, and no further away than the
	// time it takes to refill the whole bucket.
	reset, err := strconv.ParseInt(first.Header().Get(HeaderRateLimitReset), 10, 64)
	if err != nil {
		t.Fatalf("%s = %q: %v", HeaderRateLimitReset, first.Header().Get(HeaderRateLimitReset), err)
	}
	if now := time.Now().Unix(); reset < now || reset > now+60 {
		t.Fatalf("%s = %d, want between %d and %d", HeaderRateLimitReset, reset, now, now+60)
	}
}

// A bucket refills continuously, so a caller that waits is let through again
// without waiting for a window boundary that does not exist.
func TestRateLimitRefillsOverTime(t *testing.T) {
	var runs atomic.Int64
	// 20 per 2 s is one token every 100 ms — slow enough that spending the
	// bucket refills nothing measurable, short enough to wait for one token.
	h := RateLimit{Requests: 20, Window: 2 * time.Second}.Handler(served(&runs))

	for i := 0; i < 20; i++ {
		from(h, "203.0.113.7")
	}
	if res := from(h, "203.0.113.7"); res.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 after spending the bucket", res.Code)
	}
	time.Sleep(150 * time.Millisecond)
	if res := from(h, "203.0.113.7"); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a token should have refilled", res.Code)
	}
}

func TestRateLimitIsPerCaller(t *testing.T) {
	var runs atomic.Int64
	h := RateLimit{Requests: 1, Window: time.Minute}.Handler(served(&runs))

	if res := from(h, "203.0.113.7"); res.Code != http.StatusOK {
		t.Fatalf("first caller: status = %d, want 200", res.Code)
	}
	if res := from(h, "203.0.113.7"); res.Code != http.StatusTooManyRequests {
		t.Fatalf("first caller again: status = %d, want 429", res.Code)
	}
	if res := from(h, "198.51.100.4"); res.Code != http.StatusOK {
		t.Fatalf("second caller: status = %d, want 200 — buckets were shared", res.Code)
	}
}

// X-Forwarded-For is free text. Believing it without a proxy in front lets a
// caller pick a fresh bucket per request, which is no limit at all.
func TestRateLimitIgnoresForwardedForUnlessTrusted(t *testing.T) {
	var runs atomic.Int64
	naive := RateLimit{Requests: 1, Window: time.Minute}.Handler(served(&runs))

	from(naive, "203.0.113.7", "X-Forwarded-For", "1.1.1.1")
	if res := from(naive, "203.0.113.7", "X-Forwarded-For", "2.2.2.2"); res.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 — a forged header chose a new bucket", res.Code)
	}

	behindProxy := RateLimit{Requests: 1, Window: time.Minute, TrustProxy: true}.Handler(served(&runs))
	from(behindProxy, "10.0.0.1", "X-Forwarded-For", "1.1.1.1")
	if res := from(behindProxy, "10.0.0.1", "X-Forwarded-For", "1.1.1.1"); res.Code != http.StatusTooManyRequests {
		t.Fatalf("same forwarded client: status = %d, want 429", res.Code)
	}
	if res := from(behindProxy, "10.0.0.1", "X-Forwarded-For", "2.2.2.2"); res.Code != http.StatusOK {
		t.Fatalf("other forwarded client: status = %d, want 200 — every caller shared the proxy's bucket", res.Code)
	}
}

func TestRateLimitKeyCanLetARequestPastUncounted(t *testing.T) {
	var runs atomic.Int64
	h := RateLimit{
		Requests: 1,
		Window:   time.Minute,
		Key: func(r *http.Request) string {
			if r.URL.Path == "/healthz" {
				return ""
			}
			return "everyone"
		},
	}.Handler(served(&runs))

	for i := 0; i < 5; i++ {
		r := httptest.NewRequest("GET", "/healthz", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("health check %d: status = %d, want 200", i, w.Code)
		}
		if w.Header().Get(HeaderRateLimit) != "" {
			t.Fatal("an uncounted request carried rate-limit headers")
		}
	}
}

func TestRateLimitWithoutARateIsNotAMiddleware(t *testing.T) {
	var runs atomic.Int64
	h := RateLimit{Window: time.Minute}.Handler(served(&runs)) // Requests never set

	for i := 0; i < 20; i++ {
		if res := from(h, "203.0.113.7"); res.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 — a config with no rate blocked traffic", i, res.Code)
		}
	}
	if runs.Load() != 20 {
		t.Fatalf("handler ran %d times, want 20", runs.Load())
	}
}

func TestRateLimitOnLimitOwnsTheBody(t *testing.T) {
	var runs atomic.Int64
	h := RateLimit{
		Requests: 1,
		Window:   time.Minute,
		OnLimit: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The headers are already set when OnLimit runs, which is what lets
			// api.Spec.Limit put the delay in its JSON message.
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"retry_after":` + w.Header().Get("Retry-After") + `}`)) //nolint:errcheck
		}),
	}.Handler(served(&runs))

	from(h, "203.0.113.7")
	res := from(h, "203.0.113.7")
	if res.Body.String() != `{"retry_after":60}` {
		t.Fatalf("body = %q", res.Body.String())
	}
}

// Plain LRU eviction would make "ask under ten thousand invented keys" the way
// to clear the bucket that is refusing you.
func TestRateLimitEvictionKeepsTheBucketThatIsRefusing(t *testing.T) {
	var runs atomic.Int64
	h := RateLimit{Requests: 10, Window: time.Hour, Max: 8}.Handler(served(&runs))

	for i := 0; i < 10; i++ {
		from(h, "203.0.113.7") // spends the attacker's own bucket
	}
	if res := from(h, "203.0.113.7"); res.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", res.Code)
	}
	for i := 0; i < 200; i++ { // 25x the table, every key seen once
		from(h, "198.51.100."+strconv.Itoa(i%256))
	}
	if res := from(h, "203.0.113.7"); res.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 — the flood evicted the limited bucket", res.Code)
	}
}

// A caller arriving at a table with no free room must still end up with a
// bucket that survives the next request. Evicting the newcomer instead would
// leave it with no bucket, and so no limit, for as long as the table is full.
func TestRateLimitStillLimitsANewCallerAtCapacity(t *testing.T) {
	var runs atomic.Int64
	h := RateLimit{Requests: 1, Window: time.Hour, Max: 1}.Handler(served(&runs))

	from(h, "203.0.113.7")
	if res := from(h, "203.0.113.7"); res.Code != http.StatusTooManyRequests {
		t.Fatalf("first caller again: status = %d, want 429", res.Code)
	}
	from(h, "198.51.100.4") // arrives to a table with no free room
	if res := from(h, "198.51.100.4"); res.Code != http.StatusTooManyRequests {
		t.Fatalf("newcomer again: status = %d, want 429 — its own bucket was evicted", res.Code)
	}
}

// The store is one map behind every request the process serves; a data race in
// it is a crash under exactly the traffic it exists to survive.
func TestRateLimitIsSafeUnderConcurrency(t *testing.T) {
	l := NewLimiter(128)
	rate := Rate{Requests: 100, Window: time.Second}
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if l.Take(context.Background(), "ip\x00203.0.113."+strconv.Itoa(j%64), rate).OK {
					allowed.Add(1)
				}
			}
		}(i)
	}
	wg.Wait()
	// 64 keys, at most 64 requests each against a bucket of 100: everything
	// fits, so anything refused is a token lost to a lock that was not held.
	if allowed.Load() != 3200 {
		t.Fatalf("allowed %d of 3200", allowed.Load())
	}
}
