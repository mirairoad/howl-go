package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirairoad/howl-go/core/api"
	"github.com/mirairoad/howl-go/core/mw"
)

type SignIn struct {
	Email string `json:"email"`
}

type Session struct {
	Email string `json:"email"`
}

// Signing in is a cookie. An endpoint that cannot set one sends the
// application off to write a second, untyped handler for that one call.
func TestHandlerSetsHeadersAndCookies(t *testing.T) {
	route := api.At("POST", "/api/sign-in", api.Define(api.Spec[api.None, SignIn, Session]{
		Name: "Sign In",
		Handler: func(r *api.Request[api.None, SignIn]) (Session, error) {
			r.SetCookie(&http.Cookie{Name: "session", Value: "s3cr3t", HttpOnly: true, Path: "/"})
			r.Header().Set("X-Trace", "abc")
			return Session{Email: r.Body.Email}, nil
		},
	}))
	server := serve(t, api.Config{}, route)

	res, err := http.Post(server.URL+"/api/sign-in", "application/json", strings.NewReader(`{"email":"a@b.c"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	cookies := res.Cookies()
	if len(cookies) != 1 || cookies[0].Name != "session" || cookies[0].Value != "s3cr3t" || !cookies[0].HttpOnly {
		t.Fatalf("cookies = %v", cookies)
	}
	if res.Header.Get("X-Trace") != "abc" {
		t.Fatalf("X-Trace = %q", res.Header.Get("X-Trace"))
	}
	if res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q — the framework's must win", res.Header.Get("Content-Type"))
	}
}

// Clearing a session cookie on a 401 is a real case, so headers set before an
// error still go out.
func TestHeadersSurviveAnError(t *testing.T) {
	route := api.At("GET", "/api/me", api.Define(api.Spec[api.None, api.None, Session]{
		Name: "Me",
		Handler: func(r *api.Request[api.None, api.None]) (Session, error) {
			r.SetCookie(&http.Cookie{Name: "session", Value: "", MaxAge: -1, Path: "/"})
			return Session{}, api.Unauthorized("session expired")
		},
	}))
	server := serve(t, api.Config{}, route)

	res, err := http.Get(server.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized || len(res.Cookies()) != 1 {
		t.Fatalf("status %d, cookies %v: want 401 clearing the session", res.StatusCode, res.Cookies())
	}
}

// A Request built by hand in a unit test has no writer; Header must not panic.
func TestHandBuiltRequestHasAHeader(t *testing.T) {
	r := &api.Request[api.None, api.None]{}
	r.SetCookie(&http.Cookie{Name: "a", Value: "b"})
	if got := r.Header().Get("Set-Cookie"); got != "a=b" {
		t.Fatalf("Set-Cookie = %q", got)
	}
}

type Suggestion struct {
	Run int64 `json:"run"`
}

type SuggestQuery struct {
	Q string `query:"q"`
}

func suggest(runs *atomic.Int64, roles ...string) api.Route {
	return api.At("GET", "/api/suggest", api.Define(api.Spec[SuggestQuery, api.None, Suggestion]{
		Name:  "Suggest",
		Roles: roles,
		Cache: api.Cache{TTL: time.Minute},
		Handler: func(r *api.Request[SuggestQuery, api.None]) (Suggestion, error) {
			return Suggestion{Run: runs.Add(1)}, nil
		},
	}))
}

func call(t *testing.T, url string, header ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body) //nolint:errcheck
	res.Body.Close()
	return res
}

func TestCacheReusesResponsesPerCaller(t *testing.T) {
	var runs atomic.Int64
	server := serve(t, api.Config{}, suggest(&runs))

	if res := call(t, server.URL+"/api/suggest?q=syd"); res.Header.Get(mw.HeaderCache) != "miss" {
		t.Fatalf("first call: X-Howl-Cache = %q", res.Header.Get(mw.HeaderCache))
	}
	if res := call(t, server.URL+"/api/suggest?q=syd"); res.Header.Get(mw.HeaderCache) != "hit" {
		t.Fatalf("second call: X-Howl-Cache = %q", res.Header.Get(mw.HeaderCache))
	}
	call(t, server.URL+"/api/suggest?q=mel")                                  // another query
	call(t, server.URL+"/api/suggest?q=syd", "Authorization", "Bearer alice") // another caller
	call(t, server.URL+"/api/suggest?q=syd", "Authorization", "Bearer alice") // alice's own hit

	if runs.Load() != 3 {
		t.Fatalf("handler ran %d times, want 3", runs.Load())
	}
}

func TestCacheIdentityDecidesWhoShares(t *testing.T) {
	var runs atomic.Int64
	server := serve(t, api.Config{
		// The application knows a session cookie from a CSRF cookie; the
		// framework does not.
		Identity: func(r *http.Request) string {
			if c, err := r.Cookie("session"); err == nil {
				return "user:" + c.Value
			}
			return ""
		},
	}, suggest(&runs))

	call(t, server.URL+"/api/suggest", "Cookie", "csrf=1")
	call(t, server.URL+"/api/suggest", "Cookie", "csrf=2")             // anonymous too: shared
	call(t, server.URL+"/api/suggest", "Cookie", "session=u1; csrf=3") // a user: their own

	if runs.Load() != 2 {
		t.Fatalf("handler ran %d times, want 2", runs.Load())
	}
}

// An entry filled by a caller with the role must not become the way a caller
// without it reads the response.
func TestCacheNeverSkipsAuthorize(t *testing.T) {
	var runs atomic.Int64
	server := serve(t, api.Config{
		Authorize: func(r *http.Request, roles []string) error {
			if r.Header.Get("X-Role") != "admin" {
				return api.Forbidden("admins only")
			}
			return nil
		},
		Identity: func(*http.Request) string { return "" }, // deliberately shared
	}, suggest(&runs, "admin"))

	call(t, server.URL+"/api/suggest", "X-Role", "admin")
	if res := call(t, server.URL+"/api/suggest"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — the cached entry was served past Authorize", res.StatusCode)
	}
}

func TestCacheOnAPostPanicsAtRegistration(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a cached POST was accepted")
		}
	}()
	route := api.At("POST", "/api/things", api.Define(api.Spec[api.None, api.None, api.None]{
		Name:    "Create",
		Cache:   api.Cache{TTL: time.Minute},
		Handler: func(r *api.Request[api.None, api.None]) (api.None, error) { return api.None{}, nil },
	}))
	api.Register(http.NewServeMux(), api.Config{}, route)
}

type OrderQuery struct {
	ID     int64  `path:"id" doc:"the order number"`
	Expand string `query:"expand"`
}

type Order struct {
	ID     int64  `json:"id"`
	Expand string `json:"expand"`
}

func order() api.Route {
	return api.At("GET", "/api/orders/{id}", api.Define(api.Spec[OrderQuery, api.None, Order]{
		Name:        "Order",
		Description: "One order, by its number.",
		Errors:      []int{http.StatusNotFound},
		Handler: func(r *api.Request[OrderQuery, api.None]) (Order, error) {
			return Order{ID: r.Query.ID, Expand: r.Query.Expand}, nil
		},
	}))
}

func TestPathFieldsAreDecodedAndTyped(t *testing.T) {
	server := serve(t, api.Config{}, order())

	res, err := http.Get(server.URL + "/api/orders/42?expand=lines")
	if err != nil {
		t.Fatal(err)
	}
	var got Order
	json.NewDecoder(res.Body).Decode(&got) //nolint:errcheck
	res.Body.Close()
	if got.ID != 42 || got.Expand != "lines" {
		t.Fatalf("decoded %+v, want id 42 expand lines", got)
	}

	res, err = http.Get(server.URL + "/api/orders/forty-two")
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ Field string }
	json.NewDecoder(res.Body).Decode(&body) //nolint:errcheck
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest || body.Field != "id" {
		t.Fatalf("status %d field %q, want 400 naming id", res.StatusCode, body.Field)
	}
}

// A tag naming no placeholder would leave the field zero on every request —
// "load order 42" quietly becoming "load order 0".
func TestPathFieldWithoutAPlaceholderPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a path tag with no matching placeholder was accepted")
		}
	}()
	api.Register(http.NewServeMux(), api.Config{}, api.At("GET", "/api/orders/{order_id}", order()))
}

func TestPathFieldsStayOutOfTheQueryString(t *testing.T) {
	values, err := api.EncodeQuery(OrderQuery{ID: 42, Expand: "lines"})
	if err != nil {
		t.Fatal(err)
	}
	if values.Encode() != "expand=lines" {
		t.Fatalf("query = %q, want only expand", values.Encode())
	}
}

func TestDocumentCarriesDescriptionErrorsAndTypedPath(t *testing.T) {
	doc := api.Document(api.Info{}, []api.Route{order()})
	op := doc["paths"].(map[string]any)["/api/orders/{id}"].(map[string]any)["get"].(map[string]any)

	if op["description"] != "One order, by its number." {
		t.Fatalf("description = %v", op["description"])
	}
	if _, ok := op["responses"].(map[string]any)["404"]; !ok {
		t.Fatalf("declared 404 missing: %v", op["responses"])
	}
	var id, expand map[string]any
	for _, p := range op["parameters"].([]any) {
		switch param := p.(map[string]any); param["name"] {
		case "id":
			id = param
		case "expand":
			expand = param
		}
	}
	if id == nil || id["in"] != "path" || id["schema"].(map[string]any)["type"] != "integer" || id["description"] != "the order number" {
		t.Fatalf("path parameter = %v, want an integer described from its field", id)
	}
	if expand == nil || expand["in"] != "query" {
		t.Fatalf("query parameter = %v", expand)
	}
	if len(op["parameters"].([]any)) != 2 {
		t.Fatalf("parameters = %v — the path field was also documented as a query", op["parameters"])
	}
}
