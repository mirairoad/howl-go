// Package sse is the Server-Sent Events wire format, in one place.
//
// It is its own package because two layers need it and neither should own it:
// core/app.SSE opens a stream from a plain handler (the dev server's reload
// channel is one), and core/api answers an endpoint whose response type is
// api.SSE. Written twice, the two would drift, and a frame written slightly
// wrong does not fail — it delivers half an event and looks like data loss.
//
// event.go is the format and names nothing but the standard library's string
// handling; stream.go is the half that needs a response to write to.
package sse

import (
	"strconv"
	"strings"
	"time"
)

// Event is one frame.
//
// See https://html.spec.whatwg.org/multipage/server-sent-events.html
type Event struct {
	// Data is the payload.
	Data string
	// Name is the event: field. Empty sends none, and the browser calls it
	// "message".
	Name string
	// ID is the id the client echoes back in Last-Event-ID when it reconnects.
	// Empty sends none.
	ID string
	// Retry is the reconnection delay to suggest. Zero sends none and leaves
	// the browser's own default, which is about three seconds.
	Retry time.Duration
}

// Frame renders the event in the wire format, blank line included.
func (e Event) Frame() string {
	var b strings.Builder
	if e.ID != "" {
		b.WriteString("id: " + e.ID + "\n")
	}
	if e.Name != "" {
		b.WriteString("event: " + e.Name + "\n")
	}
	if e.Retry > 0 {
		b.WriteString("retry: " + strconv.FormatInt(e.Retry.Milliseconds(), 10) + "\n")
	}
	// One data: per line. A payload with a newline written as a single field
	// ends the frame at that newline and delivers the remainder as a second,
	// malformed one — which looks like data loss and is not.
	for _, line := range strings.Split(e.Data, "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n") // the blank line is what dispatches the event
	return b.String()
}
