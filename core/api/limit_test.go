package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirairoad/howl-go/core/api"
	"github.com/mirairoad/howl-go/core/mw"
)

// signIn is the endpoint the whole feature exists for: nobody is signed in
// when it is called, so the caller can only be an address, and guessing
// passwords is exactly a caller repeating it.
func signIn(runs *atomic.Int64, limit api.Limit) api.Route {
	return api.At("GET", "/api/sign-in", api.Define(api.Spec[api.None, api.None, Reply]{
		Name:  "Sign In",
		Limit: limit,
		Handler: func(r *api.Request[api.None, api.None]) (Reply, error) {
			runs.Add(1)
			return Reply{Echo: "ok"}, nil
		},
	}))
}

func TestLimitRefusesWithTheEndpointErrorEnvelope(t *testing.T) {
	var runs atomic.Int64
	server := serve(t, api.Config{}, signIn(&runs, api.Limit{Requests: 2, Window: time.Minute}))

	call(t, server.URL+"/api/sign-in")
	call(t, server.URL+"/api/sign-in")
	res, err := http.Get(server.URL + "/api/sign-in")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", res.StatusCode)
	}
	if runs.Load() != 2 {
		t.Fatalf("handler ran %d times, want 2", runs.Load())
	}
	// The same shape as every other endpoint error, so a generated client
	// decodes a throttle the way it decodes a 400.
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body.Error, "too many requests") {
		t.Fatalf("error = %q", body.Error)
	}
	if res.Header.Get("Retry-After") != "30" { // 2 per minute is one every 30 s
		t.Fatalf("Retry-After = %q, want 30", res.Header.Get("Retry-After"))
	}
	if res.Header.Get(mw.HeaderRateLimit) != "2" || res.Header.Get(mw.HeaderRateLimitRemaining) != "0" {
		t.Fatalf("rate-limit headers: %v", res.Header)
	}
}

// One table, one bucket map, but a limit on one endpoint is not spent by
// traffic to another.
func TestLimitIsPerEndpoint(t *testing.T) {
	var runs atomic.Int64
	other := api.At("GET", "/api/other", api.Define(api.Spec[api.None, api.None, Reply]{
		Name:  "Other",
		Limit: api.Limit{Requests: 1, Window: time.Minute},
		Handler: func(r *api.Request[api.None, api.None]) (Reply, error) {
			return Reply{Echo: "ok"}, nil
		},
	}))
	server := serve(t, api.Config{}, signIn(&runs, api.Limit{Requests: 1, Window: time.Minute}), other)

	call(t, server.URL+"/api/sign-in")
	if res := call(t, server.URL+"/api/sign-in"); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second sign-in: status = %d, want 429", res.StatusCode)
	}
	if res := call(t, server.URL+"/api/other"); res.StatusCode != http.StatusOK {
		t.Fatalf("other endpoint: status = %d, want 200 — the endpoints shared a bucket", res.StatusCode)
	}
}

// A bucket per path parameter is a limit the caller escapes by counting.
func TestLimitIgnoresPathAndQuery(t *testing.T) {
	route := api.At("GET", "/api/orders/{id}", api.Define(api.Spec[OrderQuery, api.None, Order]{
		Name:  "Order",
		Limit: api.Limit{Requests: 2, Window: time.Minute},
		Handler: func(r *api.Request[OrderQuery, api.None]) (Order, error) {
			return Order{ID: r.Query.ID}, nil
		},
	}))
	server := serve(t, api.Config{}, route)

	call(t, server.URL+"/api/orders/1")
	call(t, server.URL+"/api/orders/2?expand=lines")
	if res := call(t, server.URL+"/api/orders/3"); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 — each id had its own bucket", res.StatusCode)
	}
}

// Identity names the caller when it can. When it cannot — which is every
// request to a sign-in endpoint — the address does, because one shared
// anonymous bucket would let one attacker lock out every visitor.
func TestLimitIdentityNamesTheCaller(t *testing.T) {
	var runs atomic.Int64
	server := serve(t, api.Config{
		Identity: func(r *http.Request) string {
			if c, err := r.Cookie("session"); err == nil {
				return "user:" + c.Value
			}
			return ""
		},
	}, signIn(&runs, api.Limit{Requests: 1, Window: time.Minute}))

	if res := call(t, server.URL+"/api/sign-in", "Cookie", "session=u1"); res.StatusCode != http.StatusOK {
		t.Fatalf("u1: status = %d, want 200", res.StatusCode)
	}
	if res := call(t, server.URL+"/api/sign-in", "Cookie", "session=u1"); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("u1 again: status = %d, want 429", res.StatusCode)
	}
	if res := call(t, server.URL+"/api/sign-in", "Cookie", "session=u2"); res.StatusCode != http.StatusOK {
		t.Fatalf("u2: status = %d, want 200 — two users shared a bucket", res.StatusCode)
	}
	// Anonymous callers fall back to the address, and this test has one.
	if res := call(t, server.URL+"/api/sign-in"); res.StatusCode != http.StatusOK {
		t.Fatalf("anonymous: status = %d, want 200", res.StatusCode)
	}
	if res := call(t, server.URL+"/api/sign-in"); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("anonymous again: status = %d, want 429 — the address was not counted", res.StatusCode)
	}
}

// By replaces who is counted — a limit per API key rather than per caller.
func TestLimitByOverridesTheKeyAndCanSkip(t *testing.T) {
	var runs atomic.Int64
	server := serve(t, api.Config{}, signIn(&runs, api.Limit{
		Requests: 1,
		Window:   time.Minute,
		By: func(r *http.Request) string {
			if r.Header.Get("X-Internal") == "yes" {
				return "" // uncounted, the way a health check should be
			}
			return r.Header.Get("X-Api-Key")
		},
	}))

	call(t, server.URL+"/api/sign-in", "X-Api-Key", "k1")
	if res := call(t, server.URL+"/api/sign-in", "X-Api-Key", "k1"); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("same key: status = %d, want 429", res.StatusCode)
	}
	if res := call(t, server.URL+"/api/sign-in", "X-Api-Key", "k2"); res.StatusCode != http.StatusOK {
		t.Fatalf("other key: status = %d, want 200", res.StatusCode)
	}
	for i := 0; i < 5; i++ {
		if res := call(t, server.URL+"/api/sign-in", "X-Internal", "yes"); res.StatusCode != http.StatusOK {
			t.Fatalf("internal call %d: status = %d, want 200", i, res.StatusCode)
		}
	}
}

// A cached response still costs bandwidth, so it still spends a token — the
// limit is outside the cache.
func TestLimitCountsCachedResponsesToo(t *testing.T) {
	var runs atomic.Int64
	route := api.At("GET", "/api/suggest", api.Define(api.Spec[SuggestQuery, api.None, Suggestion]{
		Name:  "Suggest",
		Cache: api.Cache{TTL: time.Minute},
		Limit: api.Limit{Requests: 1, Window: time.Minute},
		Handler: func(r *api.Request[SuggestQuery, api.None]) (Suggestion, error) {
			return Suggestion{Run: runs.Add(1)}, nil
		},
	}))
	server := serve(t, api.Config{}, route)

	if res := call(t, server.URL+"/api/suggest?q=syd"); res.Header.Get(mw.HeaderCache) != "miss" {
		t.Fatalf("first call: X-Howl-Cache = %q", res.Header.Get(mw.HeaderCache))
	}
	res := call(t, server.URL+"/api/suggest?q=syd")
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second call: status = %d, want 429", res.StatusCode)
	}
	if res.Header.Get(mw.HeaderCache) == "hit" {
		t.Fatal("a throttled request was answered from the cache")
	}
}

// The flood this exists to stop arrives with no session. Checking the limit
// first means it costs a bucket lookup, not a session lookup and a query.
func TestLimitRunsBeforeAuthorize(t *testing.T) {
	var runs, checks atomic.Int64
	route := api.At("GET", "/api/secrets", api.Define(api.Spec[api.None, api.None, Reply]{
		Name:  "Secrets",
		Roles: []string{"admin"},
		Limit: api.Limit{Requests: 1, Window: time.Minute},
		Handler: func(r *api.Request[api.None, api.None]) (Reply, error) {
			runs.Add(1)
			return Reply{Echo: "ok"}, nil
		},
	}))
	server := serve(t, api.Config{
		Authorize: func(r *http.Request, roles []string) error {
			checks.Add(1)
			return api.Forbidden("admins only")
		},
	}, route)

	if res := call(t, server.URL+"/api/secrets"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
	if res := call(t, server.URL+"/api/secrets"); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 — a refused caller went uncounted", res.StatusCode)
	}
	if checks.Load() != 1 {
		t.Fatalf("Authorize ran %d times, want 1 — the throttled request paid for it", checks.Load())
	}
}

// Requests with no Window reads like a limit and is not one.
func TestLimitWithoutAWindowPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a Limit with no Window was accepted")
		}
	}()
	api.Define(api.Spec[api.None, api.None, api.None]{
		Name:    "Broken",
		Limit:   api.Limit{Requests: 5},
		Handler: func(r *api.Request[api.None, api.None]) (api.None, error) { return api.None{}, nil },
	})
}

// A client generated from the document has to know 429 is possible, or it
// treats being throttled as an unknown failure.
func TestDocumentDescribesTheLimit(t *testing.T) {
	var runs atomic.Int64
	doc := api.Document(api.Info{Title: "Test", Version: "1"},
		api.Routes(signIn(&runs, api.Limit{Requests: 5, Window: time.Minute})))

	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Description string `json:"description"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	got := parsed.Paths["/api/sign-in"]["get"].Responses["429"].Description
	if !strings.Contains(got, "5 per 1m0s") {
		t.Fatalf("429 description = %q, want the rate in it", got)
	}
}
