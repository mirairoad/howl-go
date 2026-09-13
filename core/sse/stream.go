// The half of the format that needs a response to write to.
//
// Untagged, so core/app — which has no build tags of its own and is compiled
// for GOOS=js even though nothing in a browser imports it — still finds it.
// That is affordable because the only importers of this package are servers:
// core/app and core/api's respond.go, which is itself excluded from the wasm
// build. core/api's shared half deliberately declares its own api.Event rather
// than importing this package, so a generated client never links net/http.
package sse

import (
	"fmt"
	"net/http"
)

// Open turns a response into an event stream: the headers go out immediately,
// so the browser's EventSource connects rather than waiting for the first
// frame.
func Open(w http.ResponseWriter, r *http.Request) (*Stream, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// Buffering proxies hold an event stream until the response ends, i.e.
	// forever. This is the header nginx reads to turn that off.
	h.Set("X-Accel-Buffering", "no")

	s := &Stream{w: w, ctl: http.NewResponseController(w), done: r.Context().Done()}
	if err := s.ctl.Flush(); err != nil {
		return nil, fmt.Errorf("sse: response is not flushable: %w", err)
	}
	return s, nil
}

// Stream is an open event stream.
type Stream struct {
	w    http.ResponseWriter
	ctl  *http.ResponseController
	done <-chan struct{}
}

// Send writes one named event. Sugar for the two fields that come up every
// time; SendEvent takes the rest.
func (s *Stream) Send(event, data string) error {
	return s.SendEvent(Event{Name: event, Data: data})
}

// SendEvent writes one frame and flushes it.
func (s *Stream) SendEvent(e Event) error {
	select {
	case <-s.done:
		return http.ErrBodyNotAllowed // client disconnected
	default:
	}
	if _, err := s.w.Write([]byte(e.Frame())); err != nil {
		return err
	}
	return s.ctl.Flush()
}

// Done closes when the client goes away.
func (s *Stream) Done() <-chan struct{} { return s.done }
