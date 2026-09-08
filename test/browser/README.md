# Browser tests

The real `core/runtime/app.js`, booted in jsdom. Covers what only a browser can: the morph
behind `Element.Render`, row transitions, the fragment swap and head merge, the build-drift
reload, `.raw` routes and link interception.

```bash
make test-browser        # from the repo root; installs jsdom into this directory
```

Node is a dependency of this directory only. The framework's `go.mod`, `make` and
`make test` do not touch it.
