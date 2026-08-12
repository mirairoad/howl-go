package app

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Static serves an fs.FS with the three things net/http's FileServer leaves to
// you: an ETag, a Cache-Control, and a compressed copy.
//
// Compression happens once per file and is kept, which is the difference that
// matters at this scale. A 5.6 MB wasm binary gzipped on every request burns a
// core per download; gzipped once it costs 1.63 MB of memory and nothing per
// request. That is also why this holds files in memory: the FS is normally an
// embed.FS, so the bytes are in the binary already.
type Static struct {
	FS fs.FS
	// MaxAge is the Cache-Control lifetime. Zero means no-cache: the client
	// still revalidates every time, but an unchanged file answers 304 with no
	// body. Safe default for names without a content hash.
	MaxAge time.Duration
	// Immutable marks files that can never change under their name — anything
	// with a content hash in it. Those get a year and `immutable`, so the
	// browser does not even revalidate.
	Immutable func(name string) bool
	// Reload re-reads a file when it changes on disk. For a dev server pointed
	// at a directory instead of an embed.FS.
	//
	// It does NOT mean "redo the work every request": modification time and
	// size are compared first, and an unchanged file is served from the cache.
	// Getting that wrong cost 530 ms per request on a 6.94 MB wasm binary —
	// re-read, re-hashed and re-compressed every time, including on the 304s.
	Reload bool

	mu    sync.RWMutex
	cache map[string]*entry
}

type entry struct {
	ctype string
	raw   []byte
	gz    []byte // nil when compression did not pay
	etag  string
	// stamp is what the file looked like when this entry was built, so Reload
	// can tell "changed" from "asked for again".
	stamp stamp
}

type stamp struct {
	mod  time.Time
	size int64
}

// ---------------------------------------------------------------------------
// Content-hashed URLs
//
// A file served under its own name has to be revalidated: the browser cannot
// know it is unchanged without asking, so every page load costs a conditional
// request per asset — a round-trip that transfers nothing and answers 304.
//
// Under a name containing its content hash the question cannot arise. Change
// the file and the URL changes with it, so the response can promise a year of
// immutability and the browser never asks again.
//
// The hash is computed here rather than by a build step because it already
// exists: it is the ETag. That makes this work identically for an embed.FS in
// production and a watched directory in development, with nothing to generate
// and no renamed files on disk.
// ---------------------------------------------------------------------------

// Name returns the content-hashed file name for an asset — views.wasm becomes
// views.9f8c2a1b.wasm. Unknown files are returned unchanged, so a typo is a 404
// rather than a panic at render time.
func (s *Static) Name(name string) string {
	e, err := s.load(name)
	if err != nil {
		return name
	}
	ext := path.Ext(name)
	return strings.TrimSuffix(name, ext) + "." + strings.Trim(e.etag, `"`)[:8] + ext
}

// unhash splits views.9f8c2a1b.wasm into ("views.wasm", "9f8c2a1b").
func unhash(name string) (string, string) {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	dot := strings.LastIndex(stem, ".")
	if dot < 0 {
		return name, ""
	}
	hash := stem[dot+1:]
	if len(hash) != 8 || strings.Trim(hash, "0123456789abcdef") != "" {
		return name, ""
	}
	return stem[:dot] + ext, hash
}

// Handler serves the FS. The URL path is used as-is, so mount it with
// http.StripPrefix (Mux does this for /static/).
func (s *Static) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" || !fs.ValidPath(name) {
			http.NotFound(w, r)
			return
		}
		name, requested := unhash(name)
		e, err := s.load(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		h := w.Header()
		h.Set("Content-Type", e.ctype)
		// Immutable only when the hash asked for is the hash we have. A stale
		// hashed URL — a page cached across a deploy — still gets the current
		// bytes, but must not be told to keep them for a year.
		if requested != "" && requested == strings.Trim(e.etag, `"`)[:8] {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", s.cacheControl(name))
		}

		body, etag := e.raw, e.etag
		if e.gz != nil && acceptsGzip(r) {
			// A different encoding is a different representation, so it needs
			// its own validator — otherwise a cache can answer a plain request
			// with gzipped bytes it stored under the same ETag.
			body, etag = e.gz, strings.TrimSuffix(e.etag, `"`)+`-gz"`
			h.Set("Content-Encoding", "gzip")
			h.Add("Vary", "Accept-Encoding")
		}
		h.Set("ETag", etag)

		// ServeContent handles If-None-Match, Range and HEAD against the bytes
		// we hand it. The zero modtime keeps Last-Modified out of it: the ETag
		// is the validator, and an embed.FS has no meaningful mtime anyway.
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
	})
}

func (s *Static) cacheControl(name string) string {
	if s.Immutable != nil && s.Immutable(name) {
		return "public, max-age=31536000, immutable"
	}
	if s.MaxAge > 0 {
		return "public, max-age=" + strconv.Itoa(int(s.MaxAge.Seconds()))
	}
	return "public, no-cache"
}

func (s *Static) load(name string) (*entry, error) {
	s.mu.RLock()
	cached, ok := s.cache[name]
	s.mu.RUnlock()
	if ok && !s.Reload {
		return cached, nil
	}

	// Reload: the file may have changed, and usually has not. Comparing the
	// stat is microseconds; rebuilding a multi-megabyte entry is half a second.
	current, statErr := fs.Stat(s.FS, name)
	if ok && statErr == nil && cached.stamp.mod.Equal(current.ModTime()) && cached.stamp.size == current.Size() {
		return cached, nil
	}

	f, err := s.FS.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.IsDir() {
		return nil, fs.ErrNotExist
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(raw)
	e := &entry{
		ctype: contentType(name),
		raw:   raw,
		etag:  `"` + hex.EncodeToString(sum[:8]) + `"`,
	}
	if info, err := f.Stat(); err == nil {
		e.stamp = stamp{mod: info.ModTime(), size: info.Size()}
	}
	if compressible(e.ctype) && len(raw) >= 512 {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if _, err := zw.Write(raw); err == nil && zw.Close() == nil && buf.Len() < len(raw) {
			e.gz = buf.Bytes()
		}
	}

	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[string]*entry{}
	}
	s.cache[name] = e
	s.mu.Unlock()
	return e, nil
}

// Warm reads and compresses every file up front, so no request pays for it.
//
// Without this the first visitor after a restart waits for a 6.94 MB binary to
// be hashed and gzipped — half a second, attributed to the page rather than to
// the server that was not ready. It runs in the background at Listen: the
// server answers immediately, and by the time anything asks for the wasm it is
// already in memory.
func (s *Static) Warm() (files int, bytes int64) {
	fs.WalkDir(s.FS, ".", func(path string, d fs.DirEntry, err error) error { //nolint:errcheck
		if err != nil || d.IsDir() {
			return nil
		}
		e, err := s.load(path)
		if err != nil {
			return nil
		}
		files++
		bytes += int64(len(e.raw))
		return nil
	})
	return files, bytes
}

// contentType covers what mime misses on a bare system. wasm in particular:
// without application/wasm the browser refuses instantiateStreaming and the
// client renderer silently never loads.
func contentType(name string) string {
	switch path.Ext(name) {
	case ".wasm":
		return "application/wasm"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func compressible(ctype string) bool {
	ctype = strings.ToLower(strings.Split(ctype, ";")[0])
	switch {
	case strings.HasPrefix(ctype, "text/"),
		strings.HasPrefix(ctype, "application/json"),
		strings.HasPrefix(ctype, "application/wasm"),
		strings.HasPrefix(ctype, "application/xml"),
		strings.HasPrefix(ctype, "image/svg+xml"):
		return true
	}
	return false
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, q, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(name, "gzip") {
			return !strings.Contains(strings.ReplaceAll(q, " ", ""), "q=0")
		}
	}
	return false
}

// Hashed is the usual Immutable rule: a name carrying a content hash, like
// app.9f8c2a1b.css, can be cached forever because changing the file changes
// the name.
func Hashed(name string) bool {
	base := path.Base(name)
	parts := strings.Split(base, ".")
	if len(parts) < 3 {
		return false
	}
	h := parts[len(parts)-2]
	if len(h) < 8 {
		return false
	}
	for _, c := range h {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}
