package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mirairoad/howl-go/core/api"
)

type Query struct {
	Service string    `query:"service"`
	Limit   int       `query:"limit"`
	Live    bool      `query:"live"`
	Since   time.Time `query:"since"`
	Ratio   float64   `query:"ratio"`
	Skipped string    `query:"-"`
}

func (q Query) Validate() error {
	if q.Limit < 0 {
		return api.Invalid("limit", "must not be negative")
	}
	return nil
}

type Body struct {
	Message string `json:"message"`
}

func (b Body) Validate() error {
	if strings.TrimSpace(b.Message) == "" {
		return api.Invalid("message", "is required")
	}
	return nil
}

type Reply struct {
	Echo    string `json:"echo"`
	Service string `json:"service"`
	Limit   int    `json:"limit"`
}

type Created struct {
	ID string `json:"id"`
}

func (Created) Status() int { return http.StatusCreated }

func serve(t *testing.T, cfg api.Config, routes ...api.Route) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	api.Register(mux, cfg, routes...)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

var echo = api.At("GET", "/api/echo", api.Define(api.Spec[Query, api.None, Reply]{
	Name: "Echo",
	Handler: func(r *api.Request[Query, api.None]) (Reply, error) {
		return Reply{Echo: "ok", Service: r.Query.Service, Limit: r.Query.Limit}, nil
	},
}))

func TestQueryIsDecodedAndTyped(t *testing.T) {
	var seen Query
	route := api.At("GET", "/api/echo", api.Define(api.Spec[Query, api.None, Reply]{
		Name: "Echo",
		Handler: func(r *api.Request[Query, api.None]) (Reply, error) {
			seen = r.Query
			return Reply{Echo: "ok"}, nil
		},
	}))
	server := serve(t, api.Config{}, route)

	res, err := http.Get(server.URL + "/api/echo?service=guard&limit=25&live=true&ratio=0.5&since=2026-08-12T00:00:00Z&skipped=nope")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if seen.Service != "guard" || seen.Limit != 25 || !seen.Live || seen.Ratio != 0.5 {
		t.Fatalf("decoded = %#v", seen)
	}
	if seen.Since.IsZero() {
		t.Fatal("time was not decoded")
	}
	if seen.Skipped != "" {
		t.Fatal(`query:"-" was decoded anyway`)
	}
}

// A field that cannot hold what was sent is the caller's mistake, and the
// answer names the field — clamping it instead would answer 200 with a page
// nobody asked for.
func TestBadQueryValueIsA400NamingTheField(t *testing.T) {
	server := serve(t, api.Config{}, echo)

	res, err := http.Get(server.URL + "/api/echo?limit=twelve")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(res.Body).Decode(&body) //nolint:errcheck
	if body["field"] != "limit" {
		t.Fatalf("body = %v, expected it to name the field", body)
	}
}

func TestValidateRunsBeforeTheHandler(t *testing.T) {
	called := false
	route := api.At("GET", "/api/echo", api.Define(api.Spec[Query, api.None, Reply]{
		Name: "Echo",
		Handler: func(r *api.Request[Query, api.None]) (Reply, error) {
			called = true
			return Reply{}, nil
		},
	}))
	server := serve(t, api.Config{}, route)

	res, err := http.Get(server.URL + "/api/echo?limit=-1")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if called {
		t.Fatal("the handler ran on input that failed validation")
	}
}

func TestBodyDecodingAndStatus(t *testing.T) {
	route := api.At("POST", "/api/things", api.Define(api.Spec[api.None, Body, Created]{
		Name: "Create Thing",
		Handler: func(r *api.Request[api.None, Body]) (Created, error) {
			return Created{ID: r.Body.Message}, nil
		},
	}))
	server := serve(t, api.Config{}, route)

	res, err := http.Post(server.URL+"/api/things", "application/json", strings.NewReader(`{"message":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 from Status()", res.StatusCode)
	}
	var created Created
	json.NewDecoder(res.Body).Decode(&created) //nolint:errcheck
	if created.ID != "hello" {
		t.Fatalf("body = %#v", created)
	}
}

// A misspelled field is a bug in the caller. Accepting it silently produces an
// empty value the handler then has to explain.
func TestUnknownBodyFieldIsRejected(t *testing.T) {
	route := api.At("POST", "/api/things", api.Define(api.Spec[api.None, Body, Created]{
		Name:    "Create Thing",
		Handler: func(r *api.Request[api.None, Body]) (Created, error) { return Created{}, nil },
	}))
	server := serve(t, api.Config{}, route)

	res, err := http.Post(server.URL+"/api/things", "application/json", strings.NewReader(`{"mesage":"typo"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

// Roles are strings the endpoint declares; what they mean is the application's
// business, and this is the whole contract between the two.
func TestRolesAreDelegatedToTheApplication(t *testing.T) {
	var asked []string
	route := api.At("POST", "/api/admin", api.Define(api.Spec[api.None, api.None, api.None]{
		Name:    "Admin",
		Roles:   []string{"admin", "owner"},
		Handler: func(r *api.Request[api.None, api.None]) (api.None, error) { return api.None{}, nil },
	}))
	server := serve(t, api.Config{
		Authorize: func(r *http.Request, roles []string) error {
			asked = roles
			if r.Header.Get("Authorization") == "" {
				return api.Unauthorized("token required")
			}
			return nil
		},
	}, route)

	res, err := http.Post(server.URL+"/api/admin", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	if strings.Join(asked, ",") != "admin,owner" {
		t.Fatalf("roles handed over = %v", asked)
	}

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/admin", nil)
	req.Header.Set("Authorization", "Bearer x")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("authorized status = %d, want 204 for a None response", res.StatusCode)
	}
}

// Declaring roles without wiring Authorize would serve private data to
// everyone. Better a panic at startup than a quiet 200 in production.
func TestRolesWithoutAuthorizePanicAtRegistration(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registration accepted a role-protected route with no Authorize")
		}
	}()
	route := api.At("GET", "/api/admin", api.Define(api.Spec[api.None, api.None, api.None]{
		Name:    "Admin",
		Roles:   []string{"admin"},
		Handler: func(r *api.Request[api.None, api.None]) (api.None, error) { return api.None{}, nil },
	}))
	api.Register(http.NewServeMux(), api.Config{}, route)
}

// An unexpected error can name a table, a file or a host. The caller gets a
// correlation id; the log gets the truth.
func TestUnexpectedErrorsAreNotLeaked(t *testing.T) {
	route := api.At("GET", "/api/boom", api.Define(api.Spec[api.None, api.None, Reply]{
		Name: "Boom",
		Handler: func(r *api.Request[api.None, api.None]) (Reply, error) {
			return Reply{}, errors.New("dial tcp 10.0.0.5:5432: connection refused")
		},
	}))
	server := serve(t, api.Config{}, route)

	res, err := http.Get(server.URL + "/api/boom")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(res.Body).Decode(&body) //nolint:errcheck
	if strings.Contains(body["error"], "10.0.0.5") {
		t.Fatalf("the internal error reached the client: %v", body)
	}
}

// ---------------------------------------------------------------------------
// The generated client's runtime
// ---------------------------------------------------------------------------

func TestClientRoundTrip(t *testing.T) {
	server := serve(t, api.Config{}, echo)
	transport := api.NewTransport(server.URL)

	got, err := api.Call[Reply](context.Background(), transport, "GET", "/api/echo", Query{Service: "guard", Limit: 7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Service != "guard" || got.Limit != 7 {
		t.Fatalf("reply = %#v — the query struct did not survive the round trip", got)
	}
}

// The error the handler returned comes back as the same typed error, so a
// caller can read the status instead of matching on a message.
func TestClientReturnsTypedErrors(t *testing.T) {
	route := api.At("GET", "/api/missing", api.Define(api.Spec[api.None, api.None, Reply]{
		Name: "Missing",
		Handler: func(r *api.Request[api.None, api.None]) (Reply, error) {
			return Reply{}, api.NotFound("no such thing")
		},
	}))
	server := serve(t, api.Config{}, route)

	_, err := api.Call[Reply](context.Background(), api.NewTransport(server.URL), "GET", "/api/missing", nil, nil)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *api.Error", err, err)
	}
	if apiErr.Code != http.StatusNotFound || !strings.Contains(apiErr.Message, "no such thing") {
		t.Fatalf("err = %#v", apiErr)
	}
}

func TestEncodeQueryOmitsZeroValues(t *testing.T) {
	values, err := api.EncodeQuery(Query{Service: "guard"})
	if err != nil {
		t.Fatal(err)
	}
	if got := values.Encode(); got != "service=guard" {
		t.Fatalf("encoded = %q — an unset filter must not become ?limit=0", got)
	}
}

func TestPathEscapesParameters(t *testing.T) {
	if got := api.Path("/api/events/{id}", "../secrets"); got != "/api/events/..%2Fsecrets" {
		t.Fatalf("Path = %q — a parameter must not reach into another route", got)
	}
}

// ---------------------------------------------------------------------------
// OpenAPI
// ---------------------------------------------------------------------------

func TestDocumentDescribesWhatIsRegistered(t *testing.T) {
	routes := []api.Route{
		api.At("GET", "/api/echo", api.Define(api.Spec[Query, api.None, Reply]{
			Name:    "Echo",
			Handler: func(r *api.Request[Query, api.None]) (Reply, error) { return Reply{}, nil },
		})),
		api.At("POST", "/api/things/{id}", api.Define(api.Spec[api.None, Body, Created]{
			Name:    "Create Thing",
			Roles:   []string{"admin"},
			Handler: func(r *api.Request[api.None, Body]) (Created, error) { return Created{}, nil },
		})),
	}
	doc := api.Document(api.Info{Title: "Test", Version: "1.0.0"}, routes)

	paths := doc["paths"].(map[string]any)
	if len(paths) != 2 {
		t.Fatalf("paths = %d, want 2", len(paths))
	}

	echo := paths["/api/echo"].(map[string]any)["get"].(map[string]any)
	if echo["operationId"] != "getEcho" {
		t.Fatalf("operationId = %v", echo["operationId"])
	}
	// The query struct's tags are the parameter names — one definition, not a
	// hand-written list that drifts from the decoder.
	names := map[string]bool{}
	for _, p := range echo["parameters"].([]any) {
		names[p.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"service", "limit", "live", "since", "ratio"} {
		if !names[want] {
			t.Errorf("query parameter %q missing from the document", want)
		}
	}
	if names["skipped"] {
		t.Error(`a query:"-" field was documented`)
	}

	create := paths["/api/things/{id}"].(map[string]any)["post"].(map[string]any)
	params := create["parameters"].([]any)
	if len(params) != 1 || params[0].(map[string]any)["in"] != "path" {
		t.Fatalf("path parameter missing: %v", params)
	}
	if create["security"] == nil {
		t.Error("an endpoint declaring roles has no security requirement")
	}
	if create["requestBody"] == nil {
		t.Error("a POST with a body type has no requestBody")
	}

	// Named structs become components and are referenced, so a type shared by
	// several endpoints is described once.
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	if _, ok := schemas["Reply"]; !ok {
		t.Fatalf("Reply not in components: %v", schemas)
	}
	reply := schemas["Reply"].(map[string]any)["properties"].(map[string]any)
	if _, ok := reply["echo"]; !ok {
		t.Fatalf("json tag not used for the property name: %v", reply)
	}
}

// A None response is a 204 with no content, not an empty object schema.
func TestNoneResponseIs204(t *testing.T) {
	doc := api.Document(api.Info{}, []api.Route{
		api.At("DELETE", "/api/things/{id}", api.Define(api.Spec[api.None, api.None, api.None]{
			Name:    "Delete Thing",
			Handler: func(r *api.Request[api.None, api.None]) (api.None, error) { return api.None{}, nil },
		})),
	})
	op := doc["paths"].(map[string]any)["/api/things/{id}"].(map[string]any)["delete"].(map[string]any)
	responses := op["responses"].(map[string]any)
	if _, ok := responses["204"]; !ok {
		t.Fatalf("responses = %v, want a 204", responses)
	}
	if _, ok := responses["200"]; ok {
		t.Error("a None response was documented as 200 with a body")
	}
}

func TestSpecAndDocsAreServed(t *testing.T) {
	mux := http.NewServeMux()
	routes := []api.Route{api.At("GET", "/api/echo", api.Define(api.Spec[Query, api.None, Reply]{
		Name: "Echo", Handler: func(r *api.Request[Query, api.None]) (Reply, error) { return Reply{}, nil },
	}))}
	mux.HandleFunc("GET /api/openapi.json", api.OpenAPI(api.Info{Title: "Test"}, routes...))
	mux.HandleFunc("GET /api/docs", api.Docs("/api/openapi.json"))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	res, err := http.Get(server.URL + "/api/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var doc map[string]any
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatalf("the spec is not JSON: %v", err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %v", doc["openapi"])
	}

	page, err := http.Get(server.URL + "/api/docs")
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	body, _ := io.ReadAll(page.Body)
	if !strings.Contains(string(body), "/api/openapi.json") {
		t.Fatal("the docs page does not point at the spec it was given")
	}
}
