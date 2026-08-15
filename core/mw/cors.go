package mw

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CORS answers preflights and stamps the cross-origin headers. The zero value
// allows nothing, which is the right default for a server that renders its own
// pages — reach for this only when a browser on another origin calls your API.
//
//	a.Use(mw.CORS{
//	    Origins: []string{"https://app.example.com"},
//	    Methods: []string{"GET", "POST"},
//	}.Handler)
type CORS struct {
	// Origins is matched exactly against the request's Origin header. A single
	// "*" allows everything.
	Origins []string
	// Methods and Headers are what a preflight is told it may use. Empty
	// Methods means GET/HEAD/POST; empty Headers echoes what was asked for.
	Methods []string
	Headers []string
	// ExposeHeaders lists response headers JS may read beyond the CORS-safe
	// set. X-Title lives here for an SPA fetching fragments cross-origin.
	ExposeHeaders []string
	// Credentials allows cookies. Note that "*" and credentials are mutually
	// exclusive per spec, so with both set the request's own origin is echoed
	// back instead — which means every origin that asks is allowed to send
	// cookies. Only do that if you meant it.
	Credentials bool
	MaxAge      time.Duration
}

func (c CORS) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r) // not a cross-origin request at all
			return
		}
		allow, ok := c.allow(origin)
		if !ok {
			// Not allowed: send no CORS headers and let the browser refuse. A
			// 403 here would be a worse error message than the browser's.
			if isPreflight(r) {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		h := w.Header()
		h.Set("Access-Control-Allow-Origin", allow)
		if allow != "*" {
			h.Add("Vary", "Origin") // the answer differs per origin; caches must know
		}
		if c.Credentials {
			h.Set("Access-Control-Allow-Credentials", "true")
		}
		if len(c.ExposeHeaders) > 0 {
			h.Set("Access-Control-Expose-Headers", strings.Join(c.ExposeHeaders, ", "))
		}

		if !isPreflight(r) {
			next.ServeHTTP(w, r)
			return
		}

		h.Add("Vary", "Access-Control-Request-Method")
		h.Add("Vary", "Access-Control-Request-Headers")
		methods := c.Methods
		if len(methods) == 0 {
			methods = []string{http.MethodGet, http.MethodHead, http.MethodPost}
		}
		h.Set("Access-Control-Allow-Methods", strings.Join(methods, ", "))
		if len(c.Headers) > 0 {
			h.Set("Access-Control-Allow-Headers", strings.Join(c.Headers, ", "))
		} else if want := r.Header.Get("Access-Control-Request-Headers"); want != "" {
			h.Set("Access-Control-Allow-Headers", want)
		}
		if c.MaxAge > 0 {
			h.Set("Access-Control-Max-Age", strconv.Itoa(int(c.MaxAge.Seconds())))
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (c CORS) allow(origin string) (string, bool) {
	for _, o := range c.Origins {
		if o == "*" {
			if c.Credentials {
				return origin, true
			}
			return "*", true
		}
		if strings.EqualFold(o, origin) {
			return origin, true
		}
	}
	return "", false
}

func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
}
