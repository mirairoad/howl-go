// The server half of the responses that are not the JSON envelope: how each of
// the types in responses.go reaches the wire. Excluded from the wasm build,
// because every line of it names net/http — see responses.go for why the types
// themselves are not.
//go:build !(js && wasm)

package api

import (
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mirairoad/howl-go/core/sse"
)

// Responder is a response that writes itself: the status line, the headers and
// the body. Return one from a handler when the answer is not JSON.
//
// [Raw], [Redirect], [Stream] and [SSE] implement it, and an application may
// implement it too — a file server, a protobuf encoder — which is why the
// method is exported. Whatever the handler put on r.Header() is already on the
// response when Respond is called; Content-Type is the one header a Responder
// owns, because it is the one the JSON path overwrites.
type Responder interface {
	Respond(w http.ResponseWriter, r *http.Request)
}

var (
	_ Responder = Raw{}
	_ Responder = Redirect{}
	_ Responder = Stream{}
	_ Responder = SSE{}
)

// defaultContentType is what bytes nobody described are sent as. Not
// text/plain: a browser shows text inline, and an endpoint that forgot to say
// what it produced should not have its output rendered as a page.
const defaultContentType = "application/octet-stream"

func (raw Raw) Respond(w http.ResponseWriter, _ *http.Request) {
	contentType := raw.ContentType
	if contentType == "" {
		contentType = defaultContentType
	}
	code := raw.Code
	if code == 0 {
		code = http.StatusOK
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(code)
	w.Write(raw.Body) //nolint:errcheck // the client hung up; there is nothing to say
}

func (red Redirect) Respond(w http.ResponseWriter, r *http.Request) {
	code := red.Code
	if code == 0 {
		code = http.StatusFound
	}
	http.Redirect(w, r, siteRelative(red.To), code)
}

// siteRelative collapses the leading slashes of a site-relative target.
//
// "//evil.com/" is a protocol-relative URL: a browser reads it as
// https://evil.com/, so a handler that builds a redirect out of a ?next=
// parameter is an open redirect. Collapsed to "/evil.com/" it is a path on
// this site, which is a 404 and not somebody else's login form. howl (TS) does
// the same thing in ctx.redirect.
//
// Only the leading run is collapsed, and only the path: an interior "//" is a
// real (if odd) segment, and rewriting it would change URLs that work.
func siteRelative(to string) string {
	if !strings.HasPrefix(to, "//") {
		return to
	}
	path, rest := to, ""
	if i := strings.IndexAny(to, "?#"); i >= 0 {
		path, rest = to[:i], to[i:]
	}
	return "/" + strings.TrimLeft(path, "/") + rest
}

func (s Stream) Respond(w http.ResponseWriter, r *http.Request) {
	contentType := s.ContentType
	if contentType == "" {
		contentType = defaultContentType
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	if s.Write == nil {
		return
	}
	if err := s.Write(&flushing{w: w, rc: http.NewResponseController(w)}); err != nil {
		// The status line is out and part of the body with it; there is no 500
		// left to send. The log is the only place this can be reported, and it
		// is why anything that can fail before the first byte belongs in the
		// handler instead.
		slog.Error("api: stream", slog.String("path", r.URL.Path), slog.Any("err", err))
	}
}

// flushing pushes every write to the client instead of letting it sit in the
// server's buffer until the handler returns. Without it a stream is an
// ordinary response that took a long time to build.
//
// Through http.ResponseController rather than a type assertion to
// http.Flusher: by the time a response reaches here it is wrapped by whatever
// middleware the application installed, and a wrapper that does not forward
// Flush is the usual reason a stream arrives all at once. A transport that
// cannot flush — http.ErrNotSupported — is not an error worth failing a
// response over; the bytes still arrive, just later.
type flushing struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f *flushing) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	f.rc.Flush() //nolint:errcheck // see the type's comment
	return n, err
}

func (s SSE) Respond(w http.ResponseWriter, r *http.Request) {
	stream, err := sse.Open(w, r)
	if err != nil {
		// Nothing has been written but the headers, so this can still be an
		// honest failure. It means a middleware wrapped the writer without
		// forwarding Flush, which is a bug in the application, not the caller.
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if s.Events == nil {
		return
	}
	for event := range s.Events {
		if stream.SendEvent(sse.Event(event)) != nil {
			return // the caller hung up; the browser reconnects on its own
		}
	}
}
