package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// The client half
//
// cmd/fsapis generates one method per endpoint over this transport, so the
// generated file stays a list of one-liners and everything that could be wrong
// lives here, once, tested.
//
// Nothing in this file imports net/http. Only the two transport files do, and
// only one of them is compiled: net/http on a server, the browser's own fetch()
// under GOOS=js. Measured, that swap is the difference between 2.56 MB and
// 0.51 MB gzipped for a wasm binary — net/http drags in crypto/tls and
// crypto/x509 to re-implement what the browser is already doing.
//
// The generated client imports the SAME query, body and response types the
// handler is written against. Nothing is translated, mirrored or kept in sync:
// rename a field and both sides fail to compile. That is the part the
// TypeScript generator cannot have — zod schemas are runtime values, so it has
// to re-derive a type declaration for every endpoint.
// ---------------------------------------------------------------------------

// Transport is the shared half of a generated client.
type Transport struct {
	// BaseURL is empty for same-origin, which is what a browser wants.
	BaseURL string
	// Header is sent on every request — an Authorization token, a tenant id.
	// A plain map rather than http.Header, so this struct stays free of
	// net/http and can exist in a wasm build.
	Header map[string][]string
	// Timeout applies when the caller's context has no deadline of its own.
	Timeout time.Duration
}

func NewTransport(baseURL string) *Transport {
	return &Transport{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Header:  map[string][]string{},
		Timeout: 30 * time.Second,
	}
}

// Set adds a header to every request this transport makes.
func (t *Transport) Set(name, value string) *Transport {
	if t.Header == nil {
		t.Header = map[string][]string{}
	}
	t.Header[name] = []string{value}
	return t
}

// response is what a platform transport returns: the two things Call needs and
// nothing that names a platform type.
type response struct {
	Status int
	Body   []byte
}

// Call performs one request and decodes its response.
//
// query and body may be nil. A None response type means the endpoint answers
// 204 and there is nothing to decode.
func Call[R any](ctx context.Context, t *Transport, method, path string, query, body any) (R, error) {
	var out R
	if t == nil {
		return out, fmt.Errorf("api: nil transport")
	}

	// Validation runs on this side too, against the same rule the server will
	// apply. Catching it here costs nothing and saves a round-trip whose only
	// possible answer is the 400 we already know about.
	if query != nil {
		if err := validate(query); err != nil {
			return out, badRequest(err)
		}
	}
	if body != nil {
		if err := validate(body); err != nil {
			return out, badRequest(err)
		}
	}

	target := t.BaseURL + path
	if query != nil {
		values, err := EncodeQuery(query)
		if err != nil {
			return out, err
		}
		if encoded := values.Encode(); encoded != "" {
			target += "?" + encoded
		}
	}

	var payload []byte
	if body != nil {
		if _, none := body.(None); !none {
			encoded, err := json.Marshal(body)
			if err != nil {
				return out, fmt.Errorf("api: encode body: %w", err)
			}
			payload = encoded
		}
	}

	if t.Timeout > 0 {
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, t.Timeout)
			defer cancel()
		}
	}

	res, err := do(ctx, t, method, target, payload)
	if err != nil {
		return out, err
	}
	if res.Status >= 400 {
		return out, decodeError(res)
	}
	if res.Status == 204 || len(res.Body) == 0 {
		return out, nil
	}
	if _, none := any(out).(None); none {
		return out, nil
	}
	if err := json.Unmarshal(res.Body, &out); err != nil {
		return out, fmt.Errorf("api: decode response from %s: %w", path, err)
	}
	return out, nil
}

// decodeError turns the server's error envelope back into the *api.Error the
// handler returned, so a caller can errors.As it and read the status and field
// instead of matching on a message string.
func decodeError(res response) error {
	var body errorBody
	json.Unmarshal(res.Body, &body) //nolint:errcheck // a non-JSON error body is still an error
	if body.Error == "" {
		body.Error = strings.TrimSpace(string(res.Body))
	}
	if body.Error == "" {
		body.Error = "request failed"
	}
	err := &Error{Code: res.Status, Message: body.Error, Field: body.Field}
	if body.CorrelationID != "" {
		// The id is the only thing tying this to the server's log line; losing
		// it here is losing the one thing that makes the failure diagnosable.
		err.Message += " (correlation id " + body.CorrelationID + ")"
	}
	return err
}

// EncodeQuery is the inverse of decodeQuery: a query struct back into URL
// values, using the same `query:"name"` tags. Zero values are omitted, so an
// unset filter does not become `?service=`.
func EncodeQuery(v any) (url.Values, error) {
	values := url.Values{}
	if v == nil {
		return values, nil
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return values, nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, fmt.Errorf("api: query must be a struct, got %s", rv.Kind())
	}
	t := rv.Type()
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name := field.Tag.Get("query")
		if name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		if encoded, ok := encodeValue(rv.Field(i)); ok {
			values.Set(name, encoded)
		}
	}
	return values, nil
}

func encodeValue(v reflect.Value) (string, bool) {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "", false
		}
		return encodeValue(v.Elem())
	}
	if v.Type() == reflect.TypeOf(time.Time{}) {
		at := v.Interface().(time.Time)
		if at.IsZero() {
			return "", false
		}
		return at.UTC().Format(time.RFC3339Nano), true
	}
	if v.IsZero() {
		return "", false
	}
	switch v.Kind() {
	case reflect.String:
		return v.String(), true
	case reflect.Bool:
		return strconv.FormatBool(v.Bool()), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10), true
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'g', -1, 64), true
	}
	return "", false
}

// Path fills {placeholders} in a generated path. Values are escaped, so an id
// containing a slash cannot reach into another route.
func Path(pattern string, values ...string) string {
	for _, value := range values {
		open := strings.Index(pattern, "{")
		if open < 0 {
			break
		}
		closeAt := strings.Index(pattern[open:], "}")
		if closeAt < 0 {
			break
		}
		pattern = pattern[:open] + url.PathEscape(value) + pattern[open+closeAt+1:]
	}
	return pattern
}
