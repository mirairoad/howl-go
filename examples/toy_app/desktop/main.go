// Command toy_desktop is the toy app in a native window. It is the same
// application as ../main.go — same routes, same components, same wasm renderer
// — differing only in how the listener is opened.
//
// Two ways to run it:
//
//	./toy_desktop                              its own loopback server
//	./toy_desktop -attach :9000                the `howl dev` front door
//
// The second is the development loop. `howl dev` restarts the server binary on
// every save but keeps its proxy port up, so the window connects once and the
// dev client reloads its content in place — no window is killed, nothing
// flashes, and the wasm renderer is rebuilt underneath it.
package main

import (
	"flag"
	"log"
	"log/slog"

	"github.com/mirairoad/howl-go/core/console"
	"github.com/mirairoad/howl-go/desktop"
	"github.com/mirairoad/howl-go/examples/toy_app/boot"
)

func main() {
	attach := flag.String("attach", "", "point the window at an already-running server, e.g. :9000")
	debug := flag.Bool("debug", false, "enable the webview inspector and debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	console.Setup(console.Options{Level: level})

	a, mux := boot.New()

	// Not log.Fatal(desktop.Run(...)): closing the window is a successful exit
	// and returns nil, so the a.Listen idiom would print "<nil>" and exit 1.
	if err := desktop.Run(a, mux, desktop.Options{
		Title:  "howl-go — toy",
		Width:  1180,
		Height: 820,
		Attach: *attach,
		Debug:  *debug,
	}); err != nil {
		log.Fatal(err)
	}
}
