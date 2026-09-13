# End-to-end tests

`examples/toy_app`, served in-process, driven in a real headless Chromium through
chromedp. This is the only place the actual `views.wasm` runs: the `//go:build js && wasm`
half of the application cannot be compiled into any host test, and the jsdom suite in
`test/browser` stubs the renderer on purpose.

```bash
make test-e2e            # builds the toy app's wasm, then runs this module
HOWL_CHROME=/path/to/chrome make test-e2e   # if chromedp cannot find one
```

A nested module, like `db/pg/livetest`: chromedp and its transitive dependencies never enter
the framework's `go.mod`, and `go test ./...` from the repository root never comes here. The
binary is embedded at compile time — `client/public/views.wasm` has to exist before this
module is built, which is what the Makefile target guarantees and what the test checks first.
