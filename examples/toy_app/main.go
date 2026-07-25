package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/mirairoad/howl-go/core/app"
	"github.com/mirairoad/howl-go/examples/toy_app/client/pages"
	"github.com/mirairoad/howl-go/examples/toy_app/client/store"
	"github.com/mirairoad/howl-go/examples/toy_app/client/ui"
)

//go:generate go run github.com/mirairoad/howl-go/core/cmd/fsroutes -module github.com/mirairoad/howl-go/examples/toy_app/client/pages

//go:embed client/public
var publicFS embed.FS

var db = store.New()

func demoMetrics() store.Metrics {
	return store.Metrics{
		Cards: []store.Card{
			{Label: "Active sessions", Value: "12,847", Delta: 4.2},
			{Label: "p95 latency", Value: "184 ms", Delta: -11.5},
			{Label: "Error rate", Value: "0.31%", Delta: -2.0},
			{Label: "Throughput", Value: "9.2k/s", Delta: 8.7},
		},
		Rows: []store.Row{
			{Name: "sydney", Value: 48210}, {Name: "singapore", Value: 39104},
			{Name: "frankfurt", Value: 28755}, {Name: "us-east-1", Value: 91233},
			{Name: "us-west-2", Value: 40188}, {Name: "sao-paulo", Value: 12044},
			{Name: "tokyo", Value: 33590}, {Name: "mumbai", Value: 21877},
		},
	}
}

// data is the application's contribution to every render's context. Pages take
// no arguments — the generated table needs one uniform signature — so this is
// how they receive everything.
func data(ctx context.Context, path string) context.Context {
	ctx = store.WithMetrics(ctx, demoMetrics())
	ctx = store.WithTodos(ctx, db.List())
	return store.WithMeta(ctx, store.Meta{
		RenderedAt: time.Now().Format("15:04:05.000"),
		GoVersion:  runtime.Version(),
		Region:     "us-east-1",
	})
}

func main() {
	static := flag.String("static", "", "render routes to this directory and exit")
	flag.Parse()

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
	})

	if *static != "" {
		if err := a.Export(*static); err != nil {
			log.Fatal(err)
		}
		return
	}

	mux := a.Mux()

	// Data endpoints. Once the wasm renderer is up these are the only things
	// that cross the wire.
	mux.HandleFunc("GET /api/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(demoMetrics())
	})

	mux.HandleFunc("GET /api/todos", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(db.Snapshot())
	})

	// The client has ALREADY applied these ops and re-rendered; this is
	// bookkeeping, not the critical path.
	mux.HandleFunc("POST /api/todos/sync", func(w http.ResponseWriter, r *http.Request) {
		var ops []store.Op
		if err := json.NewDecoder(r.Body).Decode(&ops); err != nil {
			http.Error(w, "bad ops", http.StatusBadRequest)
			return
		}
		for _, op := range ops {
			db.Apply(op)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(db.Snapshot())
	})

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
