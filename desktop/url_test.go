package desktop

import "testing"

// The window navigates to `full` and waits on `addr`. Returning a normalized
// address but the raw string to navigate to was a window that opened on a
// healthy server and rendered white, because ":9000" is not a URL and a webview
// has no address bar to show you that.
func TestNormalize(t *testing.T) {
	for _, c := range []struct{ in, full, addr string }{
		{":9000", "http://127.0.0.1:9000", "127.0.0.1:9000"},
		{"localhost:9000", "http://localhost:9000", "localhost:9000"},
		{"127.0.0.1:9000", "http://127.0.0.1:9000", "127.0.0.1:9000"},
		{"http://localhost:9000", "http://localhost:9000", "localhost:9000"},
		{"https://example.com", "https://example.com", "example.com:443"},
	} {
		full, addr, err := normalize(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if full != c.full || addr != c.addr {
			t.Errorf("%q -> (%q, %q), want (%q, %q)", c.in, full, addr, c.full, c.addr)
		}
	}
}
