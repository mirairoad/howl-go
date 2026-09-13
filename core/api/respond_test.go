package api_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mirairoad/howl-go/core/api"
)

// The reason this whole family exists: robots.txt is an endpoint at a fixed
// URL, and a crawler handed application/json ignores it.
func TestTextAnswersItsOwnContentType(t *testing.T) {
	route := api.At("GET", "/robots.txt", api.Define(api.Spec[api.None, api.None, api.Raw]{
		Name: "Robots",
		Path: "/robots.txt",
		Handler: func(*api.Request[api.None, api.None]) (api.Raw, error) {
			return api.Text("User-agent: *\nDisallow: /\n"), nil
		},
	}))
	server := serve(t, api.Config{}, route)

	res, err := http.Get(server.URL + "/robots.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	if got := res.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if string(body) != "User-agent: *\nDisallow: /\n" {
		t.Fatalf("body = %q — the JSON envelope was applied", body)
	}
}

func TestHTMLAndXMLAndBytesEachNameTheirType(t *testing.T) {
	for _, tc := range []struct {
		path, want string
		result     api.Raw
	}{
		{"/page", "text/html; charset=utf-8", api.HTML("<p>hi</p>")},
		{"/sitemap.xml", "application/xml; charset=utf-8", api.XML("<urlset/>")},
		{"/manifest.json", "application/manifest+json", api.Bytes("application/manifest+json", []byte(`{"name":"x"}`))},
		// Nothing said, so nothing guessed: a browser must not render bytes
		// the endpoint declined to describe.
		{"/blob", "application/octet-stream", api.Raw{Body: []byte("...")}},
	} {
		result := tc.result
		route := api.At("GET", tc.path, api.Define(api.Spec[api.None, api.None, api.Raw]{
			Name: tc.path,
			Path: tc.path,
			Handler: func(*api.Request[api.None, api.None]) (api.Raw, error) {
				return result, nil
			},
		}))
		server := serve(t, api.Config{}, route)
		res, err := http.Get(server.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if got := res.Header.Get("Content-Type"); got != tc.want {
			t.Fatalf("%s: Content-Type = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// Cookies and headers set by the handler still go out: a Responder owns
// Content-Type and the status line, and nothing else.
func TestARawResponseKeepsTheHandlersHeadersAndStatus(t *testing.T) {
	route := api.At("POST", "/api/export", api.Define(api.Spec[api.None, api.None, api.Raw]{
		Name:   "Export",
		Method: "POST",
		Handler: func(r *api.Request[api.None, api.None]) (api.Raw, error) {
			r.Header().Set("Content-Disposition", `attachment; filename="a.csv"`)
			r.SetCookie(&http.Cookie{Name: "seen", Value: "1", Path: "/"})
			return api.Bytes("text/csv", []byte("a,b\n1,2\n")).WithStatus(http.StatusCreated), nil
		},
	}))
	server := serve(t, api.Config{}, route)

	res, err := http.Post(server.URL+"/api/export", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 from WithStatus", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "text/csv" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := res.Header.Get("Content-Disposition"); got == "" {
		t.Fatal("the handler's header was dropped")
	}
	if cookies := res.Cookies(); len(cookies) != 1 || cookies[0].Name != "seen" {
		t.Fatalf("cookies = %v", cookies)
	}
}

// An error is still the JSON envelope with its correlation id. A handler that
// answers text/plain on success has not opted out of how failure is reported.
func TestARawEndpointStillFailsAsJSON(t *testing.T) {
	route := api.At("GET", "/api/doc", api.Define(api.Spec[api.None, api.None, api.Raw]{
		Name: "Doc",
		Handler: func(*api.Request[api.None, api.None]) (api.Raw, error) {
			return api.Raw{}, api.NotFound("no such document")
		},
	}))
	server := serve(t, api.Config{}, route)

	res, err := http.Get(server.URL + "/api/doc")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want the error envelope's", got)
	}
	if !strings.Contains(string(body), "no such document") {
		t.Fatalf("body = %q", body)
	}
}

// The open redirect: a protocol-relative target is a URL to another site, and
// a handler that builds one out of a ?next= parameter hands the visitor to it.
func TestRedirectRefusesToLeaveTheSite(t *testing.T) {
	route := api.At("GET", "/api/next", api.Define(api.Spec[api.None, api.None, api.Redirect]{
		Name: "Next",
		Handler: func(r *api.Request[api.None, api.None]) (api.Redirect, error) {
			return api.Redirect{To: r.HTTP.URL.Query().Get("to")}, nil
		},
	}))
	server := serve(t, api.Config{}, route)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	for _, tc := range []struct{ to, want string }{
		{"//evil.com/login", "/evil.com/login"},
		{"////evil.com/login", "/evil.com/login"},
		{"//evil.com?a=b", "/evil.com?a=b"},
		{"/dashboard", "/dashboard"},
	} {
		res, err := client.Get(server.URL + "/api/next?to=" + tc.to)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusFound {
			t.Fatalf("%s: status = %d, want the 302 default", tc.to, res.StatusCode)
		}
		if got := res.Header.Get("Location"); got != tc.want {
			t.Fatalf("%s: Location = %q, want %q", tc.to, got, tc.want)
		}
	}
}

// A stream that arrives all at once is an ordinary slow response. This asserts
// the first chunk is readable before the handler has produced the last.
func TestStreamReachesTheClientBeforeItEnds(t *testing.T) {
	release := make(chan struct{})
	route := api.At("GET", "/api/tail", api.Define(api.Spec[api.None, api.None, api.Stream]{
		Name: "Tail",
		Handler: func(*api.Request[api.None, api.None]) (api.Stream, error) {
			return api.Stream{ContentType: "text/plain", Write: func(w io.Writer) error {
				if _, err := io.WriteString(w, "first\n"); err != nil {
					return err
				}
				<-release
				_, err := io.WriteString(w, "last\n")
				return err
			}}, nil
		},
	}))
	server := serve(t, api.Config{}, route)

	res, err := http.Get(server.URL + "/api/tail")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	reader := bufio.NewReader(res.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("first chunk: %v — nothing was flushed until the handler returned", err)
	}
	if line != "first\n" {
		t.Fatalf("first chunk = %q", line)
	}
	close(release)
	rest, _ := io.ReadAll(reader)
	if string(rest) != "last\n" {
		t.Fatalf("rest = %q", rest)
	}
}

func TestSSEFramesCarryEveryFieldAndSplitMultilineData(t *testing.T) {
	route := api.At("GET", "/api/events", api.Define(api.Spec[api.None, api.None, api.SSE]{
		Name: "Events",
		Handler: func(*api.Request[api.None, api.None]) (api.SSE, error) {
			return api.SSE{Events: func(yield func(api.Event) bool) {
				yield(api.Event{ID: "1", Name: "tick", Retry: 2 * time.Second, Data: "one"})
				yield(api.Event{Data: "line one\nline two"})
			}}, nil
		},
	}))
	server := serve(t, api.Config{}, route)

	res, err := http.Get(server.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if got := res.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q — a cached stream delivers its first frame forever", got)
	}
	if got := res.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q — nginx would hold every frame until the stream ends", got)
	}
	body, _ := io.ReadAll(res.Body)

	want := "id: 1\nevent: tick\nretry: 2000\ndata: one\n\n" +
		"data: line one\ndata: line two\n\n"
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

// A generator that ignores yield's answer would spin forever once the caller
// hangs up. The contract is that yield reports it; this proves it does.
func TestSSEStopsWhenTheCallerGoesAway(t *testing.T) {
	stopped := make(chan struct{})
	sent := 0
	route := api.At("GET", "/api/forever", api.Define(api.Spec[api.None, api.None, api.SSE]{
		Name: "Forever",
		Handler: func(r *api.Request[api.None, api.None]) (api.SSE, error) {
			ctx := r.Context()
			return api.SSE{Events: func(yield func(api.Event) bool) {
				defer close(stopped)
				for ctx.Err() == nil {
					sent++
					if !yield(api.Event{Data: fmt.Sprint(sent)}) {
						return
					}
					time.Sleep(time.Millisecond)
				}
			}}, nil
		},
	}))
	server := serve(t, api.Config{}, route)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/forever", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	if _, err := res.Body.Read(buf); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	cancel()
	res.Body.Close()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the generator is still running after the caller hung up")
	}
}

// Buffering a stream to store it means holding it open until it ends and then
// serving the recording. A startup panic beats a request that never returns.
func TestCachingAStreamPanicsAtDefinition(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	api.Define(api.Spec[api.None, api.None, api.SSE]{
		Name:  "Events",
		Cache: api.Cache{TTL: time.Second},
		Handler: func(*api.Request[api.None, api.None]) (api.SSE, error) {
			return api.SSE{}, nil
		},
	})
}

// The document is built from the same table that was registered, so an
// endpoint answering text/plain must not be described as JSON.
func TestDocumentDoesNotInventASchemaForAResponder(t *testing.T) {
	doc := api.Document(api.Info{}, []api.Route{
		api.At("GET", "/robots.txt", api.Define(api.Spec[api.None, api.None, api.Raw]{
			Name: "Robots",
			Handler: func(*api.Request[api.None, api.None]) (api.Raw, error) {
				return api.Text(""), nil
			},
		})),
		api.At("GET", "/api/next", api.Define(api.Spec[api.None, api.None, api.Redirect]{
			Name: "Next",
			Handler: func(*api.Request[api.None, api.None]) (api.Redirect, error) {
				return api.Redirect{To: "/"}, nil
			},
		})),
	})
	paths := doc["paths"].(map[string]any)

	raw := paths["/robots.txt"].(map[string]any)["get"].(map[string]any)["responses"].(map[string]any)
	ok, found := raw["200"].(map[string]any)
	if !found {
		t.Fatalf("responses = %v", raw)
	}
	if _, invented := ok["content"]; invented {
		t.Fatalf("200 = %v — a schema was invented for bytes reflect cannot see", ok)
	}

	redirect := paths["/api/next"].(map[string]any)["get"].(map[string]any)["responses"].(map[string]any)
	if _, found := redirect["302"]; !found {
		t.Fatalf("responses = %v, want a redirect documented as one", redirect)
	}
}
