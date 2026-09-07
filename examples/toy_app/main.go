package main

import (
	"flag"
	"log"
	"log/slog"

	"github.com/mirairoad/howl-go/core/console"
	"github.com/mirairoad/howl-go/examples/toy_app/boot"
)

//go:generate go run github.com/mirairoad/howl-go/core/cmd/fsroutes -module github.com/mirairoad/howl-go/examples/toy_app/client/pages
//go:generate go run github.com/mirairoad/howl-go/core/cmd/fsapis -dir server/apis -module github.com/mirairoad/howl-go/examples/toy_app/server/apis -client client/api/api_gen.go -client-pkg apiclient

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

	a, mux := boot.New()

	if *static != "" {
		if err := a.Export(*static); err != nil {
			log.Fatal(err)
		}
		return
	}

	log.Fatal(a.Listen(mux))
}
