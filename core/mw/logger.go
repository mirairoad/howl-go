package mw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Request id
// ---------------------------------------------------------------------------

// HeaderRequestID is both read and written: a proxy that already assigned an
// id keeps it, so one request has one id across every hop.
const HeaderRequestID = "X-Request-Id"

type ctxKey int

const (
	requestIDKey ctxKey = iota
	nonceKey
	csrfKey
)

// RequestID puts an id on the request context and echoes it in the response.
// An inbound id is trusted only if it is short and printable — it ends up in
// logs and headers, so a caller must not be able to inject newlines into them.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !safeID(id) {
			id = newID()
		}
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// ID returns the request id, or "" when RequestID is not in the chain.
func ID(ctx context.Context) string {
	s, _ := ctx.Value(requestIDKey).(string)
	return s
}

func safeID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------

// Logger logs one line per request after it completes: method, path, status,
// bytes and duration, plus the request id when RequestID ran first. Pass nil
// for slog.Default().
//
// The level follows the status, so a log filtered to warnings shows exactly
// the requests that failed. Pair it with core/console to get that in colour.
func Logger(l *slog.Logger) Middleware { return LogWith(LogOptions{Logger: l}) }

// LogOptions is Logger with the two knobs a real deployment wants.
type LogOptions struct {
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Callers adds the caller's ip and user agent for requests that did NOT
	// come from a page on this host — an API client, a scraper, a telemetry
	// exporter, curl. Your own SPA's fetches stay quiet, because they carry a
	// same-origin Referer or Sec-Fetch-Site, and logging your own IP on every
	// navigation is noise that hides the thing you wanted to see.
	Callers bool
	// TrustProxy reads the client address from X-Forwarded-For. Only set this
	// behind a proxy that overwrites the header — anyone can send it.
	TrustProxy bool
	// Skip drops requests from the log entirely. Static assets and health
	// checks are the usual candidates: see SkipNoise.
	Skip func(*http.Request) bool
}

func LogWith(o LogOptions) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if o.Skip != nil && o.Skip(r) {
				next.ServeHTTP(w, r)
				return
			}
			log := o.Logger
			if log == nil {
				log = slog.Default()
			}
			start := time.Now()
			rw := Wrap(w)
			next.ServeHTTP(rw, r)

			attrs := []any{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rw.Status),
				slog.Int("bytes", rw.Bytes),
				slog.Duration("took", roundDuration(time.Since(start))),
			}
			if id := ID(r.Context()); id != "" {
				attrs = append(attrs, slog.String("id", id))
			}
			// A fragment and a document for the same URL are different work;
			// without this the SPA path is invisible in the log.
			if r.Header.Get("X-Partial") == "1" {
				attrs = append(attrs, slog.Bool("partial", true))
			}
			if o.Callers && !fromThisSite(r) {
				attrs = append(attrs, slog.String("ip", clientIP(r, o.TrustProxy)))
				if ua := r.Header.Get("User-Agent"); ua != "" {
					attrs = append(attrs, slog.String("ua", truncate(ua, 48)))
				}
			}

			switch {
			case rw.Status >= 500:
				log.Error("http", attrs...)
			case rw.Status >= 400:
				log.Warn("http", attrs...)
			default:
				log.Info("http", attrs...)
			}
		})
	}
}

// SkipNoise drops static assets and health checks. They are the majority of
// requests and the least informative line in any log.
func SkipNoise(r *http.Request) bool {
	p := r.URL.Path
	return strings.HasPrefix(p, "/static/") ||
		p == "/favicon.ico" || p == "/robots.txt" ||
		p == "/healthz" || p == "/livez" || p == "/readyz"
}

// fromThisSite reports whether the request came from a page this server
// served. Three signals, cheapest first; any one of them is enough.
func fromThisSite(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "same-site", "none":
		// "none" is a typed URL or a bookmark — a person on this site, not a
		// program calling the API.
		return true
	case "cross-site":
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		return sameHost(o, r.Host)
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		return sameHost(ref, r.Host)
	}
	// No Origin, no Referer, no Fetch metadata: not a browser on this site.
	return false
}

func sameHost(rawURL, host string) bool {
	u, err := url.Parse(rawURL)
	return err == nil && strings.EqualFold(u.Host, host)
}

func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			first, _, _ := strings.Cut(fwd, ",")
			if first = strings.TrimSpace(first); first != "" {
				return first
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// roundDuration keeps the log readable: nobody needs 1.203847ms.
func roundDuration(d time.Duration) time.Duration {
	switch {
	case d > time.Second:
		return d.Round(time.Millisecond)
	case d > time.Millisecond:
		return d.Round(10 * time.Microsecond)
	}
	return d.Round(time.Microsecond)
}

// ---------------------------------------------------------------------------
// Panics
// ---------------------------------------------------------------------------

// Recover turns a panic into a 500 instead of a dropped connection. onPanic is
// optional; the default logs the value and the stack under the request id.
//
// Once the status line is out there is no 500 left to send — the response is
// already being read — so in that case the connection is closed by the panic
// propagating, which is the honest signal that the body is truncated.
func Recover(onPanic func(w http.ResponseWriter, r *http.Request, v any)) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := Wrap(w)
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler { // deliberate, not a bug
					panic(v)
				}
				slog.Error("panic",
					slog.Any("value", v),
					slog.String("path", r.URL.Path),
					slog.String("id", ID(r.Context())),
					slog.String("stack", string(debug.Stack())),
				)
				if rw.Wrote() {
					panic(v) // too late to answer; let the server tear it down
				}
				if onPanic != nil {
					onPanic(rw, r, v)
					return
				}
				http.Error(rw, "internal server error", http.StatusInternalServerError)
			}()
			next.ServeHTTP(rw, r)
		})
	}
}
