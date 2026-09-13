package otel

import (
	"net/http"
	"strings"

	"github.com/mirairoad/howl-go/core/observe"
)

// Routes names every request after the mux pattern that served it, including
// the ones the framework never sees: a form post, a fragment endpoint, a
// webhook, anything registered with mux.HandleFunc rather than generated into
// the route table.
//
//	log.Fatal(a.Listen(otel.Routes(mux)))
//
// Without it those requests trace as a bare "POST" with no http.route, because
// App.render and the endpoint pipeline are the only places the framework knows
// a route — and neither runs for a handler put on the mux by hand. With it the
// span is "POST /api/todos" and the metric is labelled by the pattern rather
// than by the URL, which is the difference between one time series and one per
// id.
//
// The argument is an http.Handler rather than a *http.ServeMux because that is
// what an application has in hand — app.New returns its mux as one, and a
// sub-mux or a third-party router that sets Request.Pattern works the same way.
//
// It must wrap the mux directly, with no middleware in between. ServeMux sets
// Request.Pattern on the request it was handed, in place — so a middleware
// that clones the request (mw.RequestID does, and so does anything calling
// WithContext) hides the pattern from everything outside it. Use is applied
// around this by Listen, which is the right side of that line.
//
// A route the framework already named — a page, an endpoint — is not renamed:
// both agree on the pattern, and this is only the fallback for the rest.
func Routes(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		// Read after, not before: the pattern does not exist until the mux has
		// matched. The method prefix is dropped because the tracer puts the
		// request's own method back when it renames the span, and because
		// http.route is the path template in the conventions.
		if pattern := r.Pattern; pattern != "" {
			if i := strings.IndexByte(pattern, ' '); i >= 0 {
				pattern = pattern[i+1:]
			}
			observe.Current(r.Context()).Set("http.route", pattern)
		}
	})
}
