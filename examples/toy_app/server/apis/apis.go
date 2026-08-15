// Package apis is the toy app's endpoint layer: one file per endpoint, each
// declaring its method, path and the Go types of its query, body and response.
//
// The response types live in client/store, not here. That is the rule that
// makes the generated client usable in the browser: this package imports
// api.Define, which only exists in the server build, so anything a wasm page
// needs to import has to live outside it. client/store already compiles for
// both targets — it is the same store the browser mutates.
package apis
