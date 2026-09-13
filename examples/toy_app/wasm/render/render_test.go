package render

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mirairoad/howl-go/core/router"
	"github.com/mirairoad/howl-go/examples/toy_app/boot"
	"github.com/mirairoad/howl-go/examples/toy_app/client/pages"
	"github.com/mirairoad/howl-go/examples/toy_app/client/store"
)

// Every .client route is rendered twice: once from the context the server
// builds for a request, and once from the payload the browser would hand the
// wasm renderer for the same route. The two must be byte-identical — and the
// second must differ from a render with nothing in the payload, or the route
// shows something that never crosses the wire and will be blank after the
// first local navigation.
//
// The payload is assembled the way app.js assembles it: the endpoint comes from
// the howl-client JSON in a cold document (the route's own `pages` entry, else
// the shared `data`), and bootstrap is what Config.Bootstrap returned.
func TestClientRoutesRenderTheSameLocallyAsOnTheServer(t *testing.T) {
	routes := pages.FsClientRoutes()
	_, h := boot.New()
	client := clientConfig(t, h)

	tested := 0
	for _, rt := range routes {
		if !rt.Client {
			continue
		}
		if strings.Contains(rt.Pattern, "{") {
			t.Fatalf("%s: a dynamic .client route needs a concrete path here; add one", rt.Pattern)
		}
		path := rt.Pattern
		tested++

		// The server's side: the same three router values the wasm renderer
		// installs, then Config.Data, then the fragment the X-Partial branch writes.
		ctx := router.WithParams(router.WithCurrent(router.WithRoutes(context.Background(), routes), path), nil)
		ctx = boot.Data(ctx, path)
		want, _, err := Fragment(rt, ctx)
		if err != nil {
			t.Fatalf("%s: server render: %v", path, err)
		}

		// The browser's side: bootstrap from the same request — RenderedAt is a
		// timestamp, so it has to be — and route data from the endpoint app.js
		// would resolve for this path.
		bootstrap, err := json.Marshal(store.MetaFrom(ctx))
		if err != nil {
			t.Fatal(err)
		}
		endpoint := client.Data
		if own, ok := client.Pages[rt.Pattern]; ok {
			endpoint = own
		}
		payload := router.RenderPayload{Bootstrap: bootstrap, RouteData: get(t, h, endpoint)}
		got, ok, err := Page(routes, path, payload)
		if !ok || err != nil {
			t.Fatalf("%s: local render: ok=%v err=%v", path, ok, err)
		}
		if got != want {
			t.Errorf("%s: the browser renders something other than the server did\n--- server\n%s\n--- browser\n%s", path, want, got)
		}

		// The zero-value guard: nothing installed, nothing shown. If this render
		// equals the real one, the route's content is not coming from the payload.
		empty, _, err := Page(routes, path, router.RenderPayload{
			Bootstrap: json.RawMessage("{}"), RouteData: json.RawMessage("{}"),
		})
		if err != nil {
			t.Fatalf("%s: render with an empty payload: %v", path, err)
		}
		if empty == got {
			t.Errorf("%s: renders identically with an empty payload — its data is not crossing the wire", path)
		}
	}
	if tested == 0 {
		t.Fatal("no .client route in the table; this test exists for them")
	}
}

// A route the wasm build does not own is declined, not rendered: the client
// then asks the server, which is the only party that can 404 it.
func TestNonClientRouteIsDeclined(t *testing.T) {
	routes := pages.FsClientRoutes()
	for _, path := range []string{"/", "/todos", "/no/such/route"} {
		html, ok, err := Page(routes, path, router.RenderPayload{})
		if ok || err != nil || html != "" {
			t.Errorf("%s: ok=%v err=%v html=%q; want declined", path, ok, err, html)
		}
	}
}

// clientConfig is the howl-client JSON from a cold document, which is what
// app.js reads at boot to know each route's data endpoint.
func clientConfig(t *testing.T, h http.Handler) router.Client {
	t.Helper()
	doc := string(get(t, h, "/"))
	const open = `<script id="howl-client" type="application/json">`
	start := strings.Index(doc, open)
	if start < 0 {
		t.Fatal("no howl-client JSON in the cold document")
	}
	raw := doc[start+len(open):]
	raw = raw[:strings.Index(raw, "</script>")]
	var c router.Client
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("howl-client: %v", err)
	}
	return c
}

func get(t *testing.T, h http.Handler, path string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d", path, rec.Code)
	}
	b, _ := io.ReadAll(rec.Body)
	return b
}
