# howl-go

**Read [`llms.txt`](llms.txt) before writing any code in this repository.** It is the
framework's conventions in one file: routing, page anatomy, the shell contract, the package
layering rule, and the things the Go toolchain refuses to allow. The conventions are not
guessable — Go rejects `_layout.templ` and `[id].templ`, so howl-go's answers look
different from every JS framework on purpose.

The reasoning behind each one is in [`DESIGN-LOG.md`](DESIGN-LOG.md); the reference docs
are `www/docs/*.md`, which also build the documentation site.

## The five things most likely to go wrong

1. **`fsroutes_gen.go` and `*_templ.go` are generated.** Edit the `.templ` source and re-run
   the pipeline: `fsroutes` → `templ generate` → `go build`, always in that order.
2. **Pages may not import `core/app`.** The generated table lives inside the page tree, so
   anything a page imports must stay a leaf: `core/router`, `core/state`, `core/signal`,
   `core/dom`.
3. **Behaviour lives in file names**, dot-separated: `.dyn`, `.client`, `.bare`, `.raw`.
   An unknown modifier is a hard error, deliberately.
4. **`templ` produces markup, `func` does something.** There is no `templ Mount()`.
5. **No `ctx` wrapper, no framework handler type.** Middleware is
   `func(http.Handler) http.Handler`; handlers are `http.Handler`; use `r.Cookie`,
   `r.URL.Query()`, `json.NewEncoder(w)`.

Two commands worth knowing before you edit: `go run ./core/cmd/howl check` enforces the
rules above, and the `.mcp.json` in this repo exposes them (plus the live route and
endpoint tables) as MCP tools.

## Build and test

```bash
make            # core + both example apps
make run-www    # documentation site  -> :9001
make run-toy    # example app         -> :9000
make dev-toy    # watched: rebuild + restart + reload on save
go test ./core/...
go run ./core/cmd/howl check     # the conventions, enforced
```

In an application, the dev loop is `go run github.com/mirairoad/howl-go/core/cmd/howl dev`
— not `go run .`. It regenerates routes, runs templ, rebuilds, restarts and reloads the
browser on save.

`make -C examples/toy_app slow` adds 240 ms of simulated latency to every request — the
condition the whole client runtime exists for.

## House style

Comments explain *why*, not what, and they earn their place: the reason a line exists, the
bug it prevents, the measurement behind a number. Match the density of the surrounding
file. When a claim is measurable, measure it and put the number in the text rather than
describing it as "fast".
