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

	"github.com/mirairoad/howl-go/core/mw"
)

// Static serves an fs.FS with the three things net/http's FileServer leaves to
// you: an ETag, a Cache-Control, and a compressed copy.
//
// Compression happens once per file and is kept, which is the difference that
// matters at this scale — once per process, not per Static: every App in a test
// binary serves the same embedded files (compressions). A 8.3 MB wasm binary gzipped on every request burns a
// core per download; gzipped once it costs 2.27 MB of memory and nothing per
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

	// names is every file in FS, listed once, so Middleware can pass a request
	// for a page on without touching the FS. Unused under Reload, where a file
	// may appear at any moment.
	namesOnce sync.Once
	names     map[string]bool
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

// StaticFiles serves the files in fsys at the site root — /favicon.ico,
// /robots.txt, /.well-known/security.txt — and passes every other request on:
// howl (TS)'s staticFiles().
//
//	Use: []mw.Middleware{app.StaticFiles(client.Root())}
//
// /static/ already serves the application's Public files; this is for the
// names that browsers, crawlers and other tools ask for at the root and will
// not look for anywhere else. A file shadows a page of the same path, which
// is the point for /robots.txt and a surprise for anything else, so give it
// its own directory rather than pointing it at Public.
//
// The same handler as /static/: an ETag, a Cache-Control, compressed once.
func StaticFiles(fsys fs.FS) mw.Middleware {
	return (&Static{FS: fsys}).Middleware
}

// Middleware serves a GET or HEAD for a file s has, and hands everything else
// to next: other methods, directories, and names it does not have.
func (s *Static) Middleware(next http.Handler) http.Handler {
	files := s.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" || !fs.ValidPath(name) {
			next.ServeHTTP(w, r)
			return
		}
		if base, _ := unhash(name); !s.has(base) {
			next.ServeHTTP(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}

// has reports whether name is a file in the FS. Every page request passes
// through Middleware, so the answer for a name that is not there must not cost
// an open: the FS is listed once. Not remembered per miss, which a stream of
// made-up URLs would grow without bound.
func (s *Static) has(name string) bool {
	if s.Reload {
		_, err := s.load(name)
		return err == nil
	}
	s.namesOnce.Do(func() {
		s.names = map[string]bool{}
		fs.WalkDir(s.FS, ".", func(p string, d fs.DirEntry, err error) error { //nolint:errcheck
			if err == nil && !d.IsDir() {
				s.names[p] = true
			}
			return nil
		})
	})
	return s.names[name]
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
		if s.Reload {
			e.gz = compress(raw)
		} else {
			e.gz = compressOnce(sum, raw)
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

// compressions is the gzip of every file a Static in this process has
// compressed, by the SHA-256 of its bytes — the sum load already takes for the
// ETag — and shared by every Static that serves the same bytes.
//
// Once per file per process, rather than once per Static. A test suite starts
// an App per test, and every App warms the same embedded files. Sharing it
// cannot serve the wrong bytes, because the key is the content: two Statics
// share a copy only when they hold the same file.
//
// It also closes a race inside one Static. Warm runs in the background at
// Listen, and the first page's request for the wasm arrives while it is still
// at it, so that request compressed the file a second time; the Once makes the
// later of the two wait for the first.
//
// Measured on factory's browser suite, 45 flows and an App each, over an
// 11.2 MB views.wasm at 979 ms of CPU a time: 90 compressions a run — exactly
// two per App — became 10, one per test binary. The suite went from 280 to 106
// CPU-seconds and from 32 s to 21 s on 16 cores.
//
// A Static that reloads keeps its compression to itself (compress, not this).
// Its files change, and this table is never emptied, so every edit to a
// watched file would be held for the life of the process. Without Reload a
// Static reads each file once and keeps it for its own life anyway, which
// bounds this table by the bytes those Statics serve — for an application,
// the files compiled into it.
var compressions sync.Map // [sha256.Size]byte → *compression

type compression struct {
	once sync.Once
	gz   []byte // nil when compression did not pay
}

func compressOnce(sum [sha256.Size]byte, raw []byte) []byte {
	v, _ := compressions.LoadOrStore(sum, new(compression))
	c := v.(*compression)
	c.once.Do(func() { c.gz = compress(raw) })
	return c.gz
}

// compress is raw at gzip's best compression, or nil when the result is not
// smaller: a file that does not shrink is served as it is.
func compress(raw []byte) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write(raw); err != nil || zw.Close() != nil || buf.Len() >= len(raw) {
		return nil
	}
	return buf.Bytes()
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
