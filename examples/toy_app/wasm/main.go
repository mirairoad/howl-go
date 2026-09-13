//go:build js && wasm

// Compiled with GOOS=js GOARCH=wasm. Imports the SAME generated route table and
// the SAME templ components the server uses, so a navigation renders locally
// with no HTML over the wire.
//
// Only the syscall/js wiring is here. What a route renders to, and from which
// parts of the payload, is wasm/render — a package without a build tag, so its
// test can run on the host and hold each .client route against the server.
package main

import (
	"strings"
	"syscall/js"

	"github.com/mirairoad/howl-go/core/dom"
	"github.com/mirairoad/howl-go/core/router"
	"github.com/mirairoad/howl-go/examples/toy_app/client/pages"
	"github.com/mirairoad/howl-go/examples/toy_app/wasm/render"
)

var routes = pages.FsClientRoutes()

// mount(path, element) runs the page's Go lifecycle hook after its markup is in
// the DOM. templ itself has no lifecycle — a component renders once to a writer
// and is finished — so "on first paint" lives here instead.
func mount(_ js.Value, args []js.Value) any {
	if len(args) < 2 {
		return nil
	}
	rt, _, ok := router.Lookup(routes, canonical(args[0].String()))
	if !ok {
		return nil
	}
	if rt.Mount != nil {
		dom.Mount(args[1], rt.Mount) // opens the scope its effects and listeners are released with
	}
	return nil
}

// unmount(path) runs the outgoing page's teardown before its markup is
// replaced. Anything Mount registered is released here.
func unmount(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return nil
	}
	rt, _, ok := router.Lookup(routes, canonical(args[0].String()))
	if ok {
		dom.Unmount(rt.Unmount) // nil is fine: the scope Mount opened is disposed either way
	}
	return nil
}

// render(path, renderPayloadJSON) -> html for the page + its layouts, or "" if this
// route is not client-renderable (then the client falls back to the server).
func renderRoute(_ js.Value, args []js.Value) any {
	if len(args) < 2 {
		return ""
	}
	payload, err := router.DecodeRenderPayload(args[1].String())
	if err != nil {
		return "<p class=\"hint\">bad data: " + err.Error() + "</p>"
	}
	html, ok, err := render.Page(routes, canonical(args[0].String()), payload)
	if !ok {
		return ""
	}
	if err != nil {
		return "<p class=\"hint\">" + err.Error() + "</p>"
	}
	return html
}

func canonical(p string) string {
	if len(p) > 1 {
		return strings.TrimSuffix(p, "/")
	}
	return p
}

func main() {
	js.Global().Set("howlRender", js.FuncOf(renderRoute))
	js.Global().Set("howlMount", js.FuncOf(mount))
	js.Global().Set("howlUnmount", js.FuncOf(unmount))

	// Hand the generated table to JS so the client never hardcodes a path.
	out := make([]any, 0, len(routes))
	for _, r := range routes {
		out = append(out, map[string]any{"path": r.Pattern, "label": r.Label, "client": r.Client})
	}
	js.Global().Set("howlRoutes", js.ValueOf(out))
	select {}
}
