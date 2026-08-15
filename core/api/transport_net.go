//go:build !(js && wasm)

package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

// do performs the request with net/http. This file is the only place in the
// client half that imports it, and it is excluded from the wasm build — see
// transport_js.go for the browser's side of the same function.
func do(ctx context.Context, t *Transport, method, url string, payload []byte) (response, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return response{}, err
	}
	for name, values := range t.Header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return response{}, err
	}
	defer res.Body.Close()

	// Bounded: a client that trusts a server to be reasonable is a client that
	// can be made to allocate until it dies.
	read, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return response{}, err
	}
	return response{Status: res.StatusCode, Body: read}, nil
}
