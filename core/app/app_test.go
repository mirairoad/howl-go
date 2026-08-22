package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/a-h/templ"

	"github.com/mirairoad/howl-go/core/router"
)

func comp(fn func(ctx context.Context, w io.Writer) error) templ.Component {
	return templ.ComponentFunc(fn)
}

func text(s string) templ.Component {
	return comp(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, s)
		return err
	})
}

func shell(title, head string) templ.Component {
	return comp(func(ctx context.Context, w io.Writer) error {
		io.WriteString(w, "<html><head><title>"+title+"</title>"+head+"</head><body>") //nolint:errcheck
		if err := templ.GetChildren(ctx).Render(ctx, w); err != nil {
			return err
		}
		_, err := io.WriteString(w, "</body></html>")
		return err
	})
}

func testApp(routes ...router.Route) *App {
	return New(Config{
		Routes:   routes,
		Shell:    shell,
		NotFound: func(p string) templ.Component { return text("<p>no such page</p>") },
		Public:   fstest.MapFS{"app.css": {Data: []byte(strings.Repeat("body{color:red}", 100))}},
	})
}

func get(h http.Handler, target string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", target, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// A page that discovers its record does not exist renders its own markup and
// says so in the status. Without SetStatus this is a soft 404: "not found"
// text served with 200, invisible to crawlers and uptime checks alike.
func TestPageCanAnswer404(t *testing.T) {
	rt := router.Route{
		Pattern: "/blog/{id}",
		Label:   "Post",
		Page: func() templ.Component {
			return comp(func(ctx context.Context, w io.Writer) error {
				if router.Param(ctx, "id") == "nope" {
					router.NotFound(ctx)
					return nil
				}
				_, err := io.WriteString(w, "<article>post</article>")
				return err
			})
		},
	}
	mux := testApp(rt).Mux()

	if rec := get(mux, "/blog/real", nil); rec.Code != 200 {
		t.Fatalf("existing post status = %d", rec.Code)
	}
	rec := get(mux, "/blog/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing post status = %d, want 404", rec.Code)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	rec := get(testApp().Mux(), "/nothing/here", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no such page") {
		t.Fatalf("the application's NotFound component did not render: %q", rec.Body.String())
	}
}

func TestFragmentCarriesItsHead(t *testing.T) {
	rt := router.Route{
		Pattern: "/about",
		Label:   "About",
		Page:    func() templ.Component { return text("<p>about</p>") },
		Head:    func() templ.Component { return text("<title>About — howl</title>") },
	}
	rec := get(testApp(rt).Mux(), "/about", map[string]string{"X-Partial": "1"})

	body := rec.Body.String()
	if !strings.HasPrefix(body, "<template data-head><title>About — howl</title>") {
		t.Fatalf("fragment head missing: %q", body)
	}
	if strings.Contains(body, "<html") {
		t.Fatal("a fragment must not carry the document shell")
	}
	// Percent-encoded: fetch() decodes headers as ISO-8859-1, so a raw em dash
	// would reach the client as mojibake.
	if got := rec.Header().Get("X-Title"); !strings.Contains(got, "%E2%80%94") {
		t.Fatalf("X-Title = %q, want it percent-encoded", got)
	}
	if rec.Header().Get("Vary") != "X-Partial" {
		t.Fatal("Vary: X-Partial missing — a cache would serve a fragment as a document")
	}
}

func TestRawRouteSkipsTheShell(t *testing.T) {
	rt := router.Route{
		Pattern: "/embed",
		Label:   "Embed",
		Raw:     true,
		Page:    func() templ.Component { return text("<div>widget</div>") },
	}
	rec := get(testApp(rt).Mux(), "/embed", nil)

	if got := rec.Body.String(); got != "<div>widget</div>" {
		t.Fatalf("body = %q, want the page markup and nothing else", got)
	}
}

// Buffering the render is what makes this possible: a component that fails
// halfway must not leave a truncated document on the wire with a 200 on it.
func TestRenderErrorDoesNotLeakAPartialPage(t *testing.T) {
	rt := router.Route{
		Pattern: "/broken",
		Label:   "Broken",
		Page: func() templ.Component {
			return comp(func(_ context.Context, w io.Writer) error {
				io.WriteString(w, "<p>half a page") //nolint:errcheck
				return errors.New("data source exploded")
			})
		},
	}
	rec := get(testApp(rt).Mux(), "/broken", nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "half a page") {
		t.Fatal("the partial render reached the client")
	}
}

func TestOnErrorOwnsTheResponse(t *testing.T) {
	a := New(Config{
		Shell: shell,
		Routes: []router.Route{{
			Pattern: "/broken",
			Label:   "Broken",
			Page: func() templ.Component {
				return comp(func(context.Context, io.Writer) error { return errors.New("nope") })
			},
		}},
		OnError: func(w http.ResponseWriter, r *http.Request, err error) {
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, "custom error page") //nolint:errcheck
		},
	})
	rec := get(a.Mux(), "/broken", nil)

	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "custom error page") {
		t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Static
// ---------------------------------------------------------------------------

func TestStaticRevalidatesWithETag(t *testing.T) {
	mux := testApp().Mux()

	rec := get(mux, "/static/app.css", nil)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, no-cache" {
		t.Fatalf("Cache-Control = %q", cc)
	}

	again := get(mux, "/static/app.css", map[string]string{"If-None-Match": etag})
	if again.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", again.Code)
	}
	if again.Body.Len() != 0 {
		t.Fatal("304 came with a body")
	}
}

func TestStaticServesGzipWithItsOwnETag(t *testing.T) {
	mux := testApp().Mux()

	plain := get(mux, "/static/app.css", nil)
	zipped := get(mux, "/static/app.css", map[string]string{"Accept-Encoding": "gzip"})

	if zipped.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("not compressed")
	}
	if zipped.Body.Len() >= plain.Body.Len() {
		t.Fatalf("gzip %d bytes vs plain %d", zipped.Body.Len(), plain.Body.Len())
	}
	if plain.Header().Get("ETag") == zipped.Header().Get("ETag") {
		t.Fatal("both representations share an ETag — a cache could serve gzip to a client that asked for plain")
	}
}

func TestStaticServesTheFrameworkRuntime(t *testing.T) {
	rec := get(testApp().Mux(), "/static/app.js", nil)
	if rec.Code != 200 {
		t.Fatalf("app.js status = %d — the client runtime must be served without being copied into the app", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestHashedNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"app.9f8c2a1b.css", true},
		{"vendor.deadbeefcafe.js", true},
		{"app.css", false},
		{"app.min.css", false},
	} {
		if got := Hashed(tc.name); got != tc.want {
			t.Errorf("Hashed(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMountStripsThePrefix(t *testing.T) {
	sub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "sub sees "+r.URL.Path) //nolint:errcheck
	})
	a := testApp()
	a.Mount("/admin", sub)

	rec := get(a.Mux(), "/admin/users", nil)
	if got := rec.Body.String(); got != "sub sees /users" {
		t.Fatalf("body = %q", got)
	}
}

// Reload means "notice when a file changes", not "redo the work every time".
// It cost 530 ms per request on a 6.94 MB wasm binary before this: every hit,
// including the 304s, re-read the file, re-hashed it and re-compressed it.
func TestReloadRebuildsOnlyWhenTheFileChanges(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "app.css")
	if err := os.WriteFile(name, []byte(strings.Repeat("a{color:red}", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	static := &Static{FS: os.DirFS(dir), Reload: true}

	first, err := static.load("app.css")
	if err != nil {
		t.Fatal(err)
	}
	again, err := static.load("app.css")
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatal("an unchanged file was rebuilt; Reload is not a licence to redo the work")
	}

	// A changed file must still be picked up — that is what Reload is for.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(name, []byte(strings.Repeat("a{color:blue}", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := static.load("app.css")
	if err != nil {
		t.Fatal(err)
	}
	if changed == first || !strings.Contains(string(changed.raw), "blue") {
		t.Fatal("a changed file was served from the cache")
	}
}

func TestWarmCompressesEverythingUpFront(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.css"), []byte(strings.Repeat("a{color:red}", 200)), 0o644); err != nil {
		t.Fatal(err)
	}
	static := &Static{FS: os.DirFS(dir)}

	files, bytes := static.Warm()
	if files == 0 || bytes == 0 {
		t.Fatalf("warm reported %d files, %d bytes", files, bytes)
	}
	static.mu.RLock()
	entry, cached := static.cache["app.css"]
	static.mu.RUnlock()
	if !cached || entry.gz == nil {
		t.Fatal("warm did not leave a compressed entry behind, so the first request still pays for it")
	}
}

// A hashed URL can promise a year because the name changes with the content.
// The alternative is what guard was doing: a conditional request per asset per
// page load, answering 304 with an empty body — a round-trip to be told nothing
// happened.
func TestHashedURLsAreImmutable(t *testing.T) {
	a := testApp()
	mux := a.Mux()

	hashed := a.asset("app.css")
	if !strings.HasPrefix(hashed, "/static/app.") || !strings.HasSuffix(hashed, ".css") {
		t.Fatalf("asset URL = %q, want a content hash in the name", hashed)
	}

	rec := get(mux, hashed, map[string]string{"Accept-Encoding": "gzip"})
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control = %q", cc)
	}

	// The same bytes under the plain name must still revalidate: nothing about
	// that URL says which version it is.
	plain := get(mux, "/static/app.css", nil)
	if cc := plain.Header().Get("Cache-Control"); cc != "public, no-cache" {
		t.Fatalf("unhashed Cache-Control = %q", cc)
	}
}

// A page cached across a deploy asks for a hash that no longer exists. Serving
// the current bytes is right; telling the browser to keep them for a year under
// the wrong name is not.
func TestStaleHashIsServedButNotImmutable(t *testing.T) {
	rec := get(testApp().Mux(), "/static/app.deadbeef.css", nil)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want the current file", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, no-cache" {
		t.Fatalf("Cache-Control = %q, want revalidation for a hash we do not have", cc)
	}
}

func TestUnknownHashedFileIs404(t *testing.T) {
	if rec := get(testApp().Mux(), "/static/nope.12345678.css", nil); rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestARenderedPageIsNeverReusedWithoutAsking(t *testing.T) {
	// A response with no Cache-Control, no Expires and no Last-Modified is
	// eligible for heuristic caching, and browsers take that offer. Because
	// assets are content-hashed and immutable, a stale document keeps pointing
	// at the exact assets it was built with — so the whole application stays
	// coherently one version behind an update until somebody hard-reloads.
	a := New(Config{
		Shell: shell,
		Routes: []router.Route{{
			Pattern: "/", Label: "Home",
			Page: func() templ.Component { return templ.Raw("<p>hello</p>") },
		}},
	})

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/", nil),
		partial(httptest.NewRequest(http.MethodGet, "/", nil)),
	} {
		response := httptest.NewRecorder()
		a.Mux().ServeHTTP(response, request)
		if got := response.Header().Get("Cache-Control"); got != "private, no-cache" {
			t.Fatalf("partial=%q served Cache-Control %q",
				request.Header.Get("X-Partial"), got)
		}
	}
}

func partial(r *http.Request) *http.Request {
	r.Header.Set("X-Partial", "1")
	return r
}

// The shell publishes per-route data endpoints, so the client can fetch the
// payload for the route it is about to render instead of one blob for the app.
func TestClientConfigCarriesPerRouteData(t *testing.T) {
	rt := router.Route{
		Pattern: "/dashboard/metrics",
		Label:   "Metrics",
		Client:  true,
		Data:    "/api/metrics",
		Page:    func() templ.Component { return text("<p>m</p>") },
	}
	var got router.Client
	a := New(Config{
		Routes: []router.Route{rt},
		Shell:  shell,
		Data: func(ctx context.Context, _ string) context.Context {
			got = router.ClientConfig(ctx)
			return ctx
		},
		ClientData: "/api/app",
		Public:     fstest.MapFS{},
	})
	get(a.Mux(), "/dashboard/metrics", nil)

	if got.Pages["/dashboard/metrics"] != "/api/metrics" {
		t.Errorf("Pages = %v", got.Pages)
	}
	// The shared endpoint stays as the fallback for routes that name none.
	if got.Data != "/api/app" {
		t.Errorf("Data = %q, want the fallback to survive", got.Data)
	}
}

// Parameters come from the mux, which already matched the pattern to get here.
// Re-deriving them was the O(routes) scan this replaced, so the thing worth
// testing is that the values still arrive.
func TestParamsComeFromTheMux(t *testing.T) {
	rt := router.Route{
		Pattern: "/blog/{article_id}/c/{n}",
		Label:   "Post",
		Page: func() templ.Component {
			return comp(func(ctx context.Context, w io.Writer) error {
				_, err := io.WriteString(w, router.Param(ctx, "article_id")+"|"+router.Param(ctx, "n"))
				return err
			})
		},
	}
	rec := get(testApp(rt).Mux(), "/blog/fs-routing/c/7", nil)
	if !strings.Contains(rec.Body.String(), "fs-routing|7") {
		t.Errorf("body = %q", rec.Body.String())
	}
}
