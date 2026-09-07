package desktop

import (
	"net"
	"net/url"
	"strings"
)

// normalize turns the forms people actually type — :9000, localhost:9000,
// http://localhost:9000 — into a URL and the address to dial it on.
//
// Both, from one function, deliberately. Normalizing only the dial address and
// handing the raw string to the webview is a window that waits for the right
// port, opens, and then renders nothing: ":9000" is not a URL, and a webview
// with no address bar reports that as a white page.
func normalize(raw string) (full, addr string, err error) {
	if strings.HasPrefix(raw, ":") {
		raw = "127.0.0.1" + raw
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return u.String(), net.JoinHostPort(u.Hostname(), port), nil
}
