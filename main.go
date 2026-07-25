package main

import (
	"compress/gzip"
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"howl-go/client/pages"
	"howl-go/client/router"
	"howl-go/client/store"
	"howl-go/client/ui"
)

//go:generate go run ./tools/fsroutes

//go:embed client/public
var publicFS embed.FS

// The route table is generated from the directory tree; nothing here is
// hand-maintained.
var routes = pages.FsClientRoutes()

var db = store.New()

// ---------------------------------------------------------------------------
// The hybrid switch. This is the only code that knows about SSR vs SPA.
// ---------------------------------------------------------------------------

func page(w http.ResponseWriter, r *http.Request, rt router.Route, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	ctx := withData(r.Context(), canonical(r.URL.Path))
	title, head := rt.HeadParts(ctx, rt.Label)

	// SPA navigation: the page plus its layouts, without the document shell.
	// A fragment has no <head>, so the page's head travels with it in an inert
	// <template> for the client to merge — otherwise every SPA navigation would
	// keep the first page's title, canonical and meta forever.
	if r.Header.Get("X-Partial") == "1" {
		w.Header().Set("X-Title", title)
		w.Header().Set("Vary", "X-Partial")
		if head != "" {
			fmt.Fprintf(w, "<template data-head>%s</template>", head)
		}
		if err := c.Render(ctx, w); err != nil {
			log.Printf("render fragment %s: %v", r.URL.Path, err)
		}
		return
	}

	// Cold request: app.templ wraps everything.
	if err := pages.App(title, head).Render(templ.WithChildren(ctx, c), w); err != nil {
		log.Printf("render page %s: %v", r.URL.Path, err)
	}
}

// withData puts the route table, the active path and the page data into the
// context. Pages take no arguments — the generated table needs one uniform
// signature — so this is how they receive everything.
func withData(ctx context.Context, path string) context.Context {
	ctx = router.WithRoutes(ctx, routes)
	ctx = router.WithCurrent(ctx, path)
	// Dynamic segments: [article_id] in a filename became {article_id} in the
	// pattern, and the concrete value reaches the page through context.
	if _, params, ok := router.Lookup(routes, path); ok {
		ctx = router.WithParams(ctx, params)
	}
	ctx = store.WithMetrics(ctx, demoMetrics())
	ctx = store.WithTodos(ctx, db.List())
	return store.WithMeta(ctx, store.Meta{
		RenderedAt: time.Now().Format("15:04:05.000"),
		GoVersion:  runtime.Version(),
		Region:     "us-east-1",
	})
}

func canonical(p string) string {
	if len(p) > 1 {
		return strings.TrimSuffix(p, "/")
	}
	return p
}

func demoMetrics() store.Metrics {
	return store.Metrics{
		Cards: []store.Card{
			{Label: "Active sessions", Value: "12,847", Delta: 4.2},
			{Label: "p95 latency", Value: "184 ms", Delta: -11.5},
			{Label: "Error rate", Value: "0.31%", Delta: -2.0},
			{Label: "Throughput", Value: "9.2k/s", Delta: 8.7},
		},
		Rows: []store.Row{
			{Name: "sydney", Value: 48210}, {Name: "singapore", Value: 39104},
			{Name: "frankfurt", Value: 28755}, {Name: "us-east-1", Value: 91233},
			{Name: "us-west-2", Value: 40188}, {Name: "sao-paulo", Value: 12044},
			{Name: "tokyo", Value: 33590}, {Name: "mumbai", Value: 21877},
		},
	}
}

// latency simulates distance from the origin. LATENCY=240ms is roughly Sydney
// to us-east-1: every server round-trip pays it, which is exactly the cost a
// client-side renderer avoids.
func latency(next http.Handler) http.Handler {
	d, _ := time.ParseDuration(os.Getenv("LATENCY"))
	if d <= 0 {
		return next
	}
	fmt.Printf("simulating %s of network latency on every request\n", d)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			time.Sleep(d)
		}
		next.ServeHTTP(w, r)
	})
}

// gzipStatic compresses static assets. net/http does not by default, and for a
// multi-megabyte wasm payload the difference is ~3.4x.
func gzipStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".wasm") {
			w.Header().Set("Content-Type", "application/wasm")
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		next.ServeHTTP(gzipWriter{ResponseWriter: w, Writer: gz}, r)
	})
}

type gzipWriter struct {
	http.ResponseWriter
	io.Writer
}

func (g gzipWriter) Write(b []byte) (int, error) { return g.Writer.Write(b) }

// exportStatic renders every generated route to a file — the same components,
// a third caller after SSR and wasm.
func exportStatic(dir string) error {
	for _, rt := range routes {
		if strings.Contains(rt.Pattern, "{") {
			fmt.Printf("skipped %s (dynamic: needs a parameter source)\n", rt.Pattern)
			continue
		}
		path := filepath.Join(dir, rt.StaticFile())
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		ctx := withData(context.Background(), rt.Pattern)
		title, head := rt.HeadParts(ctx, rt.Label)
		err = pages.App(title, head).Render(templ.WithChildren(ctx, rt.Component()), f)
		f.Close()
		if err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", path)
	}
	return nil
}

func main() {
	static := flag.String("static", "", "render routes to this directory and exit")
	flag.Parse()

	db.Add("render the same component three ways")
	db.Add("keep island state across navigation")

	if *static != "" {
		if err := exportStatic(*static); err != nil {
			log.Fatal(err)
		}
		return
	}

	mux := http.NewServeMux()

	public, err := fs.Sub(publicFS, "client/public")
	if err != nil {
		log.Fatal(err)
	}
	// The /static/ URL prefix is a stable public contract; only the on-disk
	// location lives under client/public.
	mux.Handle("GET /static/", gzipStatic(http.StripPrefix("/static/", http.FileServer(http.FS(public)))))

	// Every route comes from the generated table. Creating a directory with an
	// index.templ is the entire process for adding a page.
	for _, rt := range routes {
		h := func(w http.ResponseWriter, r *http.Request) {
			page(w, r, rt, rt.Component())
		}
		if rt.Pattern == "/" {
			// "GET /" is ServeMux's catch-all; "{$}" makes it match only the root
			// so the 404 handler below can still own everything else.
			mux.HandleFunc("GET /{$}", h)
			continue
		}
		mux.HandleFunc("GET "+rt.Pattern, h)
		mux.HandleFunc("GET "+rt.Pattern+"/{$}", h) // trailing slash, no redirect
	}

	// Data endpoints. Once the wasm renderer is up these are the only things
	// that cross the wire.
	mux.HandleFunc("GET /api/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(demoMetrics())
	})

	mux.HandleFunc("GET /api/todos", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(db.Snapshot())
	})

	// The client has ALREADY applied these ops and re-rendered; this is
	// bookkeeping, not the critical path.
	mux.HandleFunc("POST /api/todos/sync", func(w http.ResponseWriter, r *http.Request) {
		var ops []store.Op
		if err := json.NewDecoder(r.Body).Decode(&ops); err != nil {
			http.Error(w, "bad ops", http.StatusBadRequest)
			return
		}
		for _, op := range ops {
			db.Apply(op)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(db.Snapshot())
	})

	// Fragment API: the body is a rendered <li>, not JSON — the no-wasm path.
	mux.HandleFunc("POST /api/todos", func(w http.ResponseWriter, r *http.Request) {
		text := r.FormValue("text")
		if text == "" {
			http.Error(w, "text required", http.StatusBadRequest)
			return
		}
		t := db.Add(text)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Todo-Count", strconv.Itoa(db.Count()))
		if err := ui.TodoItem(t).Render(r.Context(), w); err != nil {
			log.Printf("render todo: %v", err)
		}
	})

	mux.HandleFunc("DELETE /api/todos/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		db.Del(id)
		w.Header().Set("X-Todo-Count", strconv.Itoa(db.Count()))
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		page(w, r, router.Route{Label: "404"}, pages.NotFound(r.URL.Path))
	})

	addr := ":9000"
	fmt.Printf("listening on http://localhost%s (%d routes from the filesystem)\n", addr, len(routes))
	log.Fatal(http.ListenAndServe(addr, latency(mux)))
}
