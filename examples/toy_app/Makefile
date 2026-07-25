.PHONY: all routes wasm run slow static clean

MODULE := github.com/mirairoad/howl-go/examples/toy_app
BIN    := toy_app

all: routes wasm
	go build -o $(BIN) .

# Crawl client/pages and regenerate the route table.
routes:
	go run github.com/mirairoad/howl-go/core/cmd/fsroutes -module $(MODULE)/client/pages
	go tool templ generate

wasm: routes
	GOOS=js GOARCH=wasm go build -o client/public/views.wasm ./wasm
	install -m 644 "$$(go env GOROOT)/lib/wasm/wasm_exec.js" client/public/wasm_exec.js

run: all
	./$(BIN)

slow: all
	LATENCY=240ms ./$(BIN)

static: all
	./$(BIN) -static ./dist

clean:
	rm -f $(BIN) client/public/views.wasm client/public/wasm_exec.js
	rm -rf dist
