//go:build js && wasm

// Compiled with GOOS=js GOARCH=wasm. Imports the SAME generated route table and
// the SAME templ components the server uses, so a navigation renders locally
// with no HTML over the wire.
package main

import (
	"context"
	"encoding/json"
	"strings"
	"syscall/js"

	"howl-go/client/dom"
	"howl-go/client/pages"
	"howl-go/client/router"
	"howl-go/client/store"
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
		dom.SetRoot(args[1]) // the element the page was rendered into
		rt.Mount()
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
	if ok && rt.Unmount != nil {
		rt.Unmount()
	}
	return nil
}

// render(path, dataJSON) -> html for the page + its layouts, or "" if this
// route is not client-renderable (then the client falls back to the server).
func render(_ js.Value, args []js.Value) any {
	if len(args) < 2 {
		return ""
	}
	path := canonical(args[0].String())

	rt, params, ok := router.Lookup(routes, path)
	if !ok || !rt.Client {
		return ""
	}

	var m store.Metrics
	if err := json.Unmarshal([]byte(args[1].String()), &m); err != nil {
		return "<p class=\"hint\">bad data: " + err.Error() + "</p>"
	}

	ctx := router.WithRoutes(context.Background(), routes)
	ctx = router.WithCurrent(ctx, path)
	ctx = router.WithParams(ctx, params)
	ctx = store.WithMetrics(ctx, m)
	ctx = store.WithMeta(ctx, store.Meta{Region: "us-east-1"})

	var sb strings.Builder
	// Same wire shape as the server's fragment: the page's head rides along in
	// an inert <template> so the client merges it the one way.
	if title, head := rt.HeadParts(ctx, rt.Label); head != "" || title != rt.Label {
		sb.WriteString("<template data-head><title>" + title + "</title>" + head + "</template>")
	}
	if err := rt.Component().Render(ctx, &sb); err != nil {
		return "<p class=\"hint\">render error: " + err.Error() + "</p>"
	}
	return sb.String()
}

func canonical(p string) string {
	if len(p) > 1 {
		return strings.TrimSuffix(p, "/")
	}
	return p
}

func main() {
	js.Global().Set("howlRender", js.FuncOf(render))
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
