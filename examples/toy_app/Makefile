.PHONY: all routes wasm run slow static desktop run-desktop clean

MODULE  := github.com/mirairoad/howl-go/examples/toy_app
BIN     := toy_app
DESKTOP := toy_desktop

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

# The same application in an OS-native window. desktop/ is a nested module
# because it needs cgo and a webview binding, and the framework's go.mod has one
# dependency — so this is not part of `all`, and a machine without WebKitGTK
# still builds everything else.
desktop: all
	cd desktop && go build -o ../$(DESKTOP) .

run-desktop: desktop
	./$(DESKTOP)

clean:
	rm -f $(BIN) $(DESKTOP) client/public/views.wasm client/public/wasm_exec.js
	rm -rf dist
