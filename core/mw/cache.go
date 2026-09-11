package mw

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/mirairoad/howl-go/core/cache"
)

// HeaderCache says what the cache did with a request: "hit" when the response
// came from the store, "miss" when the handler ran. A request the cache
// declined to consider — a POST, a caller Key refused — carries neither.
const HeaderCache = "X-Howl-Cache"

// Cache keeps successful GET responses for TTL and answers repeats from the
// store without running the handler. It is the one implementation behind
// api.Spec.Cache and `//howl:cache` pages, and it works on any handler.
//
// Unlike Coalesce, this is a cache: a response is reused after its handler
// has finished, for up to TTL, so it can be stale by up to TTL. Use it where
// that is the point — a paid upstream, an expensive aggregate — not where a
// write must be visible on the next read.
//
// What it refuses, because each would be a bug:
//
//   - anything but GET;
//   - a request Key names "" — the caller's rule for who may share an entry;
//   - any status but 200: a cached 500 is an outage that outlives its cause;
//   - a response that sets a cookie, from the handler or from middleware
//     inside this one — a stored Set-Cookie hands every later caller the same
//     session or CSRF token;
//   - a response that flushes, which is a stream with no end to store, and one
//     past MaxBody.
//
// Request directives are ignored: a `Cache-Control: no-cache` from the client
// does not bypass the store. The store usually exists to protect something
// from exactly the traffic a client could generate by sending that header.
type Cache struct {
	// Store holds the entries. Nil is an in-process LRU of 1000 entries, per
	// handler — hand the same Store to every Cache that should share one.
	Store cache.Store
	// TTL is how long an entry is served. Zero or less disables the cache: the
	// handler is returned unwrapped.
	TTL time.Duration
	// Key names the entry a request reads and writes. Return "" to skip the
	// cache for that request. Nil shares one entry per URL among every caller
	// that sends no Cookie and no Authorization, and skips the cache for those
	// that do — the rule that cannot serve one person's response to another.
	Key func(*http.Request) string
	// Vary lists request headers that make two requests different, added to
	// whatever Key returned. X-Partial for a page: a fragment and a document
	// share a URL.
	Vary []string
	// MaxBody is the largest response stored. Default 8 MiB; a larger one is
	// still served, just not kept.
	MaxBody int
}

// stored is the envelope in front of the body: one JSON line, then the bytes.
// A line rather than a JSON field so the body is not base64'd into a third
// larger and a Redis entry stays readable.
type stored struct {
	Status int         `json:"s"`
	Header http.Header `json:"h,omitempty"`
	At     int64       `json:"t"` // unix seconds, for Age
}

func (c Cache) Handler(next http.Handler) http.Handler {
	if c.TTL <= 0 {
		return next
	}
	store := c.Store
	if store == nil {
		store = cache.NewLRU(1000)
	}
	maxBody := c.MaxBody
	if maxBody <= 0 {
		maxBody = 8 << 20
	}
	keyOf := c.Key
	if keyOf == nil {
		keyOf = sharedKey
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		name := keyOf(r)
		if name == "" {
			next.ServeHTTP(w, r)
			return
		}
		key := storeKey(name, r, c.Vary)

		if raw, ok := store.Get(r.Context(), key); ok && replayStored(w, raw) {
			return
		}

		w.Header().Set(HeaderCache, "miss")
		rec := &capture{ResponseWriter: w, before: w.Header().Clone(), max: maxBody}
		next.ServeHTTP(rec, r)

		if !rec.wrote || rec.status != http.StatusOK || rec.cookie || rec.skip {
			return
		}
		meta, err := json.Marshal(stored{Status: rec.status, Header: rec.header, At: time.Now().Unix()})
		if err != nil {
			return
		}
		value := make([]byte, 0, len(meta)+1+rec.body.Len())
		value = append(append(append(value, meta...), '\n'), rec.body.Bytes()...)
		// The response is already on its way; a client that hangs up now must
		// not cancel the write that would have saved the next caller the work.
		store.Set(context.WithoutCancel(r.Context()), key, value, c.TTL)
	})
}

// sharedKey is the default Key: one entry per URL, and none at all for a
// request that carries credentials. The query is re-encoded so ?b=2&a=1 and
// ?a=1&b=2 are the same entry.
func sharedKey(r *http.Request) string {
	if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
		return ""
	}
	return r.URL.Path + "?" + r.URL.Query().Encode()
}

// storeKey hashes the name, so a key never carries a cookie or a token into a
// shared store in the clear, and a long query string cannot make a key longer
// than a backend accepts.
func storeKey(name string, r *http.Request, vary []string) string {
	h := sha256.New()
	h.Write([]byte(name)) //nolint:errcheck // a hash.Hash never fails a write
	for _, v := range vary {
		h.Write([]byte{0})               //nolint:errcheck
		h.Write([]byte(r.Header.Get(v))) //nolint:errcheck
	}
	return "howl:http:" + hex.EncodeToString(h.Sum(nil))
}

// replayStored writes an entry. It reports false for bytes it cannot read, so
// a corrupt or foreign entry is a miss rather than a broken response.
func replayStored(w http.ResponseWriter, raw []byte) bool {
	nl := bytes.IndexByte(raw, '\n')
	if nl < 0 {
		return false
	}
	var meta stored
	if err := json.Unmarshal(raw[:nl], &meta); err != nil || meta.Status == 0 {
		return false
	}
	h := w.Header()
	for k, vs := range meta.Header {
		h[k] = vs
	}
	age := max(time.Now().Unix()-meta.At, 0)
	h.Set("Age", strconv.FormatInt(age, 10))
	h.Set(HeaderCache, "hit")
	w.WriteHeader(meta.Status)
	w.Write(raw[nl+1:]) //nolint:errcheck // the client is gone; nothing to do
	return true
}

// capture passes the response through untouched while keeping a copy of what
// the handler added: the headers it set, the status and the body. Unlike
// Coalesce's recorder it never holds the response back — the first caller is
// not made to wait for the store.
type capture struct {
	http.ResponseWriter
	// before is the header map as middleware left it, so what is stored is
	// what the handler added. Replaying a stored X-Request-Id would hand every
	// hit the id of the request that filled the entry.
	before http.Header
	header http.Header
	status int
	cookie bool
	wrote  bool
	body   bytes.Buffer
	max    int
	skip   bool // flushed, or past max: serve it, do not store it
}

func (c *capture) WriteHeader(code int) {
	if c.wrote {
		return
	}
	c.wrote, c.status = true, code
	current := c.ResponseWriter.Header()
	c.cookie = len(current["Set-Cookie"]) > 0
	c.header = added(c.before, current)
	c.ResponseWriter.WriteHeader(code)
}

func (c *capture) Write(b []byte) (int, error) {
	if !c.wrote {
		c.WriteHeader(http.StatusOK)
	}
	if !c.skip {
		if c.body.Len()+len(b) > c.max {
			c.skip = true
			c.body = bytes.Buffer{}
		} else {
			c.body.Write(b) //nolint:errcheck // bytes.Buffer never fails a write
		}
	}
	return c.ResponseWriter.Write(b)
}

func (c *capture) Flush() {
	c.skip = true
	if !c.wrote {
		c.WriteHeader(http.StatusOK)
	}
	http.NewResponseController(c.ResponseWriter).Flush() //nolint:errcheck // surfaces on the next write
}

func (c *capture) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// added is the headers in after that were not in before with the same values.
func added(before, after http.Header) http.Header {
	out := http.Header{}
	for k, vs := range after {
		if k == HeaderCache || slices.Equal(before[k], vs) {
			continue
		}
		out[k] = append([]string(nil), vs...)
	}
	return out
}
