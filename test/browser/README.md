# Browser tests

The real `core/runtime/app.js`, booted in jsdom. Covers what only a browser can: the morph
behind `Element.Render`, row transitions, the fragment swap and head merge, the build-drift
reload, `.raw` routes and link interception — and, in `wasm.test.mjs`, the SPA path: what
app.js does for a `.client` route with the renderer stood in by three stub globals. The
render itself is covered twice elsewhere, on the host in `examples/toy_app/wasm/render` and
with the real binary in `../e2e`. `alive.test.mjs` boots the dev client and checks it hands
its connection back on `pagehide`.

```bash
make test-browser        # from the repo root; installs jsdom into this directory
```

Node is a dependency of this directory only. The framework's `go.mod`, `make` and
`make test` do not touch it.
