package mw

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
)

// Coalesce collapses identical concurrent requests into one execution. Ten
// clients asking for the same page at the same instant produce one render, and
// all ten receive its bytes.
//
// This is a thundering-herd guard, not a cache: nothing is kept after the last
// waiter is served, so a response is never stale.
//
// What it refuses to share, because sharing would be a bug:
//
//   - anything but GET and HEAD;
//   - requests carrying Cookie or Authorization, since the response is
//     probably specific to that user;
//   - responses that set a cookie — replaying one Set-Cookie to every waiter
//     would hand them all the same session or CSRF token;
//   - responses larger than MaxBody, which are streamed to the leader while
//     the waiters simply run the handler themselves.
type Coalesce struct {
	// Vary lists request headers that make two requests different. The default
	// is X-Partial: an SPA fragment and a full document share a URL but are not
	// the same response.
	Vary []string
	// MaxBody caps what will be held in memory per in-flight request.
	// Default 8 MiB.
	MaxBody int

	mu    sync.Mutex
	calls map[string]*call
}

type call struct {
	done   chan struct{}
	status int
	header http.Header
	body   []byte
	shared bool // false: waiters must run the handler themselves
}

func (c *Coalesce) Handler(next http.Handler) http.Handler {
	vary := c.Vary
	if len(vary) == 0 {
		vary = []string{"X-Partial"}
	}
	maxBody := c.MaxBody
	if maxBody == 0 {
		maxBody = 8 << 20
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !shareable(r) {
			next.ServeHTTP(w, r)
			return
		}
		key := coalesceKey(r, vary)

		c.mu.Lock()
		if c.calls == nil {
			c.calls = map[string]*call{}
		}
		if existing, ok := c.calls[key]; ok {
			c.mu.Unlock()
			<-existing.done
			if existing.shared {
				replay(w, existing)
				return
			}
			next.ServeHTTP(w, r) // leader's response was not shareable
			return
		}
		cl := &call{done: make(chan struct{})}
		c.calls[key] = cl
		c.mu.Unlock()

		rec := &recorder{ResponseWriter: w, status: http.StatusOK, max: maxBody}
		completed := false
		func() {
			// The waiters are released here rather than after the leader has
			// written its own copy — they have the bytes already, and holding
			// them for the leader's socket would serialise what this middleware
			// exists to parallelise.
			defer func() {
				c.mu.Lock()
				delete(c.calls, key)
				c.mu.Unlock()
				cl.status, cl.header, cl.body = rec.status, w.Header().Clone(), rec.buf.Bytes()
				// A panic leaves a half-written body behind: let the waiters run
				// the handler themselves rather than serve them the wreckage.
				cl.shared = completed && !rec.overflowed && cl.header.Get("Set-Cookie") == ""
				close(cl.done)
			}()
			next.ServeHTTP(rec, r)
			completed = true
		}()
		rec.finish()
	})
}

func shareable(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	// An event stream has no end to buffer up to. The recorder below holds a
	// response until the handler returns, so coalescing one means the browser
	// sees nothing until the stream closes — which for SSE is never. The
	// framework ships app.SSE, so this is a case it has to know about rather
	// than a caller's problem to work around.
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		return false
	}
	return r.Header.Get("Cookie") == "" && r.Header.Get("Authorization") == ""
}

func coalesceKey(r *http.Request, vary []string) string {
	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteByte(' ')
	b.WriteString(r.URL.RequestURI())
	for _, h := range vary {
		b.WriteByte('\x00')
		b.WriteString(r.Header.Get(h))
	}
	return b.String()
}

func replay(w http.ResponseWriter, c *call) {
	dst := w.Header()
	for k, vs := range c.header {
		dst[k] = append([]string(nil), vs...)
	}
	w.WriteHeader(c.status)
	w.Write(c.body) //nolint:errcheck
}

// recorder buffers the leader's response so it can be handed to the waiters,
// and stops buffering — without stopping the response — once it gets too big.
type recorder struct {
	http.ResponseWriter
	status     int
	buf        bytes.Buffer
	max        int
	overflowed bool
	wrote      bool
}

func (r *recorder) WriteHeader(code int) { r.status = code }

func (r *recorder) Write(b []byte) (int, error) {
	if r.overflowed {
		return r.ResponseWriter.Write(b)
	}
	if r.buf.Len()+len(b) > r.max {
		r.overflowed = true
		r.flushHeader()
		if _, err := r.ResponseWriter.Write(r.buf.Bytes()); err != nil {
			return 0, err
		}
		r.buf.Reset()
		return r.ResponseWriter.Write(b)
	}
	return r.buf.Write(b)
}

func (r *recorder) flushHeader() {
	if r.wrote {
		return
	}
	r.wrote = true
	r.ResponseWriter.WriteHeader(r.status)
}

// finish sends the buffered response to the leader itself.
func (r *recorder) finish() {
	if r.overflowed {
		return
	}
	r.flushHeader()
	r.ResponseWriter.Write(r.buf.Bytes()) //nolint:errcheck
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
