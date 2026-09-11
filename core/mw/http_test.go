package mw

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSecureHeadersDefaultsAndOptOut(t *testing.T) {
	w := httptest.NewRecorder()
	SecureHeaders{}.Handler(ok("x")).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
		"X-Frame-Options":        "DENY",
	} {
		if got := w.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if w.Header().Get("Strict-Transport-Security") != "" || w.Header().Get("X-XSS-Protection") != "" {
		t.Errorf("sent a header it should not by default: %v", w.Header())
	}

	w = httptest.NewRecorder()
	SecureHeaders{FrameOptions: "-", HSTS: 365 * 24 * time.Hour, HSTSSubdomains: true}.Handler(ok("x")).
		ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Header().Get("X-Frame-Options") != "" {
		t.Error(`FrameOptions "-" still sent X-Frame-Options`)
	}
	if got := w.Header().Get("Strict-Transport-Security"); got != "max-age=31536000; includeSubDomains" {
		t.Errorf("HSTS = %q", got)
	}
}

func TestOnlyAndExceptMatchWholeSegments(t *testing.T) {
	mark := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Marked", "1")
			next.ServeHTTP(w, r)
		})
	}
	only := Only("/admin", mark)(ok("x"))
	except := Except("/api", mark)(ok("x"))

	for path, want := range map[string]bool{"/admin": true, "/admin/users": true, "/administrator": false, "/": false} {
		w := httptest.NewRecorder()
		only.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if got := w.Header().Get("X-Marked") == "1"; got != want {
			t.Errorf("Only(/admin) on %s = %v, want %v", path, got, want)
		}
	}
	for path, want := range map[string]bool{"/api/v1/x": false, "/api": false, "/apiary": true, "/about": true} {
		w := httptest.NewRecorder()
		except.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if got := w.Header().Get("X-Marked") == "1"; got != want {
			t.Errorf("Except(/api) on %s = %v, want %v", path, got, want)
		}
	}
}

func TestProxyForwardsMatchingPathsOnly(t *testing.T) {
	var seen *http.Request
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		http.SetCookie(w, &http.Cookie{Name: "u", Value: "1", Domain: "internal.example", Path: "/"})
		io.WriteString(w, "from upstream") //nolint:errcheck
	}))
	defer upstream.Close()

	h := Proxy{
		Target:      upstream.URL,
		Paths:       []string{"/legacy/{table}/restore"},
		StripPrefix: true,
	}.Handler(ok("local"))

	r := httptest.NewRequest("POST", "/legacy/users/restore/42?force=1", strings.NewReader("{}"))
	r.Header.Set("X-Forwarded-For", "6.6.6.6") // a client claiming to be someone else
	r.RemoteAddr = "10.0.0.5:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Body.String() != "from upstream" {
		t.Fatalf("body = %q, want the upstream's", w.Body.String())
	}
	if seen.URL.Path != "/42" || seen.URL.RawQuery != "force=1" || seen.Method != "POST" {
		t.Fatalf("upstream saw %s %s?%s, want POST /42?force=1", seen.Method, seen.URL.Path, seen.URL.RawQuery)
	}
	if got := seen.Header.Get("X-Forwarded-For"); got != "10.0.0.5" {
		t.Fatalf("X-Forwarded-For = %q — the client's own claim was forwarded", got)
	}
	if c := w.Header().Get("Set-Cookie"); strings.Contains(strings.ToLower(c), "domain=") {
		t.Fatalf("Set-Cookie kept the upstream's Domain: %q", c)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/legacy/users", nil)) // shorter than the prefix
	if w.Body.String() != "local" {
		t.Fatalf("/legacy/users = %q, want the local handler", w.Body.String())
	}
}

func TestProxyAnswers502WhenTheTargetIsDown(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	target := upstream.URL
	upstream.Close() // nothing listens there now

	h := Proxy{Target: target, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}.Handler(ok("local"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if strings.Contains(w.Body.String(), "127.0.0.1") {
		t.Fatalf("the error named the internal address: %q", w.Body.String())
	}
}

func TestProxyPanicsOnARelativeTarget(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a relative Target was accepted")
		}
	}()
	Proxy{Target: "localhost:8000"}.Handler(ok("x"))
}

func TestClientIPIgnoresAForwardedValueThatIsNotAnAddress(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:4000"
	r.Header.Set("X-Forwarded-For", "admin\n, 10.0.0.1")
	if got := ClientIP(r, true); got != "10.0.0.5" {
		t.Fatalf("ClientIP = %q, want the peer address", got)
	}
}
