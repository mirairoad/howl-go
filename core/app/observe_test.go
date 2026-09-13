package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/a-h/templ"
	"github.com/mirairoad/howl-go/core/observe"
	"github.com/mirairoad/howl-go/core/router"
)

// A render is a span under the request, and the request learns its route
// from it: the two things the otel module needs from core and cannot get from
// otelhttp, because the mux has matched by the time either is known.
func TestRenderOpensASpanAndNamesTheRoute(t *testing.T) {
	a := New(Config{
		Routes: []router.Route{{
			Pattern: "/orders/{id}",
			Label:   "Order",
			Client:  true,
			Page:    func() templ.Component { return text("<p>order</p>") },
		}},
		Shell:  shell,
		Public: fstest.MapFS{},
	})

	for _, tc := range []struct{ partial, mode string }{{"", "document"}, {"1", "fragment"}} {
		rec := &observe.Recorder{}
		ctx, request := rec.Start(context.Background(), "request") // stands in for otelhttp's span
		r := httptest.NewRequest(http.MethodGet, "/orders/42", nil).WithContext(observe.With(ctx, rec))
		if tc.partial != "" {
			r.Header.Set("X-Partial", tc.partial)
		}
		a.Mux().ServeHTTP(httptest.NewRecorder(), r)
		request.End(nil)

		renders := rec.Named("howl.render /orders/{id}")
		if len(renders) != 1 {
			t.Fatalf("%s: %d render spans", tc.mode, len(renders))
		}
		render := renders[0]
		if render.Parent == nil || render.Parent.Name != "request" {
			t.Errorf("%s: the render is not under the request span", tc.mode)
		}
		if render.Attrs[observe.Kind] != "render" || render.Attrs["howl.mode"] != tc.mode || render.Attrs["howl.client"] != true {
			t.Errorf("%s: attributes %v", tc.mode, render.Attrs)
		}
		if n, _ := render.Attrs["howl.bytes"].(int); n == 0 || !render.Ended || render.Err != nil {
			t.Errorf("%s: bytes=%v ended=%v err=%v", tc.mode, render.Attrs["howl.bytes"], render.Ended, render.Err)
		}
		if got := rec.Spans()[0].Attrs["http.route"]; got != "/orders/{id}" {
			t.Errorf("%s: the request span was told route %v", tc.mode, got)
		}
	}
}
