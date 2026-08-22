package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/a-h/templ"

	"github.com/mirairoad/howl-go/core/mw"
	"github.com/mirairoad/howl-go/core/router"
)

// The render path is a fixed cost paid by every page, so it is benchmarked
// against two pages that bracket what an application actually serves: a
// paragraph, and a table big enough that the markup dominates. Anything the
// framework spends that does not scale with the page shows up as the gap
// between them.

// simplePage is roughly the smallest useful page: a heading and a paragraph,
// ~200 bytes of markup.
func simplePage() templ.Component {
	return comp(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, `<section class="page"><h1>Routes come from the filesystem</h1><p class="lede">Rendered by Go, into a buffer, before anything is sent.</p></section>`)
		return err
	})
}

// Cell text is precomputed: a row's data is whatever the application already
// has, and strconv inside the loop would put 600 allocations of the
// benchmark's own making into the framework's column.
var regions, values = func() ([]string, []string) {
	rs := make([]string, 16)
	vs := make([]string, 16)
	for i := range rs {
		rs[i] = fmt.Sprintf("region-%02d", i)
		vs[i] = strconv.Itoa(i*977 + 10000)
	}
	return rs, vs
}()

// tablePage is the other end: a table of n rows, each with attributes and two
// cells, which is what a dashboard route looks like.
func tablePage(n int) func() templ.Component {
	return func() templ.Component {
		return comp(func(_ context.Context, w io.Writer) error {
			// Written straight to the writer, one string at a time, because
			// that is what templ's generated code does. Buffering it here
			// first would measure the benchmark instead of the framework.
			io.WriteString(w, `<table class="tbl"><tbody data-rows>`) //nolint:errcheck
			for i := range n {
				io.WriteString(w, `<tr data-name="`)       //nolint:errcheck
				io.WriteString(w, regions[i%len(regions)]) //nolint:errcheck
				io.WriteString(w, `" data-value="`)        //nolint:errcheck
				io.WriteString(w, values[i%len(values)])   //nolint:errcheck
				io.WriteString(w, `"><td>`)                //nolint:errcheck
				io.WriteString(w, regions[i%len(regions)]) //nolint:errcheck
				io.WriteString(w, `</td><td class="num">`) //nolint:errcheck
				io.WriteString(w, values[i%len(values)])   //nolint:errcheck
				io.WriteString(w, `</td></tr>`)            //nolint:errcheck
			}
			_, err := io.WriteString(w, `</tbody></table>`)
			return err
		})
	}
}

// layouts wrap the page the way a nested route's chain does — three of them,
// which is `dashboard/metrics` plus a root layout.
func layout(tag string) router.Wrapper {
	return func() templ.Component {
		return comp(func(ctx context.Context, w io.Writer) error {
			io.WriteString(w, "<"+tag+">") //nolint:errcheck
			if err := templ.GetChildren(ctx).Render(ctx, w); err != nil {
				return err
			}
			_, err := io.WriteString(w, "</"+tag+">")
			return err
		})
	}
}

func headComp() templ.Component {
	return comp(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, `<title>Metrics — bench</title><meta name="description" content="rows"/><link rel="canonical" href="https://example.com/x"/>`)
		return err
	})
}

// filler pads the route table to a given size. Route-table cost is per-request
// in the current implementation, so the table size is a benchmark variable, not
// a detail.
func filler(n int) []router.Route {
	out := make([]router.Route, 0, n)
	for i := range n {
		out = append(out, router.Route{
			Pattern: fmt.Sprintf("/docs/section-%d/page-%d", i%12, i),
			Label:   "Doc",
			Page:    simplePage,
			// A quarter of them are browser-rendered, which is roughly the
			// toy app's ratio and is what makes NeedsWasm allocate.
			Client: i%4 == 0,
		})
	}
	return out
}

func benchApp(routes ...router.Route) *App {
	return New(Config{
		Routes:   routes,
		Shell:    shell,
		NotFound: func(p string) templ.Component { return text("<p>404</p>") },
		Public:   fstest.MapFS{"app.css": {Data: []byte(strings.Repeat("body{color:red}", 100))}},
	})
}

func serve(b *testing.B, h http.Handler, target string, partial bool) {
	b.Helper()
	r := httptest.NewRequest("GET", target, nil)
	if partial {
		r.Header.Set("X-Partial", "1")
	}
	rec := httptest.NewRecorder()

	// One warm request outside the loop: the first render fills the buffer
	// pool and the static handler's hash cache, and measuring that once as if
	// it were the steady state is how a benchmark lies about a cold start.
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		b.Fatalf("%s: status %d", target, rec.Code)
	}
	b.SetBytes(int64(rec.Body.Len()))
	b.ReportMetric(float64(rec.Body.Len()), "resp-B")

	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
	}
}

// ---------------------------------------------------------------------------
// Page size: what does the framework cost on top of the markup?
// ---------------------------------------------------------------------------

func BenchmarkPageSimple(b *testing.B) {
	a := benchApp(router.Route{Pattern: "/about", Label: "About", Page: simplePage})
	serve(b, a.Mux(), "/about", false)
}

func BenchmarkPageSimplePartial(b *testing.B) {
	a := benchApp(router.Route{Pattern: "/about", Label: "About", Page: simplePage})
	serve(b, a.Mux(), "/about", true)
}

func BenchmarkPageComplex(b *testing.B) {
	rt := router.Route{
		Pattern: "/dashboard/metrics",
		Label:   "Metrics",
		Page:    tablePage(200),
		Head:    headComp,
		Layouts: []router.Wrapper{layout("main"), layout("div"), layout("section")},
		Client:  true,
	}
	a := benchApp(rt)
	serve(b, a.Mux(), "/dashboard/metrics", false)
}

func BenchmarkPageComplexPartial(b *testing.B) {
	rt := router.Route{
		Pattern: "/dashboard/metrics",
		Label:   "Metrics",
		Page:    tablePage(200),
		Head:    headComp,
		Layouts: []router.Wrapper{layout("main"), layout("div"), layout("section")},
		Client:  true,
	}
	a := benchApp(rt)
	serve(b, a.Mux(), "/dashboard/metrics", true)
}

// ---------------------------------------------------------------------------
// Route-table size: the same page, served from tables of different sizes. The
// page is identical, so any difference is what the runtime spends per request
// on routes it did not serve.
// ---------------------------------------------------------------------------

func BenchmarkRouteTable(b *testing.B) {
	for _, n := range []int{4, 32, 128} {
		b.Run(fmt.Sprintf("routes=%d", n), func(b *testing.B) {
			// Last, not first: router.Lookup returns on the first static
			// match, so a route at the head of the table would measure a
			// table of one however long the table is.
			routes := append(filler(n-1), router.Route{Pattern: "/about", Label: "About", Page: simplePage})
			serve(b, benchApp(routes...).Mux(), "/about", false)
		})
	}
}

// A dynamic route pays the same lookup twice: once in ServeMux, once in
// router.Lookup to fill the params the page reads.
func BenchmarkRouteDynamic(b *testing.B) {
	routes := append(filler(31), router.Route{
		Pattern: "/blog/{article_id}",
		Label:   "Article",
		Page: func() templ.Component {
			return comp(func(ctx context.Context, w io.Writer) error {
				_, err := io.WriteString(w, "<h1>"+router.Param(ctx, "article_id")+"</h1>")
				return err
			})
		},
	})
	serve(b, benchApp(routes...).Mux(), "/blog/fs-routing", false)
}

// ---------------------------------------------------------------------------
// The middleware chain the examples ship with.
// ---------------------------------------------------------------------------

func BenchmarkMiddlewareChain(b *testing.B) {
	rt := router.Route{Pattern: "/dashboard/metrics", Label: "Metrics", Page: tablePage(200)}

	b.Run("none", func(b *testing.B) {
		serve(b, benchApp(rt).Mux(), "/dashboard/metrics", false)
	})
	b.Run("recover+requestid", func(b *testing.B) {
		a := benchApp(rt).Use(mw.RequestID, mw.Recover(nil))
		serve(b, a.Wrap(a.Mux()), "/dashboard/metrics", false)
	})
	b.Run("compress", func(b *testing.B) {
		a := benchApp(rt).Use(mw.RequestID, mw.Recover(nil), mw.Compress{}.Handler)
		h := a.Wrap(a.Mux())
		r := httptest.NewRequest("GET", "/dashboard/metrics", nil)
		r.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		b.SetBytes(int64(rec.Body.Len()))
		b.ReportMetric(float64(rec.Body.Len()), "resp-B")
		b.ResetTimer()
		b.ReportAllocs()
		for range b.N {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
		}
	})
}

// ---------------------------------------------------------------------------
// Context assembly on its own: no markup, no HTTP, just what every render pays
// before the first byte of the page exists.
// ---------------------------------------------------------------------------

func BenchmarkContext(b *testing.B) {
	for _, n := range []int{4, 32, 128} {
		b.Run(fmt.Sprintf("routes=%d", n), func(b *testing.B) {
			a := benchApp(append(filler(n-1), router.Route{Pattern: "/about", Label: "About", Page: simplePage})...)
			ctx := context.Background()
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				a.context(ctx, "/about", nil)
			}
		})
	}
}

// Static files are served on the same request path as the page that references
// them, so their cost is part of a page view whether or not it is part of the
// render.
func BenchmarkStatic(b *testing.B) {
	a := benchApp(router.Route{Pattern: "/about", Label: "About", Page: simplePage})
	h := a.Mux()
	r := httptest.NewRequest("GET", "/static/app.css", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		b.Fatalf("static: status %d", rec.Code)
	}
	b.SetBytes(int64(rec.Body.Len()))
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
	}
}

// ---------------------------------------------------------------------------
// Over a real socket, with a real client: the number a user would measure.
// ---------------------------------------------------------------------------

func BenchmarkLoopback(b *testing.B) {
	cases := []struct {
		name  string
		route router.Route
		path  string
	}{
		{"simple", router.Route{Pattern: "/about", Label: "About", Page: simplePage}, "/about"},
		{"complex", router.Route{
			Pattern: "/dashboard/metrics",
			Label:   "Metrics",
			Page:    tablePage(200),
			Head:    headComp,
			Layouts: []router.Wrapper{layout("main"), layout("div"), layout("section")},
		}, "/dashboard/metrics"},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			a := benchApp(append(filler(31), c.route)...)
			srv := httptest.NewServer(a.Mux())
			defer srv.Close()
			client := srv.Client()

			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				resp, err := client.Get(srv.URL + c.path)
				if err != nil {
					b.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body) //nolint:errcheck
				resp.Body.Close()
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Render only: component -> HTML, no HTTP, no context, no shell.
//
// This is the layer a JS framework's renderToString occupies, and the only one
// that compares across languages without also comparing two HTTP stacks.
// ---------------------------------------------------------------------------

func BenchmarkRenderOnly(b *testing.B) {
	cases := []struct {
		name string
		page func() templ.Component
	}{
		{"simple", simplePage},
		{"complex", tablePage(200)},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			ctx := context.Background()
			var buf bytes.Buffer
			if err := c.page().Render(ctx, &buf); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(buf.Len()))
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				buf.Reset()
				c.page().Render(ctx, &buf) //nolint:errcheck
			}
		})
	}
}
