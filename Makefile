.PHONY: all routes wasm run static clean

all: routes wasm
	go build -o howl-go .

# Crawl client/pages and regenerate the route table.
routes:
	go run ./tools/fsroutes
	go tool templ generate

# Compile the templ views to WebAssembly so routes render client-side.
wasm: routes
	GOOS=js GOARCH=wasm go build -o client/public/views.wasm ./wasm
	install -m 644 "$$(go env GOROOT)/lib/wasm/wasm_exec.js" client/public/wasm_exec.js

run: all
	./howl-go

# Simulate an overseas client: every server round-trip costs 240ms.
slow: all
	LATENCY=240ms ./howl-go

static: all
	./howl-go -static ./dist

clean:
	rm -f howl-go client/public/views.wasm client/public/wasm_exec.js
	rm -rf dist
