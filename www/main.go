package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"

	"github.com/mirairoad/howl-go/core/app"
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
	})

	if *static != "" {
		if err := a.Export(*static); err != nil {
			log.Fatal(err)
		}
		return
	}
	log.Fatal(a.Listen(a.Mux()))
}
