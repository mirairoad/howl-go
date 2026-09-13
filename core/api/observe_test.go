package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mirairoad/howl-go/core/api"
	"github.com/mirairoad/howl-go/core/observe"
)

// One span per endpoint call, ended with the stage a failure happened at, and
// the request span told the route by path alone — the tracer adds the method.
func TestEndpointSpanRecordsTheStageAndTheRoute(t *testing.T) {
	rec := &observe.Recorder{}
	observe.SetDefault(rec)
	t.Cleanup(func() { observe.SetDefault(nil) })

	route := api.At("GET", "/api/echo", api.Define(api.Spec[Query, api.None, Reply]{
		Name: "Echo",
		Handler: func(r *api.Request[Query, api.None]) (Reply, error) {
			if r.Query.Limit == 7 {
				return Reply{}, api.NotFound("no seven")
			}
			return Reply{Limit: r.Query.Limit}, nil
		},
	}))
	server := serve(t, api.Config{}, route)

	for _, tc := range []struct{ query, stage string }{
		{"?limit=1", ""},          // success: no stage
		{"?limit=-1", "validate"}, // Validate refuses a negative limit
		{"?limit=7", "handler"},   // the handler's own error
	} {
		res, err := http.Get(server.URL + "/api/echo" + tc.query)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}

	calls := rec.Named("api Echo")
	if len(calls) != 3 {
		t.Fatalf("%d endpoint spans for three calls", len(calls))
	}
	if calls[0].Err != nil || calls[0].Attrs["howl.api.stage"] != nil || calls[0].Attrs[observe.Kind] != "api" {
		t.Errorf("success: %+v", calls[0])
	}
	if calls[1].Attrs["howl.api.stage"] != "validate" || calls[1].Err == nil {
		t.Errorf("validation failure: %+v", calls[1])
	}
	if calls[2].Attrs["howl.api.stage"] != "handler" || calls[2].Err == nil {
		t.Errorf("handler failure: %+v", calls[2])
	}
	for _, c := range calls {
		if !c.Ended || c.Attrs["howl.endpoint"] != "Echo" {
			t.Errorf("span not ended or unnamed: %+v", c)
		}
	}
}

// What the caller sent reaches the span, filtered: the field names always,
// the values only when they cannot be personal data. It is recorded before
// Validate runs, because a call refused for being wrong is exactly the one
// whose arguments you want to see.
func TestEndpointRecordsItsArgumentsRedacted(t *testing.T) {
	rec := &observe.Recorder{}
	observe.SetDefault(rec)
	t.Cleanup(func() { observe.SetDefault(nil) })

	route := api.At("POST", "/api/signup", api.Define(api.Spec[Query, Signup, Created]{
		Name: "Signup",
		Handler: func(r *api.Request[Query, Signup]) (Created, error) {
			return Created{ID: "0193a5c2-5f4e-7b3a-9c1d-2e6f8a0b4c7d"}, nil
		},
	}))
	server := serve(t, api.Config{}, route)

	body := strings.NewReader(`{"email":"ada@example.com","password":"hunter2","org":"0193a5c2-5f4e-7b3a-9c1d-2e6f8a0b4c7d","note":"let me in"}`)
	res, err := http.Post(server.URL+"/api/signup?limit=5", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	span := rec.Named("api Signup")[0]
	got, _ := span.Attrs["howl.api.body"].(string)
	if !strings.Contains(got, `"email":"?"`) || !strings.Contains(got, `"password":"?"`) || !strings.Contains(got, `"note":"?"`) {
		t.Errorf("values were not redacted: %s", got)
	}
	if !strings.Contains(got, `"org":"0193a5c2-5f4e-7b3a-9c1d-2e6f8a0b4c7d"`) {
		t.Errorf("the identifier was dropped: %s", got)
	}
	for _, leak := range []string{"ada@example.com", "example.com", "hunter2", "let me in"} {
		if strings.Contains(got, leak) {
			t.Fatalf("the body leaked %q: %s", leak, got)
		}
	}
	// A query struct is tagged for the URL, not for JSON, so its span text
	// carries the Go field names. Legible, and the alternative is a second
	// reflection pass to re-derive names the caller never sees anyway.
	if q, _ := span.Attrs["howl.api.query"].(string); !strings.Contains(q, `"Limit":5`) {
		t.Errorf("query: %s", q)
	}
}

// An endpoint that declares neither carries neither: an empty object on every
// span is noise on a busy service.
func TestEndpointWithNoArgumentsRecordsNone(t *testing.T) {
	rec := &observe.Recorder{}
	observe.SetDefault(rec)
	t.Cleanup(func() { observe.SetDefault(nil) })

	route := api.At("GET", "/api/ping", api.Define(api.Spec[api.None, api.None, Created]{
		Name:    "Ping",
		Handler: func(r *api.Request[api.None, api.None]) (Created, error) { return Created{}, nil },
	}))
	server := serve(t, api.Config{}, route)
	res, err := http.Get(server.URL + "/api/ping")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	span := rec.Named("api Ping")[0]
	if _, ok := span.Attrs["howl.api.query"]; ok {
		t.Error("an api.None query was rendered")
	}
	if _, ok := span.Attrs["howl.api.body"]; ok {
		t.Error("an api.None body was rendered")
	}
}

type Signup struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Org      string `json:"org"`
	Note     string `json:"note"`
}
