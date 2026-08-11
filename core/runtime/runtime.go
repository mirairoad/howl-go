// Package runtime holds the framework's client-side code: the router, the
// prefetcher, head merging, island hydration and the wasm bridge. It is
// embedded here rather than copied into each application, so every app serves
// the same /static/app.js.
package runtime

import (
	"embed"
	"io/fs"
)

//go:embed app.js transitions.css
var files embed.FS

// Assets is the framework's static files, served under /static/.
func Assets() fs.FS { return files }
