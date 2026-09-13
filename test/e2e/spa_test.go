// The SPA path with nothing stubbed: the real views.wasm in a real browser
// against the real application. What this proves that nothing else can is
// that the GOOS=js half boots, renders, and does so without the server — the
// claim llms.txt makes as "0 bytes, server not contacted" is an assertion here.
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/mirairoad/howl-go/examples/toy_app/boot"
)

// The whole local render, end to end: the binary loads, a click into a
// .client route is drawn by it, and the server sees no request for it.
func TestClientRouteIsRenderedByTheBrowserAlone(t *testing.T) {
	app := start(t)
	ctx, cancel := browser(t)
	defer cancel()

	must(t, ctx, chromedp.Navigate(app.URL+"/dashboard"))
	waitReady(t, ctx)
	assertActiveTabs(t, ctx, "/dashboard") // the cold paint, after markActive ran at boot

	rec := record(ctx, app.URL)
	must(t, ctx,
		chromedp.Click(`a[href="/dashboard/metrics"]`),
		chromedp.Poll(`document.getElementById("outlet").dataset.route === "/dashboard/metrics"`, nil),
	)

	var rows int
	var title string
	must(t, ctx,
		chromedp.Evaluate(`document.querySelectorAll("[data-rows] tr").length`, &rows),
		chromedp.Title(&title),
	)
	if rows != 8 {
		t.Errorf("metrics table: %d rows, want 8 — the route data did not reach the local render", rows)
	}
	if title != "Metrics-and-more" {
		t.Errorf("title %q: the wasm head template was not merged", title)
	}
	// One tab lit, the right one — after app.js's repaint, which once turned
	// the server's single active tab into two because it matched by prefix
	// where ui.Tab matched exactly.
	assertActiveTabs(t, ctx, "/dashboard/metrics")
	// The page's own Mount fetches /api/metrics from Go to demonstrate the
	// typed client; that is the application's choice, not the router's. What
	// must be zero is markup: no fragment, no document, no binary.
	if got := rec.markup(); len(got) != 0 {
		t.Errorf("the server was asked for markup during a local navigation:\n%s", strings.Join(got, "\n"))
	}

	// And back, to a route with a different data shape on screen.
	must(t, ctx,
		chromedp.Click(`a[href="/dashboard"]`),
		chromedp.Poll(`document.getElementById("outlet").dataset.route === "/dashboard"`, nil),
	)
	assertActiveTabs(t, ctx, "/dashboard")
	var cards int
	must(t, ctx, chromedp.Evaluate(`document.querySelectorAll("#outlet article.card").length`, &cards))
	if cards == 0 {
		var html string
		must(t, ctx, chromedp.InnerHTML("#outlet", &html))
		t.Errorf("overview rendered no stat cards locally:\n%s", html)
	}
	if got := rec.markup(); len(got) != 0 {
		t.Errorf("the server was asked for markup going back to the overview:\n%s", strings.Join(got, "\n"))
	}
}

// One component, two renderers, one result. The binary's howlRender is called
// from the page with the payload app.js would build, and its output is held
// against the server's X-Partial response for the same route, byte for byte —
// head template included. Nothing is normalised: this is the wire, twice.
func TestLocalRenderMatchesTheServerFragment(t *testing.T) {
	app := start(t)
	ctx, cancel := browser(t)
	defer cancel()

	must(t, ctx, chromedp.Navigate(app.URL+"/dashboard"))
	waitReady(t, ctx)

	for _, path := range []string{"/dashboard/metrics", "/dashboard"} {
		want := app.fragment(t, path)
		var got string
		must(t, ctx, chromedp.Evaluate(`(async () => {
			const routeData = await (await fetch("/api/metrics")).json();
			return howlRender(`+jsString(path)+`, JSON.stringify({ bootstrap: howl.config.bootstrap, routeData }));
		})()`, &got, awaitPromise))
		if got != want {
			t.Errorf("%s: the browser and the server rendered different markup\n--- server\n%s\n--- browser\n%s", path, want, got)
		}
	}
}

// Mount runs once per visit — not zero (nothing wired) and not twice (a
// leak that doubles every effect). /dashboard/metrics logs when it mounts.
func TestMountRunsOncePerVisitIncludingHistory(t *testing.T) {
	app := start(t)
	ctx, cancel := browser(t)
	defer cancel()

	var mu sync.Mutex
	mounted := 0
	chromedp.ListenTarget(ctx, func(ev any) {
		if e, ok := ev.(*runtime.EventConsoleAPICalled); ok {
			for _, a := range e.Args {
				if strings.Contains(string(a.Value), "[metrics] mounted") {
					mu.Lock()
					mounted++
					mu.Unlock()
				}
			}
		}
	})
	must(t, ctx, runtime.Enable(), chromedp.Navigate(app.URL+"/dashboard"))
	waitReady(t, ctx)

	go_ := func(path string) {
		must(t, ctx,
			chromedp.Click(`a[href="`+path+`"]`),
			chromedp.Poll(`document.getElementById("outlet").dataset.route === "`+path+`"`, nil),
		)
	}
	go_("/dashboard/metrics")
	go_("/dashboard")
	go_("/dashboard/metrics")
	// history: metrics -> dashboard -> metrics; back lands on /dashboard, back
	// again on /dashboard/metrics.
	must(t, ctx,
		chromedp.NavigateBack(),
		chromedp.Poll(`document.getElementById("outlet").dataset.route === "/dashboard"`, nil),
		chromedp.NavigateBack(),
		chromedp.Poll(`document.getElementById("outlet").dataset.route === "/dashboard/metrics"`, nil),
	)
	// The console event trails the DOM change; wait for the third, then make
	// sure a fourth does not follow it.
	count := func() int { mu.Lock(); defer mu.Unlock(); return mounted }
	for deadline := time.Now().Add(3 * time.Second); count() < 3 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if got := count(); got != 3 {
		t.Errorf("Mount ran %d times for three visits to /dashboard/metrics", got)
	}
}

// ---------------------------------------------------------------------------

type served struct {
	*httptest.Server
}

// start serves the toy app in-process and refuses to run against a build that
// has no wasm: the embed happens at compile time, so a missing binary would
// otherwise surface as every test failing at the readiness poll.
func start(t *testing.T) served {
	t.Helper()
	_, h := boot.New()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	app := served{srv}

	doc := app.get(t, "/dashboard")
	binary := clientField(t, doc, "binary")
	if res, err := http.Get(srv.URL + binary); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("%s is not served: build it first — make -C examples/toy_app wasm (or make test-e2e from the root)", binary)
	}
	return app
}

func (s served) get(t *testing.T, path string) string {
	t.Helper()
	res, err := http.Get(s.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d", path, res.StatusCode)
	}
	return string(b)
}

// fragment is what app.js would have fetched for path: the X-Partial body,
// head template first, then the page plus its layouts.
func (s served) fragment(t *testing.T, path string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, s.URL+path, nil)
	req.Header.Set("X-Partial", "1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func clientField(t *testing.T, doc, field string) string {
	t.Helper()
	const open = `<script id="howl-client" type="application/json">`
	i := strings.Index(doc, open)
	if i < 0 {
		t.Fatal("no howl-client JSON in the document")
	}
	raw := doc[i+len(open):]
	raw = raw[:strings.Index(raw, "</script>")]
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	var v string
	if err := json.Unmarshal(m[field], &v); err != nil {
		t.Fatalf("howl-client.%s: %v", field, err)
	}
	return v
}

// browser is a headless Chromium. chromedp finds the usual binaries on its
// own; HOWL_CHROME names one when it cannot. NoSandbox is for CI containers.
func browser(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts, chromedp.NoSandbox)
	if p := os.Getenv("HOWL_CHROME"); p != "" {
		opts = append(opts, chromedp.ExecPath(p))
	}
	actx, acancel := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel := chromedp.NewContext(actx)
	ctx, tcancel := context.WithTimeout(ctx, 2*time.Minute)
	return ctx, func() { tcancel(); cancel(); acancel() }
}

// waitReady returns once app.js will render locally: the binary's main() has
// installed howlRender, and the shared data endpoint it fetches before
// declaring itself ready has completed. Compiling 8 MB of Go is the slow part,
// and it happens at idle after the cold paint — hence the generous timeout.
func waitReady(t *testing.T, ctx context.Context) {
	t.Helper()
	must(t, ctx, chromedp.Poll(
		`typeof howlRender === "function" && performance.getEntriesByType("resource").some((e) => e.name.endsWith("/api/metrics"))`,
		nil, chromedp.WithPollingTimeout(90*time.Second)))
}

// recorder collects every same-origin request the page makes from the moment
// it is created — the definition of "the server was not contacted".
type recorder struct {
	mu     sync.Mutex
	origin string
	seen   []string
}

func record(ctx context.Context, origin string) *recorder {
	r := &recorder{origin: origin}
	chromedp.ListenTarget(ctx, func(ev any) {
		if e, ok := ev.(*network.EventRequestWillBeSent); ok && strings.HasPrefix(e.Request.URL, origin) {
			line := e.Request.Method + " " + strings.TrimPrefix(e.Request.URL, origin)
			if _, partial := e.Request.Headers["X-Partial"]; partial {
				line += " (fragment)"
			}
			r.mu.Lock()
			r.seen = append(r.seen, line)
			r.mu.Unlock()
		}
	})
	_ = chromedp.Run(ctx, network.Enable())
	return r
}

// markup is every request since the last call that asked for something other
// than data: a fragment, a document, a static asset. An application's own
// fetch of an /api/ endpoint is its business and is not counted.
func (r *recorder) markup() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, line := range r.seen {
		path := strings.Fields(line)[1]
		if !strings.HasPrefix(path, "/api/") {
			out = append(out, line)
		}
	}
	r.seen = nil
	return out
}

// awaitPromise makes Evaluate wait for an async expression's result.
func awaitPromise(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }

func must(t *testing.T, ctx context.Context, actions ...chromedp.Action) {
	t.Helper()
	if err := chromedp.Run(ctx, actions...); err != nil {
		t.Fatal(err)
	}
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// assertActiveTabs checks the tab bar has exactly one active tab and it is
// the one for path. Tabs are data-active="exact" links, so this is the
// client's rule and the server's agreeing.
func assertActiveTabs(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	var active []string
	must(t, ctx, chromedp.Evaluate(`[...document.querySelectorAll("nav.tabs a.active")].map((a) => a.getAttribute("href"))`, &active))
	if len(active) != 1 || active[0] != path {
		t.Errorf("active tabs %v, want exactly [%s]", active, path)
	}
}
