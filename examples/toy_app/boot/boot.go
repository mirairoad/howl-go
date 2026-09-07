// Package boot assembles the application. It exists because two binaries need
// the identical app — the server in main.go and the desktop shell in desktop/ —
// and the only difference between them is how the listener is opened.
package boot

import (
	"context"
	"log"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/mirairoad/howl-go/core/api"
	"github.com/mirairoad/howl-go/core/app"
	"github.com/mirairoad/howl-go/core/mw"
	"github.com/mirairoad/howl-go/examples/toy_app/client"
	"github.com/mirairoad/howl-go/examples/toy_app/client/pages"
	"github.com/mirairoad/howl-go/examples/toy_app/client/store"
	"github.com/mirairoad/howl-go/examples/toy_app/client/ui"
	"github.com/mirairoad/howl-go/examples/toy_app/server/apis"
	apistore "github.com/mirairoad/howl-go/examples/toy_app/server/apis/store"
)

var db = store.New()

// data is the application's contribution to every render's context. Pages take
// no arguments — the generated table needs one uniform signature — so this is
// how they receive everything.
func data(ctx context.Context, path string) context.Context {
	ctx = store.WithMetrics(ctx, apistore.Metrics())
	ctx = store.WithTodos(ctx, db.List())
	return store.WithMeta(ctx, store.Meta{
		RenderedAt: time.Now().Format("15:04:05.000"),
		GoVersion:  runtime.Version(),
		Region:     "us-east-1",
	})
}

// New returns the application and the handler to serve it with. Pass the
// handler to a.Listen for a server, or to a.Serve for a listener you opened
// yourself.
func New() (*app.App, http.Handler) {
	db.Add("render the same component three ways")
	db.Add("keep island state across navigation")

	a := app.New(app.Config{
		Routes:   pages.FsClientRoutes(),
		Shell:    pages.App,
		NotFound: pages.NotFound,
		Public:   client.Public(),
		Data:     data,
		// The browser fetches this once before its first local render and
		// hands it to the wasm renderer. Omit it and no fetch happens.
		ClientData: "/api/metrics",
		// Runtime values are serialized from this server process and restored
		// into the wasm render context. The component never calls time.Now or
		// runtime.Version itself, so SSR and local navigation see the same value.
		Bootstrap: func(ctx context.Context, _ string) any {
			return store.MetaFrom(ctx)
		},
		// Outermost first. Ordinary net/http decorators — nothing here knows
		// about templ, routes or this application.
		Use: []mw.Middleware{
			mw.RequestID,
			mw.LogWith(mw.LogOptions{Callers: true, Skip: mw.SkipNoise}),
			mw.Recover(nil),
			mw.Compress{}.Handler,
		},
	})

	mux := a.Mux()

	// The JSON API lives in server/apis, one file per endpoint, generated into
	// apis_gen.go and into the typed client the pages call. Registering it is
	// two lines and there is no URL string in this file any more.
	apistore.Use(db)
	api.Register(mux, api.Config{}, apis.FsApiRoutes()...)

	// Fragment API: the body is a rendered <li>, not JSON — the no-wasm path.
	mux.HandleFunc("POST /api/todos", func(w http.ResponseWriter, r *http.Request) {
		text := r.FormValue("text")
		if text == "" {
			http.Error(w, "text required", http.StatusBadRequest)
			return
		}
		t := db.Add(text)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Todo-Count", strconv.Itoa(db.Count()))
		if err := ui.TodoItem(t).Render(r.Context(), w); err != nil {
			log.Printf("render todo: %v", err)
		}
	})

	mux.HandleFunc("DELETE /api/todos/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		db.Del(id)
		w.Header().Set("X-Todo-Count", strconv.Itoa(db.Count()))
		w.WriteHeader(http.StatusNoContent)
	})

	return a, mux
}
