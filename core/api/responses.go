// Responses that are not the JSON envelope.
//
// This is howl (TS)'s ctx.text, ctx.html, ctx.sse and ctx.stream, moved from a
// method on a context to the endpoint's response type. It has to be the type:
// Spec's R is what the OpenAPI document and the generated client are built
// from, so an endpoint answering text/plain has to say so in the one place
// both of those read. A handler that returned a body through the writer
// instead would be invisible to every generated artefact.
//
// This file is the shared half and compiles in the browser, because the
// generated client instantiates these types too — Spec[…, api.Raw] produces a
// client method returning api.Raw. Nothing here names http.ResponseWriter; the
// Respond methods that do are in respond.go, which the wasm build excludes.

package api

import (
	"io"
	"iter"
	"time"
)

// Raw is a response whose bytes the handler produced itself, in the media type
// it produced them in. [Text], [HTML], [XML] and [Bytes] build one.
//
//	var Robots = api.Define(api.Spec[api.None, api.None, api.Raw]{
//	    Name: "Robots",
//	    Path: "/robots.txt",
//	    Handler: func(r *api.Request[api.None, api.None]) (api.Raw, error) {
//	        return api.Text("User-agent: *\nDisallow: /\n"), nil
//	    },
//	})
type Raw struct {
	// ContentType is the media type, written verbatim. Empty is
	// application/octet-stream: the honest answer for bytes nobody described,
	// rather than a guess that leaves a browser sniffing.
	//
	// Outbound only. A Raw returned by the generated client carries the body
	// and the status and leaves this empty: the browser transport is
	// dom.Fetch, which returns those two and no headers, and widening its
	// signature for a field no caller of a typed client has asked for is not
	// worth the wasm it costs.
	ContentType string
	Body        []byte
	// Code is the status. Zero is 200.
	Code int
}

// Bytes is a response of the given media type — the general case, for the
// types that are not one of the three named below: application/manifest+json,
// text/csv, an image built in memory.
func Bytes(contentType string, body []byte) Raw {
	return Raw{ContentType: contentType, Body: body}
}

// Text is text/plain: robots.txt, security.txt, a plain-text export.
func Text(s string) Raw {
	return Raw{ContentType: "text/plain; charset=utf-8", Body: []byte(s)}
}

// HTML is text/html, for the one endpoint that answers with a document rather
// than data. A page belongs in the page tree; this is for the operator's tools
// that ship as part of the server — an API reference, a debug view.
func HTML(s string) Raw {
	return Raw{ContentType: "text/html; charset=utf-8", Body: []byte(s)}
}

// XML is application/xml: a sitemap, an RSS or Atom feed.
func XML(s string) Raw {
	return Raw{ContentType: "application/xml; charset=utf-8", Body: []byte(s)}
}

// WithStatus sets the status code — howl (TS)'s ctx.text(body, {status: 201}).
func (raw Raw) WithStatus(code int) Raw {
	raw.Code = code
	return raw
}

// Redirect answers with a Location header instead of a body.
type Redirect struct {
	// To is where the caller is sent. A site-relative target has its leading
	// slashes collapsed to one before it goes out: "//evil.com/" is a
	// protocol-relative URL, and a handler that builds one from user input is
	// an open redirect. Collapsed, it is the harmless path /evil.com/.
	To string
	// Code is the status. Zero is 302 Found, howl (TS)'s default. After a POST
	// 303 See Other is usually what is wanted: it tells the browser to follow
	// with a GET, so reloading the destination does not re-submit the form.
	Code int
}

// Stream writes the body itself, a piece at a time, for a response too large
// or too slow to hold in memory: an export, a log tail, a proxied download.
//
// Every write reaches the client rather than waiting for the handler to
// return, which is the whole difference between a stream and a slow response.
type Stream struct {
	// ContentType is the media type. Empty is application/octet-stream.
	ContentType string
	// Write is given the response body. An error it returns is logged and
	// nothing else: the status line and part of the body are already out, so
	// there is no 500 left to send. Anything that can fail before the first
	// byte belongs in the handler, where it can still be an error.
	Write func(w io.Writer) error
}

// Event is one Server-Sent Events frame.
//
// Field for field, core/sse.Event — which is what app.SSE sends from a plain
// handler, and what actually writes this one: respond.go converts with
// sse.Event(e), so the compiler refuses the build if the two ever stop
// matching. Declared here rather than aliased because core/sse's other half
// names net/http, and this file is compiled into the browser: importing it
// would cost a wasm binary 2.05 MB for a TLS stack it cannot use.
//
// See https://html.spec.whatwg.org/multipage/server-sent-events.html
type Event struct {
	// Data is the payload. A newline in it is handled — the frame repeats the
	// data: prefix for each line, which is what the wire format requires and
	// what a naive writer gets wrong, ending the frame early and delivering
	// the rest as a second one.
	Data string
	// Name is the event: field. Empty sends none, and the browser calls it
	// "message".
	Name string
	// ID is the event id the client echoes back in Last-Event-ID when it
	// reconnects. Empty sends none.
	ID string
	// Retry is the reconnection delay to suggest to the client. Zero sends
	// none and leaves the browser's own default — about three seconds.
	Retry time.Duration
}

// SSE streams Server-Sent Events.
//
//	Handler: func(r *api.Request[api.None, api.None]) (api.SSE, error) {
//	    ctx := r.Context()
//	    return api.SSE{Events: func(yield func(api.Event) bool) {
//	        for {
//	            select {
//	            case <-ctx.Done():
//	                return
//	            case t := <-ticks:
//	                if !yield(api.Event{Name: "tick", Data: t.String()}) {
//	                    return
//	                }
//	            }
//	        }
//	    }}, nil
//	},
type SSE struct {
	// Events yields frames until it returns. An iterator rather than a channel
	// so there is nothing to close and nothing to leak: when the caller goes
	// away yield returns false, and a generator that respects that — as the
	// example does — stops on the spot.
	//
	// The generator is still responsible for watching r.Context(): yield
	// reports a client that hung up only on the next frame, which for a stream
	// that is quiet for an hour is an hour late.
	Events iter.Seq[Event]
}
