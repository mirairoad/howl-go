// Package app is the server runtime: everything an application would otherwise
// copy into its own main.go. Give it a route table and a document shell and it
// serves SSR, SPA fragments, the wasm renderer's assets, and a static export.
package app

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"

	"github.com/mirairoad/howl-go/core/mw"
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

	// Use is the middleware chain, outermost first. It wraps everything —
	// pages, static files and any handler the application adds to the mux.
	Use []mw.Middleware
	// OnError renders the response when a component fails to render. Because
	// pages are rendered into a buffer before anything is sent, this still owns
	// the status line: a half-written page is never flushed to the client.
	// Optional; the default is a plain 500.
	OnError func(w http.ResponseWriter, r *http.Request, err error)

	// StaticMaxAge is the Cache-Control lifetime for /static/. Zero means
	// revalidate-with-ETag, which is the safe default for unhashed names.
	StaticMaxAge time.Duration
	// StaticImmutable marks files that may be cached for a year — pass
	// app.Hashed for the usual app.9f8c2a1b.css convention.
	StaticImmutable func(name string) bool
	// Dev re-reads static files on every request instead of caching them.
	Dev bool
	// PublicDir serves /static/ from this directory instead of Public. Set it
	// to edit CSS without rebuilding the binary that embeds it; `howl dev` sets
	// it for you through HOWL_PUBLIC_DIR.
	PublicDir string

	// Log is the runtime's own logger — the startup line, render failures.
	// Defaults to slog.Default(); see core/console for the tinted one.
	Log *slog.Logger

	// ClientData is a JSON endpoint the browser fetches once before its first
	// local render, and hands to the wasm renderer as its data argument. Leave
	// it empty when client routes need no data — the fetch is then skipped
	// entirely rather than failing on a URL that does not exist.
	ClientData string
}

type App struct {
	cfg    Config
	mounts []mount
	static *Static
}

type mount struct {
	prefix  string
	handler http.Handler
}

func New(cfg Config) *App {
	// The dev server configures the application through the environment, so an
	// application needs no dev-mode code of its own and no flag it must
	// remember to pass. Nothing here is magic in production: these variables
	// are namespaced, documented, and unset unless `howl dev` set them.
	if v := os.Getenv("HOWL_ADDR"); v != "" {
		cfg.Addr = v
	}
	if v := os.Getenv("HOWL_PUBLIC_DIR"); v != "" {
		cfg.PublicDir, cfg.Dev = v, true
	}
	if cfg.Addr == "" {
		cfg.Addr = ":9000"
	}

	public := cfg.Public
	if cfg.PublicDir != "" {
		public = os.DirFS(cfg.PublicDir)
	}
	return &App{
		cfg: cfg,
		static: &Static{
			FS:        assets(public),
			MaxAge:    cfg.StaticMaxAge,
			Immutable: cfg.StaticImmutable,
			Reload:    cfg.Dev || cfg.PublicDir != "",
		},
	}
}

// Use appends middleware to the chain, outermost first. Same as Config.Use, for
// applications that build their chain conditionally.
func (a *App) Use(ms ...mw.Middleware) *App {
	a.cfg.Use = append(a.cfg.Use, ms...)
	return a
}

// Mount serves another handler under a prefix, with the prefix stripped: an
// admin app, a metrics endpoint, a third-party mux. Sub-applications are just
// http.Handlers, so there is no special "sub-app" type to learn.
func (a *App) Mount(prefix string, h http.Handler) *App {
	prefix = "/" + strings.Trim(prefix, "/")
	a.mounts = append(a.mounts, mount{prefix: prefix, handler: h})
	return a
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
	ctx = router.WithAssets(ctx, a.asset)
	client := router.Client{
		Wasm: router.NeedsWasm(a.cfg.Routes),
		Data: a.cfg.ClientData,
		Live: liveEndpoint(),
	}
	// Only when something actually needs the renderer: publishing these on a
	// server-rendered-only site would hash two files nobody downloads.
	if len(client.Wasm) > 0 {
		client.Binary = a.asset("views.wasm")
		client.Exec = a.asset("wasm_exec.js")
	}
	ctx = router.WithClient(ctx, client)
	if a.cfg.Data != nil {
		ctx = a.cfg.Data(ctx, path)
	}
	return ctx
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func getBuf() *bytes.Buffer {
	b := bufPool.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

// raw hands already-rendered bytes to templ as a component, without copying
// them into a string first.
func raw(b []byte) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := w.Write(b)
		return err
	})
}

// Render is the hybrid switch — the only code that knows about SSR vs SPA.
func (a *App) Render(w http.ResponseWriter, r *http.Request, rt router.Route, c templ.Component) {
	a.render(w, r, rt, c, http.StatusOK)
}

// render draws the page into a buffer before touching the response. Buffering
// buys three things a streaming render cannot have: a status a component can
// still change (see router.SetStatus), an error page instead of a truncated document
// when a component fails, and a Content-Length.
func (a *App) render(w http.ResponseWriter, r *http.Request, rt router.Route, c templ.Component, status int) {
	// A component may change this while it renders — see router.NotFound.
	ctx := router.WithStatus(a.context(r.Context(), Canonical(r.URL.Path)), status)

	body := getBuf()
	defer bufPool.Put(body)
	if err := c.Render(ctx, body); err != nil {
		a.fail(w, r, fmt.Errorf("render %s: %w", r.URL.Path, err))
		return
	}
	title, head := rt.HeadParts(ctx, rt.Label)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// SPA navigation: the page plus its layouts, without the document shell.
	// A fragment has no <head>, so the page's head travels with it in an inert
	// <template> for the client to merge.
	if r.Header.Get("X-Partial") == "1" {
		// The title travels in the body, not the header. A header is bytes, and
		// fetch() decodes response headers as ISO-8859-1, so a UTF-8 em dash
		// would surface as "â€”"; the body is decoded as UTF-8 per Content-Type.
		// This also matches the wasm renderer, which has always emitted <title>
		// inside the head template — one wire shape, one merge path.
		//
		// X-Title stays as a fallback for a client that has the body but wants
		// the title without parsing it (the prefetch cache). Percent-encoded for
		// the same ISO-8859-1 reason, and decoded by the client.
		w.Header().Set("X-Title", url.PathEscape(title))
		w.Header().Set("Vary", "X-Partial")
		w.WriteHeader(router.Status(ctx))
		fmt.Fprintf(w, "<template data-head><title>%s</title>%s</template>", html.EscapeString(title), head)
		w.Write(body.Bytes()) //nolint:errcheck // the client is gone; nothing to do
		return
	}

	// A .raw route is its own document: no shell, no head template. For
	// embeds, print views and anything fetched as a partial by something that
	// is not our client runtime.
	if rt.Raw {
		w.WriteHeader(router.Status(ctx))
		w.Write(body.Bytes()) //nolint:errcheck
		return
	}

	doc := getBuf()
	defer bufPool.Put(doc)
	if err := a.cfg.Shell(title, head).Render(templ.WithChildren(ctx, raw(body.Bytes())), doc); err != nil {
		a.fail(w, r, fmt.Errorf("render shell %s: %w", r.URL.Path, err))
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(doc.Len()))
	w.WriteHeader(router.Status(ctx))
	w.Write(doc.Bytes()) //nolint:errcheck
}

func (a *App) fail(w http.ResponseWriter, r *http.Request, err error) {
	a.Log().Error("render failed", slog.String("path", r.URL.Path), slog.Any("err", err))
	if a.cfg.OnError != nil {
		a.cfg.OnError(w, r, err)
		return
	}
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// ---------------------------------------------------------------------------
// Serving
// ---------------------------------------------------------------------------

// Mux builds a ServeMux with every route from the table registered, the static
// handler mounted, and a 404 fallback. Applications add their own API routes to
// the returned mux.
func (a *App) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", a.static.Handler()))

	for _, m := range a.mounts {
		h := strip(m.prefix, m.handler)
		mux.Handle(m.prefix, h)     // /admin
		mux.Handle(m.prefix+"/", h) // /admin/anything
	}

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

	// Not "GET /": a method-specific catch-all conflicts with any method-less
	// pattern registered above it, and ServeMux panics at registration time
	// rather than at the request. Owning every method also means a POST to a
	// path that does not exist gets the application's 404 rather than a bare
	// 405 from the mux.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.NotFound == nil {
			http.Error(w, "404 "+r.URL.Path, http.StatusNotFound)
			return
		}
		a.render(w, r, router.Route{Label: "404"}, a.cfg.NotFound(r.URL.Path), http.StatusNotFound)
	})
	return mux
}

// strip removes the mount prefix, mapping the bare prefix to "/" instead of
// the empty path http.StripPrefix would hand the sub-handler — a mux given ""
// matches nothing and answers 404 at its own root.
func strip(prefix string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, prefix)
		if p == "" {
			p = "/"
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = p
		h.ServeHTTP(w, r2)
	})
}

// Handler is Mux wrapped in the configured middleware. Use it when you want the
// full chain but not Listen — tests, a custom http.Server, a cloud runtime.
func (a *App) Handler() http.Handler { return a.Wrap(a.Mux()) }

// Wrap applies the middleware chain to any handler, so an application that
// builds its own mux still gets the same treatment.
func (a *App) Wrap(h http.Handler) http.Handler { return mw.Chain(h, a.cfg.Use...) }

// asset is the content-hashed URL for a static file. See Static.Name.
func (a *App) asset(name string) string { return "/static/" + a.static.Name(name) }

// Log is the logger the runtime itself uses: Config.Log, or slog.Default().
// Install core/console at startup and this comes out tinted and aligned.
func (a *App) Log() *slog.Logger {
	if a.cfg.Log != nil {
		return a.cfg.Log
	}
	return slog.Default()
}

// Listen serves h with the middleware chain and the latency simulator attached.
// Pass a.Mux(), or your own handler built on top of it.
//
// The startup line goes through the logger rather than to stdout directly, so
// it obeys whatever the process decided about format — tinted in a terminal,
// JSON under systemd. It is the one line you always want: a server that is up
// and a server that is up *on the port you meant* look identical otherwise.
func (a *App) Listen(h http.Handler) error {
	log := a.Log()
	for _, rt := range a.cfg.Routes {
		log.Debug("route", slog.String("pattern", rt.Pattern), slog.Bool("client", rt.Client))
	}
	log.Info("listening",
		slog.String("url", localURL(a.cfg.Addr)),
		slog.Int("routes", len(a.cfg.Routes)),
	)

	// Compress the static files now rather than on someone's first request. In
	// the background, because a large wasm binary takes a few hundred
	// milliseconds and there is no reason to hold the port for it.
	go func() {
		start := time.Now()
		files, bytes := a.static.Warm()
		if files > 0 {
			log.Debug("static warmed",
				slog.Int("files", files),
				slog.String("size", size(bytes)),
				slog.Duration("took", time.Since(start).Round(time.Millisecond)),
			)
		}
	}()
	return http.ListenAndServe(a.cfg.Addr, Latency(a.Wrap(h)))
}

// liveEndpoint is where the browser subscribes to rebuild notifications. Only
// `howl dev` sets HOWL_DEV, and only it serves this path — in production the
// field is empty and the client skips the dev module entirely.
func liveEndpoint() string {
	if os.Getenv("HOWL_DEV") == "" {
		return ""
	}
	return "/_howl/alive"
}

func size(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fkB", float64(n)/(1<<10))
	}
	return strconv.FormatInt(n, 10) + "B"
}

// localURL turns a listen address into something clickable in a terminal.
func localURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "http://localhost" + addr
	}
	return "http://" + addr
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
		if rt.Raw {
			err = rt.Component().Render(ctx, f)
		} else {
			err = a.cfg.Shell(title, head).Render(templ.WithChildren(ctx, rt.Component()), f)
		}
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

// Latency simulates distance from the origin. LATENCY=240ms is roughly Sydney
// to us-east-1: every server round-trip pays it, which is exactly the cost a
// client-side renderer avoids.
func Latency(next http.Handler) http.Handler {
	d, _ := time.ParseDuration(os.Getenv("LATENCY"))
	if d <= 0 {
		return next
	}
	slog.Warn("simulating network latency on every request", slog.Duration("delay", d))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			time.Sleep(d)
		}
		next.ServeHTTP(w, r)
	})
}
