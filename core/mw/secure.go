package mw

import (
	"net/http"
	"strconv"
	"time"
)

// SecureHeaders stamps the response headers every site should send and almost
// none remember to — howl (TS)'s defaultHeaders. The zero value is the useful
// one:
//
//	X-Content-Type-Options: nosniff
//	Referrer-Policy: strict-origin-when-cross-origin
//	X-Frame-Options: DENY
//
// Headers are set before the handler runs, so a handler that needs something
// different — a page meant to be framed — simply sets its own.
//
// Two things howl (TS) sent that this deliberately does not:
//
//   - X-XSS-Protection: 1; mode=block. Every browser that implemented the
//     filter has removed it, and in the ones that had it, mode=block could be
//     used to delete chosen scripts from a page — a hole of its own. The
//     modern advice is to not send it; CSP is the replacement (mw.CSP).
//   - Cache-Control: no-store on everything. That would switch off the static
//     handler's year-long immutable assets and the page cache's revalidation;
//     each response here already says how it may be cached.
type SecureHeaders struct {
	// FrameOptions is X-Frame-Options. Empty is DENY; "-" sends none, for a
	// site that sets `frame-ancestors` in its CSP instead.
	FrameOptions string
	// ReferrerPolicy is Referrer-Policy. Empty is
	// strict-origin-when-cross-origin: other sites see the origin, never the
	// path, which can hold an id or a reset token. "-" sends none.
	ReferrerPolicy string
	// HSTS is Strict-Transport-Security's max-age. Zero sends none. Set it only
	// once the site is served over https and will stay so: a browser that has
	// seen it refuses plain http to this host for that long, and there is no
	// way to take it back from here.
	HSTS time.Duration
	// HSTSSubdomains adds includeSubDomains, which applies the same promise to
	// every subdomain — including the ones that do not exist yet.
	HSTSSubdomains bool
	// PermissionsPolicy is sent when set, e.g. "camera=(), microphone=()".
	PermissionsPolicy string
	// OpenerPolicy is Cross-Origin-Opener-Policy, sent when set. same-origin
	// isolates the window from pages it opens, which also breaks sign-in
	// popups that report back through window.opener — so it is not a default.
	OpenerPolicy string
}

func (s SecureHeaders) Handler(next http.Handler) http.Handler {
	type header struct{ name, value string }
	var set []header
	add := func(name, value, fallback string) {
		switch value {
		case "-":
			return
		case "":
			value = fallback
		}
		if value != "" {
			set = append(set, header{name, value})
		}
	}
	add("X-Content-Type-Options", "", "nosniff")
	add("Referrer-Policy", s.ReferrerPolicy, "strict-origin-when-cross-origin")
	add("X-Frame-Options", s.FrameOptions, "DENY")
	if s.HSTS > 0 {
		hsts := "max-age=" + strconv.Itoa(int(s.HSTS.Seconds()))
		if s.HSTSSubdomains {
			hsts += "; includeSubDomains"
		}
		set = append(set, header{"Strict-Transport-Security", hsts})
	}
	add("Permissions-Policy", s.PermissionsPolicy, "")
	add("Cross-Origin-Opener-Policy", s.OpenerPolicy, "")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		for _, kv := range set {
			h.Set(kv.name, kv.value)
		}
		next.ServeHTTP(w, r)
	})
}
