//go:build js && wasm

package api

import (
	"context"

	"github.com/mirairoad/howl-go/core/dom"
)

// do performs the request with the browser's own fetch(), through core/dom.
//
// The alternative — net/http, which under GOOS=js is also backed by fetch — is
// a 2.05 MB gzipped import, because the linker cannot tell that its TLS stack,
// certificate verification and connection pooling are all unreachable in a
// browser. Measured on an otherwise empty wasm binary: 0.51 MB with this, 2.56
// MB with net/http.
func do(ctx context.Context, t *Transport, method, url string, payload []byte) (response, error) {
	header := map[string]string{"Accept": "application/json"}
	if payload != nil {
		header["Content-Type"] = "application/json"
	}
	for name, values := range t.Header {
		if len(values) > 0 {
			header[name] = values[0]
		}
	}
	status, body, err := dom.Fetch(ctx, method, url, payload, header)
	if err != nil {
		return response{}, err
	}
	return response{Status: status, Body: body}, nil
}
