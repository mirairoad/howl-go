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
	"net/http"
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
