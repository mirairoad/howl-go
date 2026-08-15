.PHONY: all docs routes llms wasm-check run static clean

MODULE := github.com/mirairoad/howl-go/www
BIN    := www

all: routes llms
	go build -o $(BIN) .

# The framework's LLM context file, served at /llms.txt for agents that ask.
llms:
	cp ../llms.txt client/public/llms.txt

# Markdown -> templ pages. goldmark is a dependency of the tool only, never of
# the server.
docs:
	go run github.com/mirairoad/howl-go/core/cmd/mddocs

routes: docs
	go run github.com/mirairoad/howl-go/core/cmd/fsroutes -module $(MODULE)/client/pages
	go tool templ generate

# No wasm target, deliberately. Not one route here is .client, so
# router.NeedsWasm publishes an empty list, the client is told there is nothing
# to load, and a 5.5 MB binary would be built on every change to be downloaded
# by nobody. A public content site is also the exact case the payload argument
# rules out — 1.63 MB gzipped to save a round-trip that prefetch-on-intent
# already covers. The AOT renderer is demonstrated by examples/toy_app, which is
# behind no such constraint. Renaming a page to *.client.templ and restoring
# this target is all it would take to change the answer.

# Not part of `all`: compiles the browser renderer without keeping the binary,
# so www/wasm/main.go cannot rot unnoticed while nothing builds it.
wasm-check:
	GOOS=js GOARCH=wasm go build -o /dev/null ./wasm

run: all
	./$(BIN)

static: all
	./$(BIN) -static ./dist

clean:
	rm -f $(BIN) client/public/llms.txt
	rm -rf dist client/pages/docs
