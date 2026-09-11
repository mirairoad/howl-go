package mw

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirairoad/howl-go/core/cache"
)

// counting answers with how many times it has run, so a cached response is
// the one that did not move.
func counting(runs *atomic.Int64, set func(http.Header)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := runs.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if set != nil {
			set(w.Header())
		}
		w.Write([]byte(`{"run":` + strconv.FormatInt(n, 10) + `}`)) //nolint:errcheck
	})
}

func get(h http.Handler, target string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestCacheServesRepeatsFromTheStore(t *testing.T) {
	var runs atomic.Int64
	h := Cache{TTL: time.Minute}.Handler(counting(&runs, func(h http.Header) { h.Set("X-Total", "3") }))

	first := get(h, "/suggest?q=syd&limit=5")
	second := get(h, "/suggest?limit=5&q=syd") // same query, other order

	if runs.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", runs.Load())
	}
	if first.Header().Get(HeaderCache) != "miss" || second.Header().Get(HeaderCache) != "hit" {
		t.Fatalf("X-Howl-Cache = %q then %q, want miss then hit",
			first.Header().Get(HeaderCache), second.Header().Get(HeaderCache))
	}
	if second.Body.String() != first.Body.String() {
		t.Fatalf("hit body = %q, want %q", second.Body.String(), first.Body.String())
	}
	if second.Header().Get("X-Total") != "3" || second.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("hit lost the handler's headers: %v", second.Header())
	}
	if second.Header().Get("Age") == "" {
		t.Fatal("hit has no Age")
	}
}

func TestCacheNeverStoresWhatItMustNot(t *testing.T) {
	cases := map[string]struct {
		handler http.Handler
		method  string
	}{
		"a response that sets a cookie": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.SetCookie(w, &http.Cookie{Name: "session", Value: "abc"})
				w.Write([]byte("x")) //nolint:errcheck
			}),
		},
		"a 500": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			}),
		},
		"a stream": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("data: 1\n\n")) //nolint:errcheck
				w.(http.Flusher).Flush()
			}),
		},
		"a POST": {
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("x")) }), //nolint:errcheck
			method:  "POST",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var runs atomic.Int64
			h := Cache{TTL: time.Minute}.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				runs.Add(1)
				tc.handler.ServeHTTP(w, r)
			}))
			method := tc.method
			if method == "" {
				method = "GET"
			}
			for range 2 {
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, "/x", nil))
			}
			if runs.Load() != 2 {
				t.Fatalf("handler ran %d times, want 2 — %s was stored", runs.Load(), name)
			}
		})
	}
}

func TestCacheDefaultKeySkipsCredentialedRequests(t *testing.T) {
	var runs atomic.Int64
	h := Cache{TTL: time.Minute}.Handler(counting(&runs, nil))

	get(h, "/me", "Cookie", "session=alice")
	w := get(h, "/me", "Cookie", "session=bob")

	if runs.Load() != 2 {
		t.Fatalf("handler ran %d times — bob was served alice's response", runs.Load())
	}
	if w.Header().Get(HeaderCache) != "" {
		t.Fatalf("a request the cache declined says %q", w.Header().Get(HeaderCache))
	}
}

func TestCacheKeyAndVarySeparateEntries(t *testing.T) {
	var runs atomic.Int64
	h := Cache{
		TTL:  time.Minute,
		Key:  func(r *http.Request) string { return r.URL.Path + "|" + r.Header.Get("Authorization") },
		Vary: []string{"X-Partial"},
	}.Handler(counting(&runs, nil))

	get(h, "/p", "Authorization", "Bearer a")
	get(h, "/p", "Authorization", "Bearer a") // hit
	get(h, "/p", "Authorization", "Bearer b") // another caller
	get(h, "/p", "Authorization", "Bearer a", "X-Partial", "1")

	if runs.Load() != 3 {
		t.Fatalf("handler ran %d times, want 3", runs.Load())
	}
}

func TestCacheDoesNotReplayMiddlewareHeaders(t *testing.T) {
	store := cache.NewLRU(10)
	var runs atomic.Int64
	h := Chain(counting(&runs, nil), RequestID, Cache{Store: store, TTL: time.Minute}.Handler)

	first := get(h, "/x")
	second := get(h, "/x")
	if runs.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", runs.Load())
	}
	if first.Header().Get(HeaderRequestID) == second.Header().Get(HeaderRequestID) {
		t.Fatal("the hit replayed the first request's X-Request-Id")
	}
}

func TestCacheTreatsACorruptEntryAsAMiss(t *testing.T) {
	store := cache.NewLRU(10)
	var runs atomic.Int64
	h := Cache{Store: store, TTL: time.Minute}.Handler(counting(&runs, nil))

	r := httptest.NewRequest("GET", "/x", nil)
	store.Set(r.Context(), storeKey(sharedKey(r), r, nil), []byte("not an entry"), time.Minute)

	if w := get(h, "/x"); w.Code != http.StatusOK || runs.Load() != 1 {
		t.Fatalf("status %d, runs %d: a corrupt entry was served", w.Code, runs.Load())
	}
}
