package mw

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Proxy forwards requests under its paths to another server, and passes every
// other request on — howl (TS)'s createProxy, over the standard library's
// httputil.ReverseProxy, so streaming, WebSocket upgrades and hop-by-hop
// headers are already right.
//
//	a.Use(mw.Proxy{
//	    Target: "http://127.0.0.1:8000",
//	    Paths:  []string{"/api/v1/admin/{table}/restore"},
//	}.Handler)
//
// A bad Target panics when the chain is built, not on the first request that
// needed it.
//
// Forwarding headers are written fresh: X-Forwarded-For, -Host and -Proto
// arriving from the client are dropped and replaced with what this server
// saw, so a caller cannot tell the target it is somebody else. Everything
// else, cookies and Authorization included, goes through — a target you
// proxy to is a part of this application, and it needs to know who is asking.
type Proxy struct {
	// Target is the server to forward to, "http://host:port" with an optional
	// base path that the request path is appended to.
	Target string
	// Paths are prefixes matched a whole segment at a time: /admin matches
	// /admin and /admin/users, never /administrator. A {name} segment matches
	// any one segment. Empty forwards every request.
	//
	// howl (TS) matched with startsWith, so a pattern like
	// /api/v1/admin/:table/restore could never match anything — ":table" is
	// not a segment any request contains. Segments are compared here instead.
	Paths []string
	// StripPrefix removes the matched prefix before forwarding: /legacy/users
	// reaches the target as /users.
	StripPrefix bool
	// KeepHost forwards the incoming Host header. By default the target's own
	// host is sent, which is what a virtual-hosted target expects (howl's
	// changeOrigin: true).
	KeepHost bool
	// KeepCookieDomain passes a target's Set-Cookie Domain attribute through.
	// By default it is removed, making the cookie belong to this host: a
	// cookie scoped to the target's domain is one the browser refuses to store
	// at all, because the browser never talked to that domain.
	KeepCookieDomain bool
	// Timeout bounds the wait for the target's response headers — not the
	// body, so a stream or a long download is not cut off halfway. Zero is 30
	// seconds. A target that misses it answers 504; one that cannot be reached
	// answers 502.
	Timeout time.Duration
	// Log records failures to reach the target. Defaults to slog.Default().
	Log *slog.Logger
}

func (p Proxy) Handler(next http.Handler) http.Handler {
	target, err := url.Parse(p.Target)
	if err != nil || target.Scheme == "" || target.Host == "" {
		panic("mw: Proxy.Target " + strconv.Quote(p.Target) + " is not an absolute URL like http://127.0.0.1:8000")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = timeout

	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
			if p.KeepHost {
				pr.Out.Host = pr.In.Host
			}
		},
		ModifyResponse: func(res *http.Response) error {
			if !p.KeepCookieDomain {
				stripCookieDomain(res.Header)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return // the caller hung up; there is nobody to answer
			}
			code := http.StatusBadGateway
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				code = http.StatusGatewayTimeout
			}
			log := p.Log
			if log == nil {
				log = slog.Default()
			}
			log.Error("proxy", slog.String("path", r.URL.Path), slog.String("target", target.Host),
				slog.Int("status", code), slog.Any("err", err), slog.String("id", ID(r.Context())))
			// The status and nothing else: the error names an address inside
			// the network, which is not the caller's business.
			http.Error(w, http.StatusText(code), code)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, ok := p.match(r.URL.Path)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		if p.StripPrefix && n > 0 {
			rest := r.URL.Path[n:]
			if rest == "" {
				rest = "/"
			}
			r = r.Clone(r.Context())
			r.URL.Path, r.URL.RawPath = rest, ""
		}
		proxy.ServeHTTP(w, r)
	})
}

// match reports whether path is under one of the paths, and how many bytes of
// it the matching prefix covers.
func (p Proxy) match(path string) (int, bool) {
	if len(p.Paths) == 0 {
		return 0, true
	}
	for _, prefix := range p.Paths {
		if n, ok := underPrefix(prefix, path); ok {
			return n, true
		}
	}
	return 0, false
}

// underPrefix matches prefix against the start of path one segment at a time.
// A {name} segment matches any non-empty segment.
func underPrefix(prefix, path string) (int, bool) {
	trimmed := strings.Trim(prefix, "/")
	if trimmed == "" {
		return 0, true
	}
	i := 0
	for _, want := range strings.Split(trimmed, "/") {
		if i >= len(path) || path[i] != '/' {
			return 0, false
		}
		i++
		end := strings.IndexByte(path[i:], '/')
		if end < 0 {
			end = len(path) - i
		}
		got := path[i : i+end]
		wildcard := strings.HasPrefix(want, "{") && strings.HasSuffix(want, "}")
		if got != want && !(wildcard && got != "") {
			return 0, false
		}
		i += end
	}
	return i, true
}

// stripCookieDomain removes the Domain attribute from every Set-Cookie.
func stripCookieDomain(h http.Header) {
	cookies := h["Set-Cookie"]
	for i, c := range cookies {
		parts := strings.Split(c, ";")
		kept := []string{parts[0]}
		for _, attr := range parts[1:] {
			name, _, _ := strings.Cut(strings.TrimSpace(attr), "=")
			if !strings.EqualFold(name, "domain") {
				kept = append(kept, attr)
			}
		}
		cookies[i] = strings.Join(kept, ";")
	}
}
