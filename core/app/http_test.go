package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/a-h/templ"

	"github.com/mirairoad/howl-go/core/mw"
	"github.com/mirairoad/howl-go/core/router"
)

// counted is a page that prints how many times it has rendered, so a response
// from the cache is the one whose number did not move.
func counted(pattern string, ttl time.Duration, renders *atomic.Int64) router.Route {
	return router.Route{
		Pattern: pattern,
		Label:   "Counted",
		Cache:   ttl,
		Page: func() templ.Component {
			return comp(func(ctx context.Context, w io.Writer) error {
				_, err := io.WriteString(w, "<p>render "+strconv.FormatInt(renders.Add(1), 10)+"</p>")
				return err
			})
		},
	}
}

func TestPageCacheServesAnonymousRepeats(t *testing.T) {
	var renders atomic.Int64
	h := testApp(counted("/pricing", time.Minute, &renders)).Handler()

	first := get(h, "/pricing", nil)
	second := get(h, "/pricing/", nil) // the same page by its other spelling
	fragment := get(h, "/pricing", map[string]string{"X-Partial": "1"})
	fragmentAgain := get(h, "/pricing", map[string]string{"X-Partial": "1"})

	if renders.Load() != 2 {
		t.Fatalf("rendered %d times, want 2: one document, one fragment", renders.Load())
	}
	if second.Header().Get(mw.HeaderCache) != "hit" || second.Body.String() != first.Body.String() {
		t.Fatalf("second document: %q %q", second.Header().Get(mw.HeaderCache), second.Body.String())
	}
	if strings.Contains(fragment.Body.String(), "<html>") {
		t.Fatal("the fragment was served the cached document")
	}
	if fragmentAgain.Header().Get(mw.HeaderCache) != "hit" {
		t.Fatalf("second fragment: X-Howl-Cache = %q", fragmentAgain.Header().Get(mw.HeaderCache))
	}
	if second.Header().Get("X-Howl-Build") == "" {
		t.Fatal("a cached page lost X-Howl-Build")
	}
}

// A rendered page is somebody's. A visitor with a cookie renders their own and
// never fills the store for anyone else.
func TestPageCacheNeverSharesACredentialedRender(t *testing.T) {
	var renders atomic.Int64
	h := testApp(counted("/pricing", time.Minute, &renders)).Handler()

	get(h, "/pricing", map[string]string{"Cookie": "session=alice"})
	w := get(h, "/pricing", nil)
	get(h, "/pricing", map[string]string{"Authorization": "Bearer bob"})

	if renders.Load() != 3 {
		t.Fatalf("rendered %d times, want 3", renders.Load())
	}
	if w.Header().Get(mw.HeaderCache) != "miss" {
		t.Fatalf("anonymous request after alice's: X-Howl-Cache = %q, want miss", w.Header().Get(mw.HeaderCache))
	}
}

// mw.CSRF hands every cookie-less visitor a cookie, so its pages are never
// stored — otherwise the stored page would carry one visitor's token to all.
func TestPageCacheStoresNothingThatSetsACookie(t *testing.T) {
	var renders atomic.Int64
	a := testApp(counted("/pricing", time.Minute, &renders))
	a.Use(mw.CSRF{}.Handler)
	h := a.Handler()

	get(h, "/pricing", nil)
	get(h, "/pricing", nil)
	if renders.Load() != 2 {
		t.Fatalf("rendered %d times, want 2 — a response setting a cookie was stored", renders.Load())
	}
}

func TestPageWithoutCacheDirectiveRendersEveryTime(t *testing.T) {
	var renders atomic.Int64
	h := testApp(counted("/live", 0, &renders)).Handler()
	get(h, "/live", nil)
	if w := get(h, "/live", nil); w.Header().Get(mw.HeaderCache) != "" || renders.Load() != 2 {
		t.Fatalf("an uncached page: renders %d, X-Howl-Cache %q", renders.Load(), w.Header().Get(mw.HeaderCache))
	}
}

func TestRedirectIsADocumentLoadForAFragment(t *testing.T) {
	guard := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Redirect(w, r, "/sign-in", http.StatusSeeOther)
	})

	doc := get(guard, "/dashboard", nil)
	if doc.Code != http.StatusSeeOther || doc.Header().Get("Location") != "/sign-in" {
		t.Fatalf("document: %d %q", doc.Code, doc.Header().Get("Location"))
	}

	frag := get(guard, "/dashboard", map[string]string{"X-Partial": "1"})
	if frag.Code != http.StatusNoContent || frag.Header().Get(HeaderLocation) != "/sign-in" || frag.Header().Get("Location") != "" {
		t.Fatalf("fragment: %d Location=%q X-Howl-Location=%q, want 204 and only X-Howl-Location",
			frag.Code, frag.Header().Get("Location"), frag.Header().Get(HeaderLocation))
	}
}

func TestStaticFilesServesRootFilesAndPassesTheRest(t *testing.T) {
	root := fstest.MapFS{
		"robots.txt":               {Data: []byte("User-agent: *\n")},
		".well-known/security.txt": {Data: []byte("Contact: mailto:security@example.com\n")},
		"favicon.ico":              {Data: []byte{0, 0, 1, 0}},
	}
	h := StaticFiles(root)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "page") //nolint:errcheck
	}))

	for path, want := range map[string]string{
		"/robots.txt":               "User-agent: *\n",
		"/.well-known/security.txt": "Contact: mailto:security@example.com\n",
		"/about":                    "page",
		"/.well-known":              "page", // a directory is not a file
		"/../robots.txt":            "User-agent: *\n",
	} {
		if got := get(h, path, nil).Body.String(); got != want {
			t.Errorf("GET %s = %q, want %q", path, got, want)
		}
	}
	if w := get(h, "/robots.txt", nil); w.Header().Get("ETag") == "" {
		t.Error("a root file has no ETag — it did not go through the static handler")
	}

	post := httptest.NewRecorder()
	h.ServeHTTP(post, httptest.NewRequest("POST", "/robots.txt", nil))
	if post.Body.String() != "page" {
		t.Errorf("POST /robots.txt = %q, want it passed on", post.Body.String())
	}
}

func TestLoggerLogsEachRequestOnce(t *testing.T) {
	var buf bytes.Buffer
	a := New(Config{
		Routes: []router.Route{{Pattern: "/", Label: "Home", Page: func() templ.Component { return text("home") }}},
		Shell:  shell,
		Log:    slog.New(slog.NewTextHandler(&buf, nil)),
		Logger: true,
		Use:    []mw.Middleware{mw.RequestID}, // listed again: must keep one id
	})
	w := get(a.Handler(), "/", nil)

	if n := strings.Count(buf.String(), "msg=http"); n != 1 {
		t.Fatalf("%d request lines, want 1:\n%s", n, buf.String())
	}
	id := w.Header().Get(mw.HeaderRequestID)
	if id == "" || !strings.Contains(buf.String(), "id="+id) {
		t.Fatalf("logged id and answered id differ: header %q, log %s", id, buf.String())
	}
}

func TestRedirectTrailingSlashOnlyTouchesPages(t *testing.T) {
	a := New(Config{
		Routes: []router.Route{
			{Pattern: "/", Label: "Home", Page: func() templ.Component { return text("home") }},
			{Pattern: "/about", Label: "About", Page: func() templ.Component { return text("about") }},
		},
		Shell:                 shell,
		RedirectTrailingSlash: true,
	})
	mux := a.Mux()
	// A subtree pattern: ServeMux itself redirects /files to /files/. A
	// middleware trimming the slash would bounce that request forever.
	mux.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.FS(fstest.MapFS{"a.txt": {Data: []byte("a")}}))))
	h := a.Wrap(mux)

	if w := get(h, "/about/?tab=team", nil); w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/about?tab=team" {
		t.Fatalf("/about/ = %d %q, want 301 /about?tab=team", w.Code, w.Header().Get("Location"))
	}
	if w := get(h, "/about", nil); w.Code != http.StatusOK {
		t.Fatalf("/about = %d", w.Code)
	}
	if w := get(h, "/", nil); w.Code != http.StatusOK {
		t.Fatalf("/ = %d — the root is only a slash", w.Code)
	}
	if w := get(h, "/files/", nil); w.Code != http.StatusOK {
		t.Fatalf("/files/ = %d %q — a subtree handler was redirected", w.Code, w.Header().Get("Location"))
	}
}
