package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/mirairoad/howl-go/core/api"
	"github.com/mirairoad/howl-go/core/app"
	"github.com/mirairoad/howl-go/core/console"
	"github.com/mirairoad/howl-go/core/mw"
	"github.com/mirairoad/howl-go/examples/toy_app/client/pages"
	"github.com/mirairoad/howl-go/examples/toy_app/client/store"
	"github.com/mirairoad/howl-go/examples/toy_app/client/ui"
	"github.com/mirairoad/howl-go/examples/toy_app/server/apis"
	apistore "github.com/mirairoad/howl-go/examples/toy_app/server/apis/store"
)

//go:generate go run github.com/mirairoad/howl-go/core/cmd/fsroutes -module github.com/mirairoad/howl-go/examples/toy_app/client/pages
//go:generate go run github.com/mirairoad/howl-go/core/cmd/fsapis -dir server/apis -module github.com/mirairoad/howl-go/examples/toy_app/server/apis -client client/api/api_gen.go -client-pkg apiclient

//go:embed client/public
var publicFS embed.FS

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

func main() {
	static := flag.String("static", "", "render routes to this directory and exit")
	debug := flag.Bool("debug", false, "log at debug level")
	flag.Parse()

	// Tinted columns in a terminal, JSON into a pipe. Everything that logs
	// through slog — this app, core/app, core/mw — comes out the same way.
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	console.Setup(console.Options{Level: level})

	db.Add("render the same component three ways")
	db.Add("keep island state across navigation")

	public, err := fs.Sub(publicFS, "client/public")
	if err != nil {
		log.Fatal(err)
	}

	a := app.New(app.Config{
		Routes:   pages.FsClientRoutes(),
		Shell:    pages.App,
		NotFound: pages.NotFound,
		Public:   public,
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

	if *static != "" {
		if err := a.Export(*static); err != nil {
			log.Fatal(err)
		}
		return
	}

	mux := a.Mux()

	// The JSON API now lives in server/apis, one file per endpoint, generated
	// into apis_gen.go and into the typed client the pages call. Registering it
	// is two lines and there is no URL string in this file any more.
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

	log.Fatal(a.Listen(mux))
}
