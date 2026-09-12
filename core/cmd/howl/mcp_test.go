package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A section with subheadings must come back whole. Breaking at the first
// heading of any depth returned "Making a page interactive" as its intro and
// none of the rules — 870 bytes of 7800 — which is worse than returning
// nothing, because the answer looks complete.
func TestConventionsKeepsSubsections(t *testing.T) {
	got := conventions("Making a page interactive")
	for _, want := range []string{
		"### The store: two files, two different rules",
		"### The three wires, in the order they run",
		"### The lifecycle contract",
		"### What does not exist",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("section is missing %q", want)
		}
	}
	// ...and must still stop at the next section of the same level.
	if strings.Contains(got, "\n## Client render safety") {
		t.Error("section ran past its sibling heading")
	}
}

func TestConventionsSubsectionOnItsOwn(t *testing.T) {
	got := conventions("The lifecycle contract")
	if !strings.HasPrefix(strings.TrimSpace(got), "### The lifecycle contract") {
		t.Fatalf("wrong section:\n%.120s", got)
	}
	if strings.Contains(got, "### Overlays") {
		t.Error("a subsection must stop at its next sibling")
	}
}

func TestConventionsNativeWindow(t *testing.T) {
	got := conventions("Native window")
	for _, want := range []string{"desktop.Run", "Attach", "a.Serve", "go:embed"} {
		if !strings.Contains(got, want) {
			t.Errorf("Native window section is missing %q", want)
		}
	}
}

func TestConventionsUnknownSection(t *testing.T) {
	if got := conventions("no such thing"); !strings.HasPrefix(got, "no section matching") {
		t.Errorf("got %q", got)
	}
}

// Every frontend topic has the same five parts, and that shape is the point:
// an agent that reads "Rules" without "Wrong" writes the wrong thing with
// confidence. The test is what keeps a topic from being added without it.
func TestFrontendTopicsAreStructured(t *testing.T) {
	topics := []string{"checklist", "page", "store", "events", "list", "form", "modal", "filter", "navigation", "animation", "islands"}
	for _, topic := range topics {
		got := frontend(topic)
		if !strings.HasPrefix(strings.TrimSpace(got), "## "+topic) {
			t.Errorf("%s: wrong section:\n%.80s", topic, got)
			continue
		}
		for _, part := range []string{"**When**", "**Rules**", "**Wrong**", "**Check**"} {
			if !strings.Contains(got, part) {
				t.Errorf("%s is missing %s", topic, part)
			}
		}
		if topic != "checklist" && topic != "navigation" && topic != "islands" && !strings.Contains(got, "**Example**") {
			t.Errorf("%s is missing an example", topic)
		}
		if strings.Count(got, "\n## ") > 0 {
			t.Errorf("%s ran into the next topic", topic)
		}
	}
	// The two that models get wrong most carry the rule that fixes it.
	if got := frontend("modal"); !strings.Contains(got, "one signal") || !strings.Contains(got, "showModal") {
		t.Error("the modal recipe must state the one-signal rule and name showModal as the wrong turn")
	}
	if got := frontend("store"); !strings.Contains(got, "dom.Embedded") || !strings.Contains(got, "templ.JSONScript") {
		t.Error("the store recipe must show the embedded snapshot on both sides")
	}
}

// The JSON view cuts at the bold markers, so every field of every topic is
// filled — a client rendering "Wrong" beside "Example" never gets an empty pane.
func TestFrontendJSONHasEveryPart(t *testing.T) {
	out, err := frontendJSON("modal")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"when": "`, `"rules": "`, `"example": "`, `"wrong": "`, `"check": "`} {
		if !strings.Contains(out, key) || strings.Contains(out, key+`"`) {
			t.Errorf("modal JSON is missing or empty at %s", key)
		}
	}
	if !strings.Contains(out, "editing") {
		t.Error("the modal example should reach the JSON view")
	}
	if _, err := frontendJSON("hooks"); err == nil {
		t.Error("an unknown topic must be an error in JSON mode, not an empty object")
	}
	all, err := frontendJSON("")
	if err != nil || strings.Count(all, `"topic": "`) != len(frontendTopics) {
		t.Errorf("all topics: err=%v count=%d", err, strings.Count(all, `"topic": "`))
	}
}

func TestFrontendUnknownTopic(t *testing.T) {
	if got := frontend("hooks"); !strings.HasPrefix(got, "no section matching") || !strings.Contains(got, "modal") {
		t.Errorf("an unknown topic should list the real ones, got %q", got)
	}
}

// A '#' at the start of a line inside a code fence is a shell comment, not an
// h1, and must not end the section it sits in.
func TestHeadingLevelIgnoresFences(t *testing.T) {
	if got := headingLevel("# rebuild everything", true); got != 0 {
		t.Errorf("fenced line read as a level-%d heading", got)
	}
	if got := headingLevel("### Layouts", false); got != 3 {
		t.Errorf("got level %d, want 3", got)
	}
	if got := headingLevel("#not-a-heading", false); got != 0 {
		t.Errorf("got level %d, want 0", got)
	}
}

// Every generated line must come back, whatever fields it carries. The reader
// was one regexp that could not cross the } of a Layouts slice, so routes with
// a layout — a third of the toy app — were missing from howl_routes.
func TestRoutesReadsEveryGeneratedLine(t *testing.T) {
	root := t.TempDir()
	gen := `package pages

func FsClientRoutes() []router.Route {
	return []router.Route{
		{Pattern: "/", Label: "Home", Page: Page, Head: nil, Mount: nil, Unmount: nil, Layouts: nil, Client: false, Raw: false},
		{Pattern: "/dashboard/metrics", Label: "Metrics", Page: p3.Page, Head: p3.Head, Mount: p3.Mount, Unmount: nil, Layouts: []router.Wrapper{p2.Layout}, Client: true, Raw: false, Data: "/api/metrics"},
		{Pattern: "/pricing", Label: "Pricing", Page: p4.Page, Head: nil, Mount: nil, Unmount: nil, Layouts: []router.Wrapper{Layout, p4.Layout}, Client: false, Raw: false, Cache: 30000000000 /* 30s */},
	}
}
`
	if err := os.WriteFile(filepath.Join(root, "fsroutes_gen.go"), []byte(gen), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(readPageRoutes(root))
	var got struct{ Routes []pageRoute }
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Routes) != 3 {
		t.Fatalf("read %d routes, want 3: %s", len(got.Routes), raw)
	}
	metrics, pricing := got.Routes[1], got.Routes[2]
	if !metrics.Client || !metrics.Mount || metrics.Data != "/api/metrics" {
		t.Errorf("metrics = %+v", metrics)
	}
	if pricing.Cache != "30s" || pricing.Mount {
		t.Errorf("pricing = %+v, want cache 30s and no mount", pricing)
	}
}

// howl_endpoints is how an agent learns what an endpoint declares before
// editing it — a limit it cannot see is a limit it will write a second time.
func TestEndpointsReadsWhatTheSpecDeclares(t *testing.T) {
	root := t.TempDir()
	api := `package apis

var SignIn = api.Define(api.Spec[api.None, Credentials, Session]{
	Name:        "Sign In",
	Description: "Exchange credentials for a session cookie.",
	Roles:       []string{"admin", "user"},
	Cache:       api.Cache{TTL: 5 * time.Second},
	Limit: api.Limit{
		Requests: 5,
		Window:   time.Minute,
	},
	Handler: func(r *api.Request[api.None, Credentials]) (Session, error) { return Session{}, nil },
})
`
	gen := `package apis

func FsApiRoutes() []api.Route {
	return []api.Route{
		api.At("POST", "/api/sign-in", SignIn),
	}
}
`
	if err := os.WriteFile(filepath.Join(root, "sign-in.post.api.go"), []byte(api), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "apis_gen.go"), []byte(gen), 0o644); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(readEndpoints(root))
	var got struct{ Endpoints []endpointInfo }
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Endpoints) != 1 {
		t.Fatalf("read %d endpoints, want 1: %s", len(got.Endpoints), raw)
	}
	e := got.Endpoints[0]
	if e.Method != "POST" || e.Path != "/api/sign-in" || e.Name != "Sign In" {
		t.Errorf("endpoint = %+v", e)
	}
	// Written over two lines, reported on one.
	if e.Limit != "Requests: 5, Window: time.Minute" {
		t.Errorf("limit = %q", e.Limit)
	}
	if e.Cache != "5 * time.Second" || len(e.Roles) != 2 {
		t.Errorf("cache = %q, roles = %v", e.Cache, e.Roles)
	}
}
