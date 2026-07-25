//go:build js && wasm

// The browser half of the documentation site. It imports the same generated
// route table and the same templ components the server uses, so a navigation
// to a client route renders locally with no HTML over the wire.
package main

import (
	"context"
	"strings"
	"syscall/js"

	"github.com/mirairoad/howl-go/core/app"
	"github.com/mirairoad/howl-go/core/dom"
	"github.com/mirairoad/howl-go/core/router"
	"github.com/mirairoad/howl-go/www/client/pages"
)

var routes = pages.FsClientRoutes()

func render(_ js.Value, args []js.Value) any {
	if len(args) < 2 {
		return ""
	}
	path := app.Canonical(args[0].String())
	rt, params, ok := router.Lookup(routes, path)
	if !ok || !rt.Client {
		return "" // not client-renderable: fall back to the server
	}

	ctx := router.WithRoutes(context.Background(), routes)
	ctx = router.WithCurrent(ctx, path)
	ctx = router.WithParams(ctx, params)

	var sb strings.Builder
	if title, head := rt.HeadParts(ctx, rt.Label); head != "" || title != rt.Label {
		sb.WriteString("<template data-head><title>" + title + "</title>" + head + "</template>")
	}
	if err := rt.Component().Render(ctx, &sb); err != nil {
		return "<p>render error: " + err.Error() + "</p>"
	}
	return sb.String()
}

func mount(_ js.Value, args []js.Value) any {
	if len(args) < 2 {
		return nil
	}
	if rt, _, ok := router.Lookup(routes, app.Canonical(args[0].String())); ok && rt.Mount != nil {
		dom.SetRoot(args[1])
		rt.Mount()
	}
	return nil
}

func unmount(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return nil
	}
	if rt, _, ok := router.Lookup(routes, app.Canonical(args[0].String())); ok && rt.Unmount != nil {
		rt.Unmount()
	}
	return nil
}

func main() {
	js.Global().Set("howlRender", js.FuncOf(render))
	js.Global().Set("howlMount", js.FuncOf(mount))
	js.Global().Set("howlUnmount", js.FuncOf(unmount))

	out := make([]any, 0, len(routes))
	for _, r := range routes {
		out = append(out, map[string]any{"path": r.Pattern, "label": r.Label, "client": r.Client})
	}
	js.Global().Set("howlRoutes", js.ValueOf(out))
	select {}
}
