// Package api is the typed endpoint layer: one file per endpoint, declaring
// its method, its path, who may call it, and the Go types of its query, body
// and response.
//
//	var Ping = api.Define(api.Spec[PingQuery, api.None, PingResponse]{
//	    Name:  "Ping",
//	    Roles: nil,                     // public
//	    Handler: func(r *api.Request[PingQuery, api.None]) (PingResponse, error) {
//	        return PingResponse{OK: true, Page: r.Query.Page}, nil
//	    },
//	})
//
// The spec is an ordinary Go composite literal, so it is checked by the
// compiler rather than by a schema library at runtime — which is the whole
// reason this layer is smaller in Go than the TypeScript original: the types
// the handler is written against ARE the contract, so nothing has to be
// derived from them.
//
// # What this package deliberately does not do
//
// It does not know what a role is. An endpoint declares strings; the
// application supplies Config.Authorize and decides what they mean. Permissions
// belong to the application — they need its user model, its session, its
// database — and a framework that guesses at them is a framework you have to
// fight. Same for logging and correlation ids: they arrive through core/mw if
// you want them.
//
// It is also JSON-only. An endpoint speaking protobuf, serving a file, or
// streaming is an ordinary http.Handler on the mux; wrapping those in a typed
// envelope would buy nothing.
//
// # Two halves, one of which runs in a browser
//
// This file is the server half and is excluded from the wasm build. The client
// half — Transport, Call, Error, None, Validator — has no net/http in it at
// all, because linking that package into a wasm binary costs 2.05 MB gzipped
// for a TLS stack the browser already has.
//
// The types an endpoint declares are shared by both, which is the whole point:
// the generated client sends what the handler validates, and a renamed field
// fails to compile on both sides.
//go:build !(js && wasm)

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mirairoad/howl-go/core/mw"
)

// Spec is one endpoint's contract.
type Spec[Q, B, R any] struct {
	// Name is for humans: logs, generated client method names, OpenAPI.
	Name string
	// Method defaults to GET.
	Method string
	// Path overrides the path derived from the file's location. Use it for the
	// shapes a directory cannot express, like /healthz.
	Path string
	// Roles is passed verbatim to Config.Authorize. Empty means public.
	Roles []string
	// Handler receives decoded, validated input and returns a value to encode.
	// Return an *api.Error to choose the status; anything else is a 500 with
	// its details kept server-side.
	Handler func(*Request[Q, B]) (R, error)
}

// Request is what a handler is given: the decoded query and body, plus the
// underlying *http.Request for cookies, headers and path values.
type Request[Q, B any] struct {
	HTTP  *http.Request
	Query Q
	Body  B
}

// Param returns a {placeholder} from the path.
func (r *Request[Q, B]) Param(name string) string { return r.HTTP.PathValue(name) }

// Context is the request context — where core/state values and the request id
// live.
func (r *Request[Q, B]) Context() context.Context { return r.HTTP.Context() }

// Route is a Spec with its type parameters erased, so a slice of them can hold
// endpoints of different shapes — the same trick router.Route uses for pages.
type Route struct {
	Name   string
	Method string
	Path   string
	Roles  []string
	// Types records the query, body and response type names for the generated
	// client and the OpenAPI document. Filled by Define.
	Types  TypeNames
	handle func(Config) http.HandlerFunc
}

// TypeNames are the Go type names behind the erased Route.
type TypeNames struct{ Query, Body, Response string }

// Config is what the application supplies once, and every endpoint shares.
type Config struct {
	// Authorize is the application's permission layer. howl-go has no user
	// model and no idea what a role is; it hands over the strings the endpoint
	// declared and honours the answer. Return nil to allow, or an *api.Error
	// to reject with a chosen status.
	//
	// A route that declares roles with no Authorize configured is a wiring
	// mistake that would otherwise serve private data to everyone, so it
	// panics at registration rather than at 3am.
	Authorize func(r *http.Request, roles []string) error
	// OnError observes every failed request. The response is already decided;
	// this is for logging and metrics.
	OnError func(r *http.Request, err error)
	// Log defaults to slog.Default().
	Log *slog.Logger
	// Prefix is prepended to every derived path. Default "/api".
	Prefix string
}

// Define erases the type parameters and produces the registerable Route.
func Define[Q, B, R any](s Spec[Q, B, R]) Route {
	if s.Method == "" {
		s.Method = http.MethodGet
	}
	if s.Handler == nil {
		panic("api: " + s.Name + " has no Handler")
	}
	return Route{
		Name:   s.Name,
		Method: strings.ToUpper(s.Method),
		Path:   s.Path,
		Roles:  s.Roles,
		Types:  TypeNames{Query: typeName[Q](), Body: typeName[B](), Response: typeName[R]()},
		handle: func(cfg Config) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				if err := authorize(cfg, r, s.Roles); err != nil {
					fail(cfg, w, r, err)
					return
				}
				req := &Request[Q, B]{HTTP: r}
				if err := decodeQuery(r, &req.Query); err != nil {
					fail(cfg, w, r, err)
					return
				}
				if err := decodeBody(r, &req.Body); err != nil {
					fail(cfg, w, r, err)
					return
				}
				// A Validate that returns a plain error still answers 400: it
				// ran before the handler, so by definition the request was
				// wrong, and a domain type should not have to import this
				// package just to say so.
				if err := validate(req.Query); err != nil {
					fail(cfg, w, r, badRequest(err))
					return
				}
				if err := validate(req.Body); err != nil {
					fail(cfg, w, r, badRequest(err))
					return
				}
				out, err := s.Handler(req)
				if err != nil {
					fail(cfg, w, r, err)
					return
				}
				write(w, out)
			}
		},
	}
}

// At sets the method and path a Route is registered under. The generator calls
// it, so an endpoint's location on disk is its URL and its method without
// either being written twice.
//
// Both are assigned, not defaulted: the generator has already resolved the
// precedence between the file name and an explicit Spec.Method or Spec.Path.
// Defaulting here instead was a bug worth remembering — the file-name modifier
// in logs/index.post.api.go never reached the table, both /api/logs endpoints
// registered as GET, and ServeMux panicked at startup with two identical
// patterns.
func At(method, path string, r Route) Route {
	if method != "" {
		r.Method = strings.ToUpper(method)
	}
	if path != "" {
		r.Path = path
	}
	return r
}

// Register mounts every route on the mux. Patterns are "METHOD /path", so Go's
// own router does the matching and a duplicate is a startup panic rather than
// a route that silently never runs.
func Register(mux *http.ServeMux, cfg Config, routes ...Route) {
	if cfg.Prefix == "" {
		cfg.Prefix = "/api"
	}
	for _, rt := range routes {
		if len(rt.Roles) > 0 && cfg.Authorize == nil {
			panic(fmt.Sprintf("api: %q declares roles %v but Config.Authorize is nil — every caller would be let through", rt.Name, rt.Roles))
		}
		if rt.Path == "" {
			panic("api: " + rt.Name + " has no path (generated tables call api.At)")
		}
		mux.HandleFunc(rt.Method+" "+rt.Path, rt.handle(cfg))
	}
}

// Routes is sugar for building a table by hand, in tests or a small app.
func Routes(rs ...Route) []Route { return rs }

func authorize(cfg Config, r *http.Request, roles []string) error {
	if len(roles) == 0 || cfg.Authorize == nil {
		return nil
	}
	return cfg.Authorize(r, roles)
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

func write(w http.ResponseWriter, out any) {
	code := http.StatusOK
	if s, ok := out.(Status); ok && s.Status() != 0 {
		code = s.Status()
	}
	if _, isNone := out.(None); isNone {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(out); err != nil {
		// The status line is already out; there is nothing to say to the
		// client. The server log is the only place this can be reported.
		slog.Error("api: encode response", slog.Any("err", err))
	}
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

func fail(cfg Config, w http.ResponseWriter, r *http.Request, err error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.OnError != nil {
		cfg.OnError(r, err)
	}

	id := mw.ID(r.Context())
	body := errorBody{CorrelationID: id}
	code := http.StatusInternalServerError

	var deliberate *Error
	if errors.As(err, &deliberate) {
		code, body.Error, body.Field = deliberate.Code, deliberate.Message, deliberate.Field
		if code >= 500 {
			log.Error("api", slog.String("path", r.URL.Path), slog.Any("err", err), slog.String("id", id))
		}
	} else {
		// An unexpected error is not a message for the caller: it can name a
		// table, a file path or a hostname. The log gets the real thing.
		body.Error = "internal server error"
		log.Error("api", slog.String("path", r.URL.Path), slog.Any("err", err), slog.String("id", id))
	}

	if id != "" {
		w.Header().Set("X-Correlation-Id", id)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body) //nolint:errcheck
}
