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
	"io"
	"strings"
	"syscall/js"

	"github.com/mirairoad/howl-go/core/signal"
)

var (
	root    js.Value
	dispose func() // the current page's scope, nil between pages
)

// SetRoot is called by the wasm runtime before a page's Mount runs. It takes
// `any` so the stub build can share the signature without naming js.Value.
func SetRoot(v any) {
	if jv, ok := v.(js.Value); ok {
		root = jv
	}
}

// Mount runs a page's Mount hook inside a scope: every effect, watcher and
// listener it registers is released by the next Unmount, with no bookkeeping
// in the page. The wasm entrypoint calls this from howlMount.
//
// The scope is lexical. A goroutine started in Mount — the hydrate fetch —
// runs after Mount has returned, so anything it registers is outside it;
// a goroutine should write signals and let the effects registered in Mount
// react, which is the shape hydration already has.
func Mount(el any, fn func()) {
	SetRoot(el)
	Unmount(nil) // a page swapped in without its predecessor's Unmount running
	if fn == nil {
		return
	}
	dispose = signal.Scope(fn)
}

// Unmount runs the outgoing page's Unmount hook, if any, then disposes the
// scope its Mount opened. The wasm entrypoint calls this from howlUnmount.
func Unmount(fn func()) {
	if fn != nil {
		fn()
	}
	if dispose != nil {
		dispose()
		dispose = nil
	}
}

// Root is the element the current page was rendered into.
func Root() Element { return Element{root} }

// Embedded decodes a JSON script the server rendered into this page. It is
// the hydrate step without a request:
//
//	@templ.JSONScript("todos", store.SnapshotFrom(ctx))   // in the page markup
//	dom.Embedded("todos", &sn); store.Client().Restore(sn) // in Mount
//
// The fragment carries the script, so it works on a cold load and after a
// client-side navigation alike, and the browser store starts with exactly what
// the user is looking at — no empty-then-full flash, no round trip, and the
// server owns the serialisation. Looked up inside the page first, then the
// document, so a shell-level script is reachable too.
func Embedded(id string, out any) error {
	sel := `script[type="application/json"][id="` + id + `"]`
	el := js.Value{}
	if root.Truthy() {
		el = root.Call("querySelector", sel)
	}
	if !el.Truthy() {
		el = js.Global().Get("document").Call("querySelector", sel)
	}
	if !el.Truthy() {
		return fmt.Errorf("dom.Embedded: no <script type=\"application/json\" id=%q> in the page", id)
	}
	return json.Unmarshal([]byte(el.Get("textContent").String()), out)
}

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

// Focus moves keyboard focus to the element — the first field of a modal that
// just opened, the box a shortcut targets. A no-op on a missing element.
func (e Element) Focus() {
	if e.v.Truthy() {
		e.v.Call("focus")
	}
}

func (e Element) Attr(name string) string {
	v := e.v.Call("getAttribute", name)
	if v.IsNull() || v.IsUndefined() {
		return ""
	}
	return v.String()
}

func (e Element) SetAttr(name, val string) { e.v.Call("setAttribute", name, val) }

// Component is anything that renders itself to a writer — a templ component,
// without this package having to import templ.
type Component interface {
	Render(ctx context.Context, w io.Writer) error
}

// Render draws c into the element, replacing its content. This is the repaint:
// the same component the server rendered, rendered again in the browser and
// swapped in. An element that is no longer in the document — the page was
// swapped away between the write and the effect — is a no-op, not an error,
// because that is the ordinary end of an effect's life.
//
// The swap is innerHTML today. Listeners bound with Delegate survive it, which
// is why that is the listener to use on anything inside a repainted region.
func (e Element) Render(c Component) error {
	if !e.v.Truthy() {
		return nil
	}
	var sb strings.Builder
	if err := c.Render(context.Background(), &sb); err != nil {
		return err
	}
	e.v.Set("innerHTML", sb.String())
	return nil
}

// Event is what a listener receives: the element it fired on and the few
// fields a page reads. Target is the delegated match when there is one, so a
// handler on "[data-del]" gets the button, not the icon inside it.
type Event struct {
	v      js.Value
	target js.Value
}

func (ev Event) Target() Element { return Element{ev.target} }
func (ev Event) Value() string   { return Element{ev.target}.Value() }
func (ev Event) Key() string     { return ev.v.Get("key").String() }
func (ev Event) PreventDefault() { ev.v.Call("preventDefault") }

// On registers a listener on this element and returns the func that removes
// it. Inside a page's Mount the release is automatic — the scope calls it when
// the page leaves — so the result can be ignored. Outside a scope it is the
// only handle: a js.Func is held alive on the JS side until it is released,
// and nothing else can reach it.
//
// A submit, and a click that lands on a link, are prevented before fn runs —
// the browser would otherwise navigate away mid-handler. Nothing else is: a
// click on a checkbox must still toggle it and a keydown must still type.
func (e Element) On(event string, fn func(Event)) func() {
	return e.listen(event, "", fn)
}

// Delegate registers one listener on this element for every descendant that
// matches selector, now or later. Rows rendered by a repaint are covered
// without rebinding, which is what makes a repaint one line:
//
//	root.Delegate("click", "[data-del]", func(e dom.Event) { del(e.Target().Attr("data-del")) })
//
// Only bubbling events reach it. Use focusin/focusout rather than focus/blur.
func (e Element) Delegate(event, selector string, fn func(Event)) func() {
	return e.listen(event, selector, fn)
}

func (e Element) listen(event, selector string, fn func(Event)) func() {
	if !e.v.Truthy() {
		return func() {}
	}
	cb := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) == 0 || !args[0].Truthy() {
			return nil
		}
		ev := args[0]
		target := ev.Get("target")
		if selector != "" {
			// A text node has no closest(); its parent is the element that matters.
			if target.Type() == js.TypeObject && target.Get("closest").IsUndefined() {
				target = target.Get("parentElement")
			}
			if !target.Truthy() {
				return nil
			}
			target = target.Call("closest", selector)
			if !target.Truthy() || !e.v.Call("contains", target).Bool() {
				return nil
			}
		}
		if event == "submit" || (event == "click" && target.Truthy() && isElement(target) && target.Call("closest", "a[href]").Truthy()) {
			ev.Call("preventDefault")
		}
		fn(Event{v: ev, target: target})
		return nil
	})
	e.v.Call("addEventListener", event, cb)

	var once bool
	release := func() {
		// Releasing twice panics, and a release func is exactly the kind of
		// thing a defensive Unmount calls again on a second pass.
		if once {
			return
		}
		once = true
		e.v.Call("removeEventListener", event, cb)
		cb.Release()
	}
	signal.OnCleanup(release)
	return release
}

func isElement(v js.Value) bool {
	return v.Type() == js.TypeObject && !v.Get("closest").IsUndefined()
}

// Frame runs fn once per animation frame until it returns false. t is the
// frame timestamp in milliseconds, as requestAnimationFrame reports it. This
// is the loop behind a progress bar, a countdown or a canvas: one callback per
// paint, never a timer fighting the display's refresh. Inside a page's Mount
// the loop is released with the scope; outside one, call the returned stop.
//
// Do not write signals from it every frame unless the effects they wake are
// cheap — an effect is a repaint, and sixty repaints a second of a list is the
// thing this framework has no virtual DOM to absorb. Set text, width and
// transforms directly from the frame callback and leave signals for state.
func Frame(fn func(t float64) bool) (stop func()) {
	var cb js.Func
	var pending js.Value // the frame the browser has queued, cancelled on stop
	stopped := false
	cb = js.FuncOf(func(_ js.Value, args []js.Value) any {
		if stopped {
			return nil
		}
		t := 0.0
		if len(args) > 0 {
			t = args[0].Float()
		}
		if fn(t) {
			pending = js.Global().Call("requestAnimationFrame", cb)
			return nil
		}
		stopped = true
		cb.Release()
		return nil
	})
	pending = js.Global().Call("requestAnimationFrame", cb)
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		// The browser already holds the next frame. Releasing the func with
		// that frame still queued is a "call to released function" a few
		// milliseconds later — measured, once per page leave — so cancel it
		// first.
		js.Global().Call("cancelAnimationFrame", pending)
		cb.Release()
	}
	signal.OnCleanup(stop)
	return stop
}

// Off releases several listeners at once, for code that keeps handles outside
// a scope: an app shell's own controls, registered once at startup.
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
