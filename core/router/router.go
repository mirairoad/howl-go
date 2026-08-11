// Package router is the route model shared by the generated table, the pages,
// the HTTP mux and the wasm build. It is a leaf: it imports nothing from the
// app, so page packages can import it without creating a cycle with the
// generated table that imports them.
package router

import (
	"context"
	"html"
	"io"
	"regexp"
	"strings"

	"github.com/a-h/templ"
)

// Wrapper is a layout.templ: a component that renders `{ children... }`.
type Wrapper func() templ.Component

// Route is one filesystem-derived route. Everything here is produced by
// tools/fsroutes at build time; nothing is written by hand.
type Route struct {
	Pattern string // "/dashboard/metrics", "/posts/{id}"
	// Label is the short text for nav and tabs, derived from the file or
	// directory name. The document title is NOT this — it comes from Head.
	Label string
	Page  func() templ.Component
	// Head is an optional `templ Head()` in the page file. Its output is merged
	// into <head>, so a page can set its own <title> and pull in <link>/<meta>.
	Head func() templ.Component
	// Mount is an optional `func Mount()` in the page file: a lifecycle hook
	// that runs in the browser after the markup is in the DOM. It is a plain
	// func, so it needs no build tag and lives in the same table the server
	// uses — where it is simply never called.
	Mount func()
	// Unmount runs just before this page's markup is replaced. Anything Mount
	// registered — a store subscription, a timer, an observer — is released
	// here. Without it a subscription outlives its DOM and fires against nodes
	// that no longer exist.
	Unmount func()
	Layouts []Wrapper // outermost first
	Client  bool      // file named *.client.templ -> the wasm build renders it
}

var titleTag = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// HeadParts renders the route's Head and splits the <title> out of it. The
// title is returned separately because the shell must emit exactly one <title>
// element: two in a document and the browser keeps the first, which would make
// a page's own title silently lose to the fallback.
//
// The returned title is plain text, not markup: it is unescaped here because it
// was extracted from rendered HTML, and every consumer escapes for its own
// context — templ for `<title>{ title }</title>`, html.EscapeString for the
// fragment, percent-encoding for the X-Title header. Returning it still escaped
// would double-escape the shell's title, so `A & B` reads as `A &amp;amp; B`.
func (r Route) HeadParts(ctx context.Context, fallback string) (title, rest string) {
	if r.Head == nil {
		return fallback, ""
	}
	var sb strings.Builder
	if err := r.Head().Render(ctx, &sb); err != nil {
		return fallback, ""
	}
	rendered := sb.String()
	title = fallback
	if m := titleTag.FindStringSubmatch(rendered); m != nil {
		title = html.UnescapeString(strings.TrimSpace(m[1]))
		rendered = titleTag.ReplaceAllString(rendered, "")
	}
	return title, strings.TrimSpace(rendered)
}

// Component composes the layout chain around the page. Layouts receive the
// inner tree as templ children, so `layout.templ` is written the obvious way.
func (r Route) Component() templ.Component {
	c := r.Page()
	for i := len(r.Layouts) - 1; i >= 0; i-- {
		c = wrap(r.Layouts[i], c)
	}
	return c
}

func wrap(l Wrapper, inner templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		return l().Render(templ.WithChildren(ctx, inner), w)
	})
}

// Match reports whether a concrete path matches this route's pattern, filling
// in any {param} segments. Used by the wasm renderer, which has no mux.
func (r Route) Match(path string) (map[string]string, bool) {
	want := split(r.Pattern)
	got := split(path)
	if len(want) != len(got) {
		return nil, false
	}
	params := map[string]string{}
	for i, seg := range want {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			params[seg[1:len(seg)-1]] = got[i]
			continue
		}
		if seg != got[i] {
			return nil, false
		}
	}
	return params, true
}

func split(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// Lookup finds the route for a path. Static patterns win over dynamic ones, so
// /posts/new is not swallowed by /posts/{id}.
func Lookup(rs []Route, path string) (Route, map[string]string, bool) {
	var dyn Route
	var dynParams map[string]string
	found := false
	for _, r := range rs {
		params, ok := r.Match(path)
		if !ok {
			continue
		}
		if len(params) == 0 {
			return r, params, true
		}
		if !found {
			dyn, dynParams, found = r, params, true
		}
	}
	return dyn, dynParams, found
}

// Nav returns the routes that belong in the header: top level and static.
// Derived rather than declared — a nested route like /dashboard/metrics reaches
// the user through its own layout's tabs, and a route with a parameter has no
// single URL to link to.
func Nav(rs []Route) []Route {
	var out []Route
	for _, r := range rs {
		if strings.Contains(r.Pattern, "{") {
			continue
		}
		if strings.Count(strings.Trim(r.Pattern, "/"), "/") == 0 {
			out = append(out, r)
		}
	}
	return out
}

// NeedsWasm lists the patterns that require the wasm binary — either because
// the browser renders them or because they have a lifecycle hook. The shell
// hands this to the client so nothing has to hardcode a path prefix.
func NeedsWasm(rs []Route) []string {
	var out []string
	for _, r := range rs {
		if r.Client || r.Mount != nil || r.Unmount != nil {
			out = append(out, r.Pattern)
		}
	}
	return out
}

// Children returns the routes nested directly or indirectly under prefix,
// excluding prefix itself. Layouts use it to draw their own tab bars without
// knowing what lives beneath them.
func Children(rs []Route, prefix string) []Route {
	var out []Route
	for _, r := range rs {
		if strings.HasPrefix(r.Pattern, strings.TrimSuffix(prefix, "/")+"/") {
			out = append(out, r)
		}
	}
	return out
}

// StaticFile maps a pattern to its file on disk: "/about" -> "about/index.html".
func (r Route) StaticFile() string {
	p := strings.Trim(r.Pattern, "/")
	if p == "" {
		return "index.html"
	}
	return p + "/index.html"
}

// ---------------------------------------------------------------------------
// Context: layouts need the route table (to draw nav) and the active path (to
// highlight it) without importing the package that holds them.
// ---------------------------------------------------------------------------

type ctxKey int

const (
	routesKey ctxKey = iota
	currentKey
	paramsKey
)

func WithRoutes(ctx context.Context, rs []Route) context.Context {
	return context.WithValue(ctx, routesKey, rs)
}

func Routes(ctx context.Context) []Route {
	rs, _ := ctx.Value(routesKey).([]Route)
	return rs
}

func WithCurrent(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, currentKey, path)
}

func Current(ctx context.Context) string {
	s, _ := ctx.Value(currentKey).(string)
	return s
}

func WithParams(ctx context.Context, p map[string]string) context.Context {
	return context.WithValue(ctx, paramsKey, p)
}

func Param(ctx context.Context, name string) string {
	p, _ := ctx.Value(paramsKey).(map[string]string)
	return p[name]
}

// Under reports whether the active path sits inside prefix — for nav
// highlighting, where /dashboard should stay lit on /dashboard/metrics.
func Under(ctx context.Context, prefix string) bool {
	cur := Current(ctx)
	return cur == prefix || strings.HasPrefix(cur, strings.TrimSuffix(prefix, "/")+"/")
}
