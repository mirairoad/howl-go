package mw

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
)

// CSRF rejects state-changing requests that did not come from your own pages.
// Two independent checks, because each one alone has a hole:
//
//  1. The origin must be this site. Cheap, and it covers the plain <form> post
//     from another domain — but a request with no Origin and no Referer (some
//     proxies, some old clients) passes it by default.
//  2. Double submit: the token in the cookie must equal the token in the
//     request. An attacker's page can make the browser send your cookie, but
//     it cannot read it, so it cannot echo the value back.
//
// The token is on the context for the page to render into its forms, and in a
// cookie for fetch() to echo:
//
//	<input type="hidden" name="csrf_token" value={ mw.CSRFToken(ctx) }/>
//
// Multipart posts must send the token in the header — parsing a multipart body
// here would mean buffering an upload before the handler decides it wants it.
type CSRF struct {
	CookieName string // default "csrf"
	HeaderName string // default "X-CSRF-Token"
	FieldName  string // default "csrf_token"
	// Path scopes the cookie; default "/".
	Path string
	// Secure marks the cookie https-only. Leave false for local development
	// over http, set it in production.
	Secure bool
	// HTTPOnly hides the cookie from JS. Only set it if every request carries
	// the token from a server-rendered form — fetch() cannot read the cookie
	// to echo it back.
	HTTPOnly bool
	// TrustedOrigins are extra origins allowed to post here, e.g. a separate
	// admin domain. The site's own origin is always trusted.
	TrustedOrigins []string
	// OnReject replaces the default 403.
	OnReject http.HandlerFunc
}

func (c CSRF) Handler(next http.Handler) http.Handler {
	cookieName := or(c.CookieName, "csrf")
	headerName := or(c.HeaderName, "X-CSRF-Token")
	fieldName := or(c.FieldName, "csrf_token")
	path := or(c.Path, "/")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if ck, err := r.Cookie(cookieName); err == nil && len(ck.Value) >= 16 {
			token = ck.Value
		}
		if token == "" {
			token = newToken()
			http.SetCookie(w, &http.Cookie{
				Name:     cookieName,
				Value:    token,
				Path:     path,
				Secure:   c.Secure,
				HttpOnly: c.HTTPOnly,
				SameSite: http.SameSiteLaxMode,
			})
		}
		r = r.WithContext(context.WithValue(r.Context(), csrfKey, token))

		if safeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		if !c.originOK(r) || !constantEqual(token, submitted(r, headerName, fieldName)) {
			if c.OnReject != nil {
				c.OnReject(w, r)
				return
			}
			http.Error(w, "csrf: request rejected", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// CSRFToken returns the token for this request, or "" when CSRF is not in the
// chain. Render it into every form that posts.
func CSRFToken(ctx context.Context) string {
	s, _ := ctx.Value(csrfKey).(string)
	return s
}

func (c CSRF) originOK(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// No Origin: fall back to Referer, and if there is neither, let the
		// token check stand alone rather than break non-browser clients.
		if ref := r.Header.Get("Referer"); ref != "" {
			u, err := url.Parse(ref)
			if err != nil {
				return false
			}
			origin = u.Scheme + "://" + u.Host
		} else {
			return true
		}
	}
	if strings.EqualFold(origin, "https://"+r.Host) || strings.EqualFold(origin, "http://"+r.Host) {
		return true
	}
	for _, t := range c.TrustedOrigins {
		if strings.EqualFold(t, origin) {
			return true
		}
	}
	return false
}

func submitted(r *http.Request, header, field string) string {
	if v := r.Header.Get(header); v != "" {
		return v
	}
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		// ParseForm caches, so the handler's own r.FormValue still works.
		if err := r.ParseForm(); err == nil {
			return r.PostFormValue(field)
		}
	}
	return ""
}

func safeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

func constantEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func newToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
