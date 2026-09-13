package observe

import (
	"context"
	"sync"
)

// Recorder is a Tracer that keeps every span it opened, for tests: the
// framework's own, and an application's that want to assert "this request
// touched the database once". Spans nest through the context the way real
// ones do, so Parent is set for a span opened with a context another span
// returned.
type Recorder struct {
	mu    sync.Mutex
	spans []*Recorded
}

// Recorded is what a Recorder kept of one span.
type Recorded struct {
	Name   string
	Parent *Recorded
	Attrs  map[string]any
	Events []string
	Err    error
	Ended  bool
}

type recordedKey struct{}

// Start implements Tracer.
func (r *Recorder) Start(ctx context.Context, name string) (context.Context, Span) {
	s := &Recorded{Name: name, Attrs: map[string]any{}}
	s.Parent, _ = ctx.Value(recordedKey{}).(*Recorded)
	r.mu.Lock()
	r.spans = append(r.spans, s)
	r.mu.Unlock()
	return context.WithValue(ctx, recordedKey{}, s), s
}

// Current implements Tracer.
func (r *Recorder) Current(ctx context.Context) Span {
	if s, ok := ctx.Value(recordedKey{}).(*Recorded); ok {
		return s
	}
	return noop{}
}

// Spans is everything recorded so far, in the order it was opened.
func (r *Recorder) Spans() []*Recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*Recorded(nil), r.spans...)
}

// Named is the recorded spans with this name.
func (r *Recorder) Named(name string) []*Recorded {
	var out []*Recorded
	for _, s := range r.Spans() {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func (s *Recorded) Set(key string, value any) { s.Attrs[key] = value }
func (s *Recorded) Event(name string)         { s.Events = append(s.Events, name) }
func (s *Recorded) End(err error)             { s.Err, s.Ended = err, true }
