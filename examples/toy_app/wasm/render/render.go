// Package render is the browser renderer with the build tag taken off.
//
// wasm/main.go is `//go:build js && wasm`, so nothing in it can run under
// `go test` on the host — and it holds the one decision that matters for a
// .client route: how the render context is rebuilt from the payload app.js
// hands over. The failure that leaves is silent. A route whose data was never
// installed here renders correctly on the cold request, because the server's
// Config.Data filled its context, and renders empty on the second visit,
// because the store's accessors return the zero value on a miss. No build
// breaks, nothing logs. Everything that does not need syscall/js lives in this
// package so render_test.go can hold it against the server's render of the
// same route.
package render

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"strings"

	"github.com/mirairoad/howl-go/core/router"
	"github.com/mirairoad/howl-go/core/state"
	"github.com/mirairoad/howl-go/examples/toy_app/client/store"
)

// Page renders the route at path — the page plus its layouts, behind the same
// <template data-head> prefix the server sends — from the payload the browser
// fetched for it. ok is false for a route the wasm build does not render; the
// client then asks the server, which answers or 404s.
func Page(routes []router.Route, path string, payload router.RenderPayload) (html string, ok bool, err error) {
	rt, params, found := router.Lookup(routes, path)
	if !found || !rt.Client {
		return "", false, nil
	}
	ctx, err := Context(routes, path, params, payload)
	if err != nil {
		return "", true, err
	}
	return Fragment(rt, ctx)
}

// Context is the browser's counterpart of Config.Data: everything a component
// may read from ctx, rebuilt from the three things a local render has — the
// route's parameters, the bootstrap state the document was served with, and
// the JSON its data endpoint returned. Bootstrap is decoded as the type the
// server's Bootstrap hook returned; route data as the type this application's
// client routes render from. A page that reads anything else from ctx is a
// page that will render its zero value here.
func Context(routes []router.Route, path string, params map[string]string, payload router.RenderPayload) (context.Context, error) {
	var m store.Metrics
	if err := json.Unmarshal(payload.RouteData, &m); err != nil {
		return nil, fmt.Errorf("route data: %w", err)
	}
	ctx := router.WithRoutes(context.Background(), routes)
	ctx = router.WithCurrent(ctx, path)
	ctx = router.WithParams(ctx, params)
	ctx, err := state.Hydrate[store.Meta](ctx, payload.Bootstrap)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	return store.WithMetrics(ctx, m), nil
}

// Fragment writes what the server writes for an X-Partial request: the head in
// an inert template, then the markup. One wire shape, so app.js merges a local
// render and a fetched fragment through the same code.
func Fragment(rt router.Route, ctx context.Context) (string, bool, error) {
	var sb strings.Builder
	title, head := rt.HeadParts(ctx, rt.Label)
	sb.WriteString("<template data-head><title>" + html.EscapeString(title) + "</title>" + head + "</template>")
	if err := rt.Component().Render(ctx, &sb); err != nil {
		return "", true, fmt.Errorf("render: %w", err)
	}
	return sb.String(), true, nil
}
