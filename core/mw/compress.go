package mw

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Compress gzips dynamic responses — HTML documents, SPA fragments, JSON.
// Static files are compressed by the static handler instead, which caches the
// compressed bytes rather than redoing the work on every request.
//
// Nothing is decided until the response has either produced MinSize bytes or
// finished, so a 200-byte JSON reply is not wrapped in a gzip frame that makes
// it bigger. A handler that flushes early (SSE) forces the decision at the
// first flush, since there is no "end" to wait for.
type Compress struct {
	// MinSize is the smallest body worth compressing. Default 1024.
	MinSize int
	// Level is a compress/gzip level. Default gzip.DefaultCompression.
	Level int
	// Types are Content-Type prefixes to compress. Default: text/*, JSON,
	// JavaScript, XML and SVG.
	Types []string
}

var defaultTypes = []string{
	"text/",
	"application/json",
	"application/javascript",
	"application/manifest+json",
	"application/xml",
	"image/svg+xml",
}

// Pooled writers are default-level ones. A zero-value gzip.Writer would be
// level 0 — NoCompression — and Reset keeps whatever level the writer was
// built with, so pooling those silently ships every response uncompressed
// inside a gzip frame, 0.5% *larger* than the original and with the
// Content-Encoding header claiming otherwise.
var gzPool = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

func (c Compress) Handler(next http.Handler) http.Handler {
	if c.MinSize == 0 {
		c.MinSize = 1024
	}
	if c.Level == 0 {
		c.Level = gzip.DefaultCompression
	}
	if len(c.Types) == 0 {
		c.Types = defaultTypes
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		cw := &compressor{ResponseWriter: w, cfg: c, status: http.StatusOK}
		defer cw.close()
		next.ServeHTTP(cw, r)
	})
}

type compressor struct {
	http.ResponseWriter
	cfg     Compress
	status  int
	decided bool
	gz      *gzip.Writer
	pooled  bool
	buf     []byte
}

func (c *compressor) WriteHeader(code int) { c.status = code }

func (c *compressor) Write(b []byte) (int, error) {
	if !c.decided {
		c.buf = append(c.buf, b...)
		if len(c.buf) < c.cfg.MinSize {
			return len(b), nil
		}
		c.decide(true)
		return len(b), nil
	}
	if c.gz != nil {
		return c.gz.Write(b)
	}
	return c.ResponseWriter.Write(b)
}

// decide commits to gzip or plain and sends the status line. big says the body
// is already past MinSize (or is being streamed, where size is unknowable).
func (c *compressor) decide(big bool) {
	c.decided = true
	h := c.ResponseWriter.Header()
	compressible := c.wants(h.Get("Content-Type"))
	if compressible {
		// The response differs by Accept-Encoding whether or not this
		// particular one got compressed — a shared cache must not serve one
		// client's gzip to a client that cannot read it.
		h.Add("Vary", "Accept-Encoding")
	}
	on := big && compressible &&
		h.Get("Content-Encoding") == "" &&
		c.status != http.StatusNoContent && c.status != http.StatusNotModified

	if on {
		// The length of the encoded body is not the length the handler set.
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		if c.cfg.Level == gzip.DefaultCompression {
			c.gz = gzPool.Get().(*gzip.Writer)
			c.gz.Reset(c.ResponseWriter)
			c.pooled = true
		} else {
			c.gz, _ = gzip.NewWriterLevel(c.ResponseWriter, c.cfg.Level)
		}
	}
	c.ResponseWriter.WriteHeader(c.status)
	if len(c.buf) > 0 {
		if c.gz != nil {
			c.gz.Write(c.buf) //nolint:errcheck // surfaces on Close
		} else {
			c.ResponseWriter.Write(c.buf) //nolint:errcheck
		}
		c.buf = nil
	}
}

func (c *compressor) wants(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	if ct == "" {
		return false // no Content-Type: sniffing already failed, do not guess twice
	}
	for _, t := range c.cfg.Types {
		if strings.HasPrefix(ct, t) {
			return true
		}
	}
	return strings.HasSuffix(ct, "+json") || strings.HasSuffix(ct, "+xml")
}

func (c *compressor) close() {
	if !c.decided {
		c.decide(false) // finished under MinSize: send it plain
	}
	if c.gz != nil {
		c.gz.Close() //nolint:errcheck
		if c.pooled {
			gzPool.Put(c.gz)
		}
		c.gz = nil
	}
}

// Flush is what SSE and long-polling need. There is no total size to wait for
// in a stream, so the first flush decides.
func (c *compressor) Flush() {
	if !c.decided {
		c.decide(true)
	}
	if c.gz != nil {
		c.gz.Flush() //nolint:errcheck
	}
	http.NewResponseController(c.ResponseWriter).Flush() //nolint:errcheck
}

func (c *compressor) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, q, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(name, "gzip") {
			return !strings.Contains(strings.ReplaceAll(q, " ", ""), "q=0")
		}
	}
	return false
}
