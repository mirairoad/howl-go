.PHONY: all core sync-llms toy www hello dev-toy dev-www clean

APPS := examples/toy_app www

all: core toy www

core: sync-llms
	go build ./core/...

# llms.txt is the source of truth at the repo root. `howl mcp` embeds a copy so
# the conventions tool answers the same way from a downloaded module as from a
# checkout, and www serves one at /llms.txt. Both are copies of this file.
sync-llms:
	@cp llms.txt core/cmd/howl/llms.txt

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

# Run one or the other; they bind the same port by default.
run-toy: toy
	./examples/toy_app/toy_app

run-www: www
	./www/www

clean:
	$(MAKE) -C examples/toy_app clean
	$(MAKE) -C www clean
	$(MAKE) -C examples/hello clean
