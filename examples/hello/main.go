package main

import (
	"embed"
	"io/fs"
	"log"

	"github.com/mirairoad/howl-go/core/app"
	"example.com/howl-hello/client/pages"
)

//go:generate go run github.com/mirairoad/howl-go/core/cmd/fsroutes -module example.com/howl-hello/client/pages

//go:embed client/public
var publicFS embed.FS

func main() {
	public, err := fs.Sub(publicFS, "client/public")
	if err != nil {
		log.Fatal(err)
	}
	a := app.New(app.Config{
		Routes:   pages.FsClientRoutes(),
		Shell:    pages.App,
		NotFound: pages.NotFound,
		Public:   public,
	})
	log.Fatal(a.Listen(a.Mux()))
}
