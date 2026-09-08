.PHONY: all core db test test-db-pg test-browser sync-llms toy www hello desktop run-desktop dev-desktop dev-toy dev-www clean

APPS := examples/toy_app www

all: core db toy www

core: sync-llms
	go build ./core/...

# The document store is optional and imported by nothing in core/, so it
# builds and tests on its own.
db:
	go build ./db/...

test:
	go test ./core/... ./db/...

# The live Postgres conformance run. Its driver lives in a nested module so
# the framework's go.mod keeps its single dependency.
#
#   docker run -d --name howl-conf-pg -p 54329:5432 \
#     -e POSTGRES_PASSWORD=conf -e POSTGRES_DB=howl_conformance postgres:16-alpine
test-db-pg:
	cd db/pg/livetest && PG_URL=$${PG_URL:-postgres://postgres:conf@localhost:54329/howl_conformance} go test ./...

# The client runtime and the morph can only be tested in a browser. This runs
# the real app.js in jsdom. Node is a dependency of test/browser only — never
# of the framework, never of `make` or `make test` — which is why it is a
# separate target you have to ask for.
test-browser:
	cd test/browser && npm install --no-audit --no-fund --loglevel=error && npm test

# llms.txt is the source of truth at the repo root. `howl mcp` embeds a copy so
# the conventions tool answers the same way from a downloaded module as from a
# checkout, and www serves one at /llms.txt. Both are copies of this file.
sync-llms:
	@cp llms.txt core/cmd/howl/llms.txt
	@cp core/cmd/howl/frontend.md www/docs/11-frontend.md

toy:
	$(MAKE) -C examples/toy_app

www:
	$(MAKE) -C www

hello:
	$(MAKE) -C examples/hello

# Watch, rebuild, restart, reload the browser. The port stays up across
# restarts, so nothing in the browser has to be reloaded past a dead socket.
dev-toy:
	go run ./core/cmd/howl dev -dir examples/toy_app -addr :9000

dev-www:
	go run ./core/cmd/howl dev -dir www -addr :9001 \
		-pre "go run github.com/mirairoad/howl-go/core/cmd/mddocs"

# The toy app in an OS-native window — WKWebView on macOS, WebKitGTK on Linux.
# Not in `all`: it is a nested cgo module, so it must be asked for.
desktop:
	$(MAKE) -C examples/toy_app desktop

run-desktop: desktop
	./examples/toy_app/toy_desktop

# The desktop development loop. `howl dev` rebuilds and restarts the server on
# save but keeps its front door open, so the window attaches once and the dev
# client reloads the content in place — the window itself is never killed.
#
# howl is built rather than `go run`, because the trap has to kill the dev
# server itself and `go run` would leave it orphaned behind its wrapper.
dev-desktop: desktop
	@go build -o /tmp/howl-dev ./core/cmd/howl
	@/tmp/howl-dev dev -dir examples/toy_app -addr :9000 & \
		dev=$$!; trap "kill $$dev 2>/dev/null" EXIT INT TERM; \
		./examples/toy_app/toy_desktop -attach :9000

# Run one or the other; they bind the same port by default.
run-toy: toy
	./examples/toy_app/toy_app

run-www: www
	./www/www

clean:
	$(MAKE) -C examples/toy_app clean
	$(MAKE) -C www clean
	$(MAKE) -C examples/hello clean
