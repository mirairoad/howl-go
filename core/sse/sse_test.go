package sse_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mirairoad/howl-go/core/sse"
)

func TestFrameCarriesEveryFieldInOrder(t *testing.T) {
	got := sse.Event{ID: "7", Name: "tick", Retry: 1500 * time.Millisecond, Data: "hello"}.Frame()
	want := "id: 7\nevent: tick\nretry: 1500\ndata: hello\n\n"
	if got != want {
		t.Fatalf("frame = %q, want %q", got, want)
	}
}

// An empty field is left out rather than sent empty: "event: " is not the same
// as no event name, and a browser dispatches the two differently.
func TestFrameLeavesOutWhatWasNotSet(t *testing.T) {
	got := sse.Event{Data: "hello"}.Frame()
	if got != "data: hello\n\n" {
		t.Fatalf("frame = %q", got)
	}
}

// The one that fails silently: a newline written as a single data: field ends
// the frame there, and the browser delivers the remainder as a second event.
func TestFrameSplitsAMultilinePayload(t *testing.T) {
	got := sse.Event{Data: "one\ntwo\nthree"}.Frame()
	want := "data: one\ndata: two\ndata: three\n\n"
	if got != want {
		t.Fatalf("frame = %q, want %q", got, want)
	}
}

func TestOpenSendsTheHeadersBeforeTheFirstFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	stream, err := sse.Open(rec, httptest.NewRequest("GET", "/events", nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	// nginx buffers a proxied response by default, which holds every frame
	// until the stream ends.
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q", got)
	}
	if !rec.Flushed {
		t.Fatal("the headers were not flushed — EventSource waits for the first frame to connect")
	}
	if err := stream.Send("tick", "1"); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != "event: tick\ndata: 1\n\n" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// A send to a caller who has gone away is a return, not an error to report:
// the browser reconnects on its own.
func TestSendRefusesOnceTheCallerIsGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/events", nil).WithContext(ctx)
	stream, err := sse.Open(httptest.NewRecorder(), req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := stream.Send("tick", "1"); err != http.ErrBodyNotAllowed {
		t.Fatalf("err = %v, want ErrBodyNotAllowed", err)
	}
	select {
	case <-stream.Done():
	default:
		t.Fatal("Done() is still open")
	}
}
