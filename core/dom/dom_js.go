//go:build js && wasm

// Package dom is the browser, behind an interface that also compiles for the
// server. That is what lets a page write `func Mount()` in its own .templ file:
// the generated Go from a .templ is compiled for BOTH targets, so it can never
// import syscall/js directly. The platform split lives here instead, once.
package dom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"syscall/js"
)

var root js.Value

// SetRoot is called by the wasm runtime before a page's Mount runs. It takes
// `any` so the stub build can share the signature without naming js.Value.
func SetRoot(v any) {
	if jv, ok := v.(js.Value); ok {
		root = jv
	}
}

// Root is the element the current page was rendered into.
func Root() Element { return Element{root} }

type Element struct{ v js.Value }

func (e Element) Valid() bool { return e.v.Truthy() }

func (e Element) Query(sel string) Element {
	return Element{e.v.Call("querySelector", sel)}
}

func (e Element) QueryAll(sel string) []Element {
	list := e.v.Call("querySelectorAll", sel)
	n := list.Get("length").Int()
	out := make([]Element, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Element{list.Index(i)})
	}
	return out
}

func (e Element) Text() string      { return e.v.Get("textContent").String() }
func (e Element) SetText(s string)  { e.v.Set("textContent", s) }
func (e Element) SetHTML(s string)  { e.v.Set("innerHTML", s) }
func (e Element) Value() string     { return e.v.Get("value").String() }
func (e Element) SetValue(s string) { e.v.Set("value", s) }
func (e Element) Hide(hidden bool)  { e.v.Set("hidden", hidden) }

func (e Element) Attr(name string) string {
	v := e.v.Call("getAttribute", name)
	if v.IsNull() || v.IsUndefined() {
		return ""
	}
	return v.String()
}

func (e Element) SetAttr(name, val string) { e.v.Call("setAttribute", name, val) }

// On registers a DOM listener and returns the function that removes it again.
//
// The returned func is the only way to release the js.Func underneath. A
// js.Func holds a Go closure alive on the JS side until Release is called, and
// nothing else can reach it — so an On whose handle is dropped leaks one
// closure per call, permanently. Pages mount on every client-side navigation
// and repaint handlers are re-bound on every repaint, so "one per call" is one
// per visit, for the life of the tab.
//
// Ignoring the result is still legal Go and still correct for a listener that
// should live as long as the page process — an app-shell control outside the
// outlet, say. Inside a page, keep it and call it from Unmount.
func (e Element) On(event string, fn func()) func() {
	if !e.v.Truthy() {
		return func() {}
	}
	cb := js.FuncOf(func(_ js.Value, args []js.Value) any {
		// A submit or a link click would otherwise navigate away mid-handler.
		if len(args) > 0 && args[0].Truthy() {
			args[0].Call("preventDefault")
		}
		fn()
		return nil
	})
	e.v.Call("addEventListener", event, cb)

	var once bool
	return func() {
		// Releasing twice panics, and a release func is exactly the kind of
		// thing a defensive Unmount calls again on a second pass.
		if once {
			return
		}
		once = true
		e.v.Call("removeEventListener", event, cb)
		cb.Release()
	}
}

// Off releases several listeners at once — the shape an Unmount wants, since
// it holds one handle per thing Mount registered.
//
//	var stop []func()
//	func Mount()   { stop = append(stop, el.On("click", add)) }
//	func Unmount() { dom.Off(stop...); stop = nil }
func Off(release ...func()) {
	for _, r := range release {
		if r != nil {
			r()
		}
	}
}

// Navigate moves to another route through the client router — the same code
// path a link click takes, so prefetch, head merging, scroll and lifecycle
// hooks all behave identically. Use it for the navigations a link cannot
// express: after a form submit, after a login, on a timer.
//
// Options mirror the JS side: Replace swaps the current history entry instead
// of pushing a new one; Transition names a view transition.
func Navigate(path string, opts ...NavOption) {
	o := NavOptions{}
	for _, fn := range opts {
		fn(&o)
	}
	howl := js.Global().Get("howl")
	if !howl.Truthy() {
		js.Global().Get("location").Set("href", path) // runtime not up yet
		return
	}
	howl.Call("navigate", path, map[string]any{
		"replace":    o.Replace,
		"transition": o.Transition,
	})
}

// Prefetch warms a route without going to it — the imperative form of hovering
// a link. Harmless to call twice; the client keeps one entry per URL.
func Prefetch(path string) {
	if howl := js.Global().Get("howl"); howl.Truthy() {
		howl.Call("prefetch", path)
	}
}

type NavOptions struct {
	Replace    bool
	Transition string
}

type NavOption func(*NavOptions)

// Replace overwrites the current history entry, so Back skips the page being
// left — right after a login, where returning to the form is never wanted.
func Replace() NavOption { return func(o *NavOptions) { o.Replace = true } }

// Transition names the view transition to play, matching the CSS that a
// data-transition-* attribute would have selected.
func Transition(name string) NavOption { return func(o *NavOptions) { o.Transition = name } }

func Log(args ...any)  { console("log", args...) }
func Warn(args ...any) { console("warn", args...) }

func console(level string, args ...any) {
	vals := make([]any, len(args))
	for i, a := range args {
		if el, ok := a.(Element); ok {
			vals[i] = el.v
		} else {
			vals[i] = a
		}
	}
	js.Global().Get("console").Call(level, vals...)
}

// ---------------------------------------------------------------------------
// Fetch
//
// The browser's own fetch(), because net/http is the wrong price in a wasm
// binary: it links crypto/tls and crypto/x509 to re-implement what the browser
// already did, and measured on an empty wasm build that is 0.51 MB gzipped
// versus 2.56 MB. Same capability, 200x cheaper.
//
// It must be called from a goroutine, not from a JS callback: the promise can
// only settle once control returns to the event loop, and blocking the callback
// deadlocks the Go scheduler.
// ---------------------------------------------------------------------------

// Fetch performs an HTTP request and returns the status and body. A nil body
// sends no payload. Header values are set as given.
func Fetch(ctx context.Context, method, url string, body []byte, header map[string]string) (int, []byte, error) {
	type result struct {
		status int
		body   []byte
		err    error
	}
	done := make(chan result, 1)

	options := map[string]any{"method": method}
	if body != nil {
		// A Uint8Array copy: JS cannot see Go's memory, and passing the slice
		// directly is not something syscall/js can do.
		buffer := js.Global().Get("Uint8Array").New(len(body))
		js.CopyBytesToJS(buffer, body)
		options["body"] = buffer
	}
	if len(header) > 0 {
		headers := map[string]any{}
		for name, value := range header {
			headers[name] = value
		}
		options["headers"] = headers
	}

	var then, catch, onBody js.Func
	release := func() { then.Release(); catch.Release(); onBody.Release() }

	status := 0
	onBody = js.FuncOf(func(_ js.Value, args []js.Value) any {
		buffer := js.Global().Get("Uint8Array").New(args[0])
		out := make([]byte, buffer.Get("length").Int())
		js.CopyBytesToGo(out, buffer)
		done <- result{status: status, body: out}
		return nil
	})
	then = js.FuncOf(func(_ js.Value, args []js.Value) any {
		status = args[0].Get("status").Int()
		args[0].Call("arrayBuffer").Call("then", onBody).Call("catch", catch)
		return nil
	})
	catch = js.FuncOf(func(_ js.Value, args []js.Value) any {
		message := "fetch failed"
		if len(args) > 0 && args[0].Truthy() {
			message = args[0].Get("message").String()
		}
		done <- result{err: errors.New(message)}
		return nil
	})

	js.Global().Call("fetch", url, options).Call("then", then).Call("catch", catch)

	select {
	case r := <-done:
		// Released only once the promise has settled: releasing a js.Func the
		// browser still holds turns the callback into a panic.
		release()
		return r.status, r.body, r.err
	case <-ctx.Done():
		go func() { <-done; release() }()
		return 0, nil, ctx.Err()
	}
}

// GetJSON fetches a URL and decodes the response into out. The shape page code
// actually wants — ten lines of fetch, status check and decode, written once.
func GetJSON(ctx context.Context, url string, out any) error {
	return JSON(ctx, "GET", url, nil, out)
}

// PostJSON sends body as JSON and, when out is non-nil, decodes the response
// into it.
func PostJSON(ctx context.Context, url string, body, out any) error {
	return JSON(ctx, "POST", url, body, out)
}

// JSON is the general form. A 4xx or 5xx is an error rather than a decoded
// zero value: a page that renders an error body as data is the failure mode
// this exists to prevent.
func JSON(ctx context.Context, method, url string, body, out any) error {
	var payload []byte
	header := map[string]string{"Accept": "application/json"}
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = encoded
		header["Content-Type"] = "application/json"
	}
	status, response, err := Fetch(ctx, method, url, payload, header)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("%s %s: %d %s", method, url, status, strings.TrimSpace(string(response)))
	}
	if out == nil || len(response) == 0 {
		return nil
	}
	return json.Unmarshal(response, out)
}
