package app

import (
	"fmt"
	"net/http"
	"strings"
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
func SSE(w http.ResponseWriter, r *http.Request) (*Stream, error) {
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

// Send writes one event. A multi-line payload is split across data: lines, as
// the wire format requires — a raw newline would end the event early.
func (s *Stream) Send(event, data string) error {
	select {
	case <-s.done:
		return http.ErrBodyNotAllowed // client disconnected
	default:
	}
	var b strings.Builder
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteString("\n")
	}
	for _, line := range strings.Split(data, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	if _, err := s.w.Write([]byte(b.String())); err != nil {
		return err
	}
	return s.ctl.Flush()
}

// Done closes when the client goes away.
func (s *Stream) Done() <-chan struct{} { return s.done }
