package mw

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
)

// CSP sets a Content-Security-Policy. Write the policy as a string — a struct
// per directive would be a second, worse syntax for something the spec already
// defines.
//
// The literal {nonce} is replaced per request with a fresh value, which the
// shell reads back with mw.Nonce(ctx) and puts on its own <script>:
//
//	a.Use(mw.CSP{Policy: "default-src 'self'; script-src 'self' 'nonce-{nonce}'"}.Handler)
//
//	<script nonce={ mw.Nonce(ctx) } src="/static/app.js" type="module"></script>
//
// Start with ReportOnly: true. A policy that blocks your own stylesheet looks
// exactly like a broken deploy.
type CSP struct {
	Policy     string
	ReportOnly bool
}

func (c CSP) Handler(next http.Handler) http.Handler {
	header := "Content-Security-Policy"
	if c.ReportOnly {
		header = "Content-Security-Policy-Report-Only"
	}
	needsNonce := strings.Contains(c.Policy, "{nonce}")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		policy := c.Policy
		if needsNonce {
			n := newNonce()
			policy = strings.ReplaceAll(policy, "{nonce}", n)
			r = r.WithContext(context.WithValue(r.Context(), nonceKey, n))
		}
		if policy != "" {
			w.Header().Set(header, policy)
		}
		next.ServeHTTP(w, r)
	})
}

// Nonce returns this request's CSP nonce, or "" when the policy has none.
func Nonce(ctx context.Context) string {
	s, _ := ctx.Value(nonceKey).(string)
	return s
}

func newNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return base64.RawStdEncoding.EncodeToString(b[:])
}
