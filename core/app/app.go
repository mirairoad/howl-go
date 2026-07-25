// Package app is the server runtime: everything an application would otherwise
// copy into its own main.go. Give it a route table and a document shell and it
// serves SSR, SPA fragments, the wasm renderer's assets, and a static export.
package app

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/mirairoad/howl-go/core/router"
	"github.com/mirairoad/howl-go/core/runtime"
)

// Shell renders the document around a page. An application supplies this from
// its own app.templ; the framework only needs the title and the head fragment.
type Shell func(title, head string) templ.Component

// Config is everything the runtime needs that it cannot derive.
type Config struct {
	Routes []router.Route
	Shell  Shell
	// NotFound renders the body of a 404. Optional.
	NotFound func(path string) templ.Component
	// Public is the application's own static files, served under /static/.
	// The framework's client runtime is layered underneath automatically.
	Public fs.FS
	// Data decorates the context for every render: page data, plus anything an
	// application wants its components to read. Optional.
	Data func(ctx context.Context, path string) context.Context
	Addr string
}

type App struct{ cfg Config }

func New(cfg Config) *App {
	if cfg.Addr == "" {
		cfg.Addr = ":9000"
	}
	return &App{cfg: cfg}
}

// Canonical trims a trailing slash so "/a/" and "/a" are the same route.
func Canonical(p string) string {
	if len(p) > 1 {
		return strings.TrimSuffix(p, "/")
	}
	return p
}

func (a *App) context(ctx context.Context, path string) context.Context {
	ctx = router.WithRoutes(ctx, a.cfg.Routes)
	ctx = router.WithCurrent(ctx, path)
	if _, params, ok := router.Lookup(a.cfg.Routes, path); ok {
		ctx = router.WithParams(ctx, params)
	}
	if a.cfg.Data != nil {
		ctx = a.cfg.Data(ctx, path)
	}
	return ctx
}

// Render is the hybrid switch — the only code that knows about SSR vs SPA.
func (a *App) Render(w http.ResponseWriter, r *http.Request, rt router.Route, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	ctx := a.context(r.Context(), Canonical(r.URL.Path))
	title, head := rt.HeadParts(ctx, rt.Label)

	// SPA navigation: the page plus its layouts, without the document shell.
	// A fragment has no <head>, so the page's head travels with it in an inert
	// <template> for the client to merge.
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

	if err := a.cfg.Shell(title, head).Render(templ.WithChildren(ctx, c), w); err != nil {
		log.Printf("render page %s: %v", r.URL.Path, err)
	}
}

// Mux builds a ServeMux with every route from the table registered, the static
// handler mounted, and a 404 fallback. Applications add their own API routes to
// the returned mux.
func (a *App) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", GzipStatic(http.StripPrefix("/static/", http.FileServer(http.FS(assets(a.cfg.Public))))))

	for _, rt := range a.cfg.Routes {
		h := func(w http.ResponseWriter, r *http.Request) { a.Render(w, r, rt, rt.Component()) }
		if rt.Pattern == "/" {
			// "GET /" is ServeMux's catch-all; "{$}" matches only the root so
			// the 404 handler below still owns everything else.
			mux.HandleFunc("GET /{$}", h)
			continue
		}
		mux.HandleFunc("GET "+rt.Pattern, h)
		mux.HandleFunc("GET "+rt.Pattern+"/{$}", h) // trailing slash, no redirect
	}

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		if a.cfg.NotFound == nil {
			fmt.Fprintf(w, "404 %s", r.URL.Path)
			return
		}
		a.Render(w, r, router.Route{Label: "404"}, a.cfg.NotFound(r.URL.Path))
	})
	return mux
}

// Listen serves the mux with the latency simulator attached.
func (a *App) Listen(mux http.Handler) error {
	fmt.Printf("listening on http://localhost%s (%d routes from the filesystem)\n", a.cfg.Addr, len(a.cfg.Routes))
	return http.ListenAndServe(a.cfg.Addr, Latency(mux))
}

// Export renders every static route to a directory — the same components, a
// third caller after SSR and wasm.
func (a *App) Export(dir string) error {
	for _, rt := range a.cfg.Routes {
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
		ctx := a.context(context.Background(), rt.Pattern)
		title, head := rt.HeadParts(ctx, rt.Label)
		err = a.cfg.Shell(title, head).Render(templ.WithChildren(ctx, rt.Component()), f)
		f.Close()
		if err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", path)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Static assets
// ---------------------------------------------------------------------------

// assets layers the application's own files over the framework's client
// runtime, so app.js is shared and never copied into an application.
type layered struct{ app, core fs.FS }

func (l layered) Open(name string) (fs.File, error) {
	if l.app != nil {
		if f, err := l.app.Open(name); err == nil {
			return f, nil
		}
	}
	return l.core.Open(name)
}

func assets(appFS fs.FS) fs.FS { return layered{app: appFS, core: runtime.Assets()} }

// GzipStatic compresses static assets. net/http does not by default, and for a
// multi-megabyte wasm payload the difference is ~3.4x.
func GzipStatic(next http.Handler) http.Handler {
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

// Latency simulates distance from the origin. LATENCY=240ms is roughly Sydney
// to us-east-1: every server round-trip pays it, which is exactly the cost a
// client-side renderer avoids.
func Latency(next http.Handler) http.Handler {
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
