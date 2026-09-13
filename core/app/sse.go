package app

import (
	"net/http"

	"github.com/mirairoad/howl-go/core/sse"
)

// SSE turns a handler into a server-sent event stream. It is the one piece of
// streaming the framework needs for itself — a dev server telling the browser
// to reload — and the cheapest way for an application to push without a
// WebSocket dependency.
//
//	func handler(w http.ResponseWriter, r *http.Request) {
//	    s, err := app.SSE(w, r)
//	    if err != nil { http.Error(w, err.Error(), 500); return }
//	    for range ticker.C {
//	        if s.Send("tick", time.Now().String()) != nil { return } // client gone
//	    }
//	}
//
// The browser reconnects on its own after a dropped connection, so a Send that
// fails is a return, not an error to report.
//
// The wire format lives in core/sse, because core/api needs it too: an
// endpoint whose response type is api.SSE is the same stream declared as part
// of the route table. This is the door for a plain handler.
func SSE(w http.ResponseWriter, r *http.Request) (*Stream, error) {
	return sse.Open(w, r)
}

// Stream is an open event stream. An alias rather than a wrapper: app.SSE and
// an api.SSE endpoint hand back the same thing, and code that takes one should
// not have to care which opened it.
type Stream = sse.Stream

// Event is one frame, for Stream.SendEvent — an id or a retry hint, which
// Send's two arguments leave out.
type Event = sse.Event
