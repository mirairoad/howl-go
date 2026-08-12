//go:build !(js && wasm)

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// maxBody caps a JSON request body. Without it a single request can ask the
// process to allocate whatever the client feels like sending.
const maxBody = 1 << 20 // 1 MiB

// decodeBody reads JSON into dst. None means the endpoint takes no body, and
// then nothing is read at all — a GET with a stray body is not an error worth
// inventing.
func decodeBody[B any](r *http.Request, dst *B) error {
	if _, none := any(*dst).(None); none {
		return nil
	}
	if r.Body == nil {
		return BadRequest("a JSON body is required")
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	// Unknown fields are an error: a client sending `{"emial": "..."}` has a
	// bug, and silently dropping it produces an empty field the handler then
	// has to explain.
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return Invalid(typeErr.Field, "expected "+typeErr.Type.String())
		}
		if errors.Is(err, io.EOF) {
			return BadRequest("a JSON body is required")
		}
		return BadRequest("invalid JSON: " + err.Error())
	}
	return nil
}

// decodeQuery fills dst from the URL query string, using `query:"name"` tags
// and falling back to the lowercased field name.
//
// Supported: string, bool, the int and uint widths, float32/64, time.Time
// (RFC3339), and pointers to any of those for "was it supplied at all". A
// field of any other type is a programming error and says so at startup rather
// than silently staying zero.
func decodeQuery[Q any](r *http.Request, dst *Q) error {
	if _, none := any(*dst).(None); none {
		return nil
	}
	v := reflect.ValueOf(dst).Elem()
	t := v.Type()
	if t.Kind() != reflect.Struct {
		return fmt.Errorf("api: query type %s is not a struct", t)
	}
	values := r.URL.Query()

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
		raw := strings.TrimSpace(values.Get(name))
		if raw == "" {
			continue
		}
		if err := setField(v.Field(i), raw); err != nil {
			return Invalid(name, err.Error())
		}
	}
	return nil
}

func setField(target reflect.Value, raw string) error {
	if target.Kind() == reflect.Pointer {
		created := reflect.New(target.Type().Elem())
		if err := setField(created.Elem(), raw); err != nil {
			return err
		}
		target.Set(created)
		return nil
	}

	// time.Time is a struct, so it has to be handled before the struct case
	// below turns into "unsupported".
	if target.Type() == reflect.TypeOf(time.Time{}) {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return errors.New("must be an RFC3339 timestamp")
		}
		target.Set(reflect.ValueOf(parsed))
		return nil
	}

	switch target.Kind() {
	case reflect.String:
		target.SetString(raw)
	case reflect.Bool:
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return errors.New("must be true or false")
		}
		target.SetBool(parsed)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		parsed, err := strconv.ParseInt(raw, 10, target.Type().Bits())
		if err != nil {
			return errors.New("must be a whole number")
		}
		target.SetInt(parsed)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		parsed, err := strconv.ParseUint(raw, 10, target.Type().Bits())
		if err != nil {
			return errors.New("must be a non-negative whole number")
		}
		target.SetUint(parsed)
	case reflect.Float32, reflect.Float64:
		parsed, err := strconv.ParseFloat(raw, target.Type().Bits())
		if err != nil {
			return errors.New("must be a number")
		}
		target.SetFloat(parsed)
	default:
		return fmt.Errorf("api: unsupported query field type %s", target.Type())
	}
	return nil
}

// reflectType is the instantiated type behind a type parameter.
func reflectType[T any]() reflect.Type {
	var zero T
	return reflect.TypeOf(&zero).Elem()
}

// typeName is the Go name of a type parameter, for the generated client and the
// OpenAPI document. Generics erase at run time but reflect still knows the
// instantiated type.
func typeName[T any]() string {
	var zero T
	t := reflect.TypeOf(&zero).Elem()
	if t.Name() == "" {
		return t.String()
	}
	if t.PkgPath() == "" {
		return t.Name()
	}
	pkg := t.PkgPath()
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		pkg = pkg[i+1:]
	}
	return pkg + "." + t.Name()
}
