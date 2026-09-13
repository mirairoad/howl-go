// Package mw is the middleware layer: ordinary net/http decorators, composed
// once and wrapped around the whole mux.
//
// There is no framework-specific handler signature and no context wrapper. A
// middleware is `func(http.Handler) http.Handler`, which is what every other
// Go router already speaks — anything written for chi, gorilla or the standard
// library drops in unchanged, and anything written here works outside howl-go.
//
//	a := app.New(app.Config{
//	    Use: []mw.Middleware{
//	        mw.RequestID,
//	        mw.Logger(nil),
//	        mw.Recover(nil),
//	        mw.Compress{}.Handler,
//	    },
//	})
//
// Order is outermost first: the first entry sees the request first and the
// response last.
package mw

import (
	"context"
	"net/http"
	"strings"

	"github.com/mirairoad/howl-go/core/observe"
)

// Middleware decorates a handler. The zero-dependency shape on purpose.
type Middleware func(http.Handler) http.Handler

// Chain wraps h with ms, outermost first: Chain(h, a, b) calls a, then b, then
// h. Applying them in reverse here is what makes the call site read in the
// order they run.
func Chain(h http.Handler, ms ...Middleware) http.Handler {
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i] == nil {
			continue
		}
		h = ms[i](h)
	}
	return h
}

// Only applies ms to requests under prefix and lets every other request skip
// them — a guard for /admin, a stricter CSP for /embed. Matched a whole segment
// at a time: /admin covers /admin and /admin/users, never /administrator.
//
// A function rather than a Config.UseFor map because it composes where the
// chain is written, keeps its place in the order, and is a Middleware like any
// other, so it nests and works outside howl-go.
//
//	Use: []mw.Middleware{
//	    mw.RequestID,
//	    mw.Only("/admin", requireAdmin),
//	}
func Only(prefix string, ms ...Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		inner := Chain(next, ms...)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if Under(r.URL.Path, prefix) {
				inner.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Except applies ms to every request not under prefix: CSRF for the pages and
// their forms, but not for a JSON API that authenticates with a bearer token.
//
//	mw.Except("/api", mw.CSRF{Secure: true}.Handler)
func Except(prefix string, ms ...Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		inner := Chain(next, ms...)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if Under(r.URL.Path, prefix) {
				next.ServeHTTP(w, r)
				return
			}
			inner.ServeHTTP(w, r)
		})
	}
}

// Under reports whether path is prefix or below it, a whole segment at a time.
// A plain strings.HasPrefix would put /administrator under /admin, and a guard
// written with it protects a page it was never meant to — or, used to exempt
// a path, exempts one it was never meant to.
func Under(path, prefix string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return true
	}
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	return len(path) == len(prefix) || path[len(prefix)] == '/'
}

// Writer records what a handler did to the response so middleware above it can
// log or react. Every middleware here shares one, so a chain of five does not
// stack five wrappers: Wrap returns the existing one when it finds it.
type Writer struct {
	http.ResponseWriter
	Status int // 200 unless the handler said otherwise
	Bytes  int
	wrote  bool
}

// Wrap returns w as a *Writer, reusing it if it already is one.
func Wrap(w http.ResponseWriter) *Writer {
	if rw, ok := w.(*Writer); ok {
		return rw
	}
	return &Writer{ResponseWriter: w, Status: http.StatusOK}
}

func (w *Writer) WriteHeader(code int) {
	if w.wrote {
		return // net/http would log "superfluous WriteHeader"; swallow it once
	}
	w.wrote = true
	w.Status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *Writer) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.Bytes += n
	return n, err
}

// Wrote reports whether a status line has already gone out — the difference
// between "I can still send a 500" and "the client is already reading a 200".
func (w *Writer) Wrote() bool { return w.wrote }

// Unwrap lets http.ResponseController reach the real writer, so Flush, Hijack
// and SetWriteDeadline keep working through the chain. Flush is forwarded
// explicitly as well because plenty of code still type-asserts http.Flusher.
func (w *Writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *Writer) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	//nolint:errcheck // a failed flush surfaces on the next write
	http.NewResponseController(w.ResponseWriter).Flush()
}

// Named gives a middleware a span of its own, so a guard — or anything else
// in Use — shows up in a trace as itself and not as time unaccounted for
// between the request and the render.
//
//	mw.Named("guard.session", guards.Session)
//
// The span covers what the middleware does before it hands over: it ends the
// moment next is entered, so the guard's own cost stands apart from the page
// behind it. A middleware that answers instead — a redirect to sign-in, a
// refusal — keeps the span to the end of its reply, and the status and the
// Location it wrote are recorded on it: the answer to "why did the dashboard
// bounce this user".
func Named(name string, m Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		// Constructed once: a middleware may hold state (Coalesce does), and
		// the per-request part is what travels on the context.
		inner := m(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s, ok := r.Context().Value(namedKey{}).(*namedSpan); ok {
				s.pass()
			}
			next.ServeHTTP(w, r)
		}))
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, span := observe.Start(r.Context(), name)
			span.Set(observe.Kind, "middleware")
			ns := &namedSpan{span: span}
			rw := Wrap(w)
			inner.ServeHTTP(rw, r.WithContext(context.WithValue(ctx, namedKey{}, ns)))
			if !ns.passed {
				span.Set("howl.passed", false)
				span.Set("http.response.status_code", rw.Status)
				if loc := rw.Header().Get("Location"); loc != "" {
					span.Set("howl.redirect", loc)
				} else if loc := rw.Header().Get("X-Howl-Location"); loc != "" {
					span.Set("howl.redirect", loc)
				}
				span.End(nil)
			}
		})
	}
}

type namedKey struct{}

type namedSpan struct {
	span   observe.Span
	passed bool
}

func (n *namedSpan) pass() {
	if n.passed {
		return
	}
	n.passed = true
	n.span.Set("howl.passed", true)
	n.span.End(nil)
}
