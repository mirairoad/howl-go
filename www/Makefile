.PHONY: all docs routes wasm run static clean

MODULE := github.com/mirairoad/howl-go/www
BIN    := www

all: routes wasm
	go build -o $(BIN) .

# Markdown -> templ pages. goldmark is a dependency of the tool only, never of
# the server or the wasm bundle.
docs:
	go run github.com/mirairoad/howl-go/core/cmd/mddocs

routes: docs
	go run github.com/mirairoad/howl-go/core/cmd/fsroutes -module $(MODULE)/client/pages
	go tool templ generate

wasm: routes
	GOOS=js GOARCH=wasm go build -o client/public/views.wasm ./wasm
	install -m 644 "$$(go env GOROOT)/lib/wasm/wasm_exec.js" client/public/wasm_exec.js

run: all
	./$(BIN)

static: all
	./$(BIN) -static ./dist

clean:
	rm -f $(BIN) client/public/views.wasm client/public/wasm_exec.js
	rm -rf dist client/pages/docs
