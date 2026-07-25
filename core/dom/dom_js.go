//go:build js && wasm

// Package dom is the browser, behind an interface that also compiles for the
// server. That is what lets a page write `func Mount()` in its own .templ file:
// the generated Go from a .templ is compiled for BOTH targets, so it can never
// import syscall/js directly. The platform split lives here instead, once.
package dom

import "syscall/js"

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

// On registers a DOM listener. The js.Func is intentionally never Released:
// listeners live as long as the element, and the element outlives this call.
// A page that mounts repeatedly leaks one closure per listener — fine here,
// worth revisiting if pages start mounting in a loop.
func (e Element) On(event string, fn func()) {
	if !e.v.Truthy() {
		return
	}
	e.v.Call("addEventListener", event, js.FuncOf(func(_ js.Value, args []js.Value) any {
		// A submit or a link click would otherwise navigate away mid-handler.
		if len(args) > 0 && args[0].Truthy() {
			args[0].Call("preventDefault")
		}
		fn()
		return nil
	}))
}

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
