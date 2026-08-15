package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"

	"github.com/mirairoad/howl-go/core/app"
	"github.com/mirairoad/howl-go/core/console"
	"github.com/mirairoad/howl-go/core/mw"
	"github.com/mirairoad/howl-go/www/client/pages"
)

//go:generate go run github.com/mirairoad/howl-go/core/cmd/mddocs
//go:generate go run github.com/mirairoad/howl-go/core/cmd/fsroutes -module github.com/mirairoad/howl-go/www/client/pages

//go:embed client/public
var publicFS embed.FS

func main() {
	static := flag.String("static", "", "render routes to this directory and exit")
	addr := flag.String("addr", ":9001", "listen address")
	flag.Parse()

	console.Setup(console.Options{})

	public, err := fs.Sub(publicFS, "client/public")
	if err != nil {
		log.Fatal(err)
	}

	a := app.New(app.Config{
		Routes:   pages.FsClientRoutes(),
		Shell:    pages.App,
		NotFound: pages.NotFound,
		Public:   public,
		Addr:     *addr,
		// A documentation site: every page is public, identical for everyone,
		// and the same few URLs get hammered — exactly what Coalesce is for.
		// Nothing else here needs configuring.
		Use: []mw.Middleware{
			mw.RequestID,
			mw.LogWith(mw.LogOptions{Callers: true, Skip: mw.SkipNoise}),
			mw.Recover(nil),
			mw.Compress{}.Handler,
			(&mw.Coalesce{}).Handler,
		},
	})

	if *static != "" {
		if err := a.Export(*static); err != nil {
			log.Fatal(err)
		}
		return
	}
	mux := a.Mux()

	// llms.txt is a root-level convention, so it does not live under /static/.
	// Agents that read it can write howl-go without guessing at conventions the
	// Go toolchain made unguessable.
	mux.HandleFunc("GET /llms.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.ServeFileFS(w, r, public, "llms.txt")
	})

	log.Fatal(a.Listen(mux))
}
