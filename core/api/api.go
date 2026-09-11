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
// It does not write the response body for you to break: a handler returns a
// value and the framework encodes it. What a handler may add is headers and
// cookies — r.Header() and r.SetCookie() — because signing someone in is a
// cookie, and an endpoint that cannot set one sends the application off to
// write a second, untyped handler for the one call that needed it.
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
	"reflect"
	"strings"
	"time"

	"github.com/mirairoad/howl-go/core/cache"
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
	// Description is for the reader of the OpenAPI document: what the endpoint
	// does, in a sentence. Name is the label; this is the explanation.
	Description string
	// Errors lists the statuses this endpoint answers with on purpose, beyond
	// the ones the document already derives — 400 for input, 401 and 403 for
	// roles. A 404 or a 409 the handler returns belongs here, so a client
	// generated from the document knows to expect it.
	Errors []int
	// Cache reuses successful responses for Cache.TTL. GET only; the zero
	// value caches nothing. See Cache for who shares an entry.
	Cache Cache
	// Handler receives decoded, validated input and returns a value to encode.
	// Return an *api.Error to choose the status; anything else is a 500 with
	// its details kept server-side.
	Handler func(*Request[Q, B]) (R, error)
}

// Cache is an endpoint's response cache — howl (TS)'s `caching: { ttl }`.
//
//	Cache: api.Cache{TTL: 5 * time.Second},
//
// An entry is keyed by path, query and caller, so it is never served to
// somebody else (howl (TS): `{method}:{url}:{userId}`). The caller is
// Config.Identity; without one, a request carrying Cookie or Authorization is
// its own caller, keyed by a hash of those headers. Config.Authorize still runs
// on every request, hit or miss: a cached answer is not a way past the roles.
//
// Never stored: anything but a 200, and a response that sets a cookie. The
// response carries X-Howl-Cache: hit or miss, and Age on a hit.
//
// It is TTL-based and nothing else — there is no purge. Cache what may be that
// stale: a paid upstream's suggestions, an aggregate over a day. A read that
// must show the write that just happened belongs uncached, or in db's
// document cache, which is invalidated by the write itself.
type Cache struct {
	// TTL is how long a response is reused. Zero disables the cache.
	TTL time.Duration
	// Vary lists request headers the response depends on — Accept-Language,
	// say — so each value gets its own entry.
	Vary []string
}

// Request is what a handler is given: the decoded query and body, plus the
// underlying *http.Request for cookies, headers and path values.
type Request[Q, B any] struct {
	HTTP  *http.Request
	Query Q
	Body  B

	// w is the response the framework will write. Unexported on purpose: the
	// handler adds headers through Header and SetCookie, and the framework
	// still owns the status line and the body, so a handler cannot write half
	// a response and then return an error that has nowhere to go.
	w      http.ResponseWriter
	header http.Header // only for a Request built by hand, in a test
}

// Param returns a {placeholder} from the path.
func (r *Request[Q, B]) Param(name string) string { return r.HTTP.PathValue(name) }

// Header is the response's header map — howl (TS)'s ctx.headers. Whatever is
// set here goes out with the response, including when the handler returns an
// error: clearing a session cookie on a 401 is a real case. Content-Type is the
// framework's, and is overwritten.
func (r *Request[Q, B]) Header() http.Header {
	if r.w == nil {
		if r.header == nil {
			r.header = http.Header{}
		}
		return r.header
	}
	return r.w.Header()
}

// SetCookie adds a Set-Cookie to the response — howl (TS)'s ctx.cookies.set,
// and http.SetCookie without the writer. A cookie with an invalid name is
// dropped silently, as net/http drops it.
//
// A response that sets a cookie is never stored by Spec.Cache.
func (r *Request[Q, B]) SetCookie(c *http.Cookie) {
	if v := c.String(); v != "" {
		r.Header().Add("Set-Cookie", v)
	}
}

// Context is the request context — where core/state values and the request id
// live.
func (r *Request[Q, B]) Context() context.Context { return r.HTTP.Context() }

// Route is a Spec with its type parameters erased, so a slice of them can hold
// endpoints of different shapes — the same trick router.Route uses for pages.
type Route struct {
	Name        string
	Method      string
	Path        string
	Roles       []string
	Description string
	Errors      []int
	Cache       Cache
	// Types records the query, body and response type names for the generated
	// client and the OpenAPI document. Filled by Define.
	Types TypeNames
	// schema carries the instantiated types themselves, which is what makes an
	// accurate OpenAPI document possible without a schema library: generics
	// erase at run time, but reflect still knows what Q, B and R were.
	schema shapes
	handle func(Config) http.Handler
}

type shapes struct{ query, body, response reflect.Type }

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
	// Cache stores the responses of endpoints that declare Spec.Cache. Nil is
	// an in-process LRU of 1000 entries shared by every endpoint registered
	// together — per process, so replicas each keep their own. Hand in a shared
	// store (core/cache, a Redis adapter) for anything replicated; the same
	// value serves db's document cache.
	Cache cache.Store
	// Identity names the caller for Spec.Cache: two requests share an entry
	// only when it returns the same string. Return "" for an anonymous caller,
	// and every anonymous caller shares one entry per URL.
	//
	// Nil keys a request carrying Cookie or Authorization by those headers
	// themselves — never wrong, but every browser with any cookie at all, a
	// CSRF token included, gets its own entry. An application with sessions
	// does better returning the user id.
	Identity func(r *http.Request) string
}

// Define erases the type parameters and produces the registerable Route.
func Define[Q, B, R any](s Spec[Q, B, R]) Route {
	if s.Method == "" {
		s.Method = http.MethodGet
	}
	if s.Handler == nil {
		panic("api: " + s.Name + " has no Handler")
	}
	if s.Cache.TTL < 0 {
		panic("api: " + s.Name + " has a negative Cache.TTL")
	}
	return Route{
		Name:        s.Name,
		Method:      strings.ToUpper(s.Method),
		Path:        s.Path,
		Roles:       s.Roles,
		Description: s.Description,
		Errors:      s.Errors,
		Cache:       s.Cache,
		Types:       TypeNames{Query: typeName[Q](), Body: typeName[B](), Response: typeName[R]()},
		schema:      shapes{query: reflectType[Q](), body: reflectType[B](), response: reflectType[R]()},
		handle: func(cfg Config) http.Handler {
			var run http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req := &Request[Q, B]{HTTP: r, w: w}
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
			})
			if s.Cache.TTL > 0 {
				run = mw.Cache{Store: cfg.Cache, TTL: s.Cache.TTL, Vary: s.Cache.Vary, Key: cfg.cacheKey}.Handler(run)
			}
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Before the cache, always: an entry filled by an admin must
				// not become the way a caller without the role reads it.
				if err := authorize(cfg, r, s.Roles); err != nil {
					fail(cfg, w, r, err)
					return
				}
				run.ServeHTTP(w, r)
			})
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
		if rt.Cache.TTL > 0 && cfg.Cache == nil {
			cfg.Cache = cache.NewLRU(1000) // one store for the table, not one per endpoint
			break
		}
	}
	for _, rt := range routes {
		if len(rt.Roles) > 0 && cfg.Authorize == nil {
			panic(fmt.Sprintf("api: %q declares roles %v but Config.Authorize is nil — every caller would be let through", rt.Name, rt.Roles))
		}
		if rt.Path == "" {
			panic("api: " + rt.Name + " has no path (generated tables call api.At)")
		}
		// A cached POST would answer the second submission of a form with the
		// first one's result, and never run it.
		if rt.Cache.TTL > 0 && rt.Method != http.MethodGet {
			panic(fmt.Sprintf("api: %q caches a %s — only GET responses can be reused", rt.Name, rt.Method))
		}
		if err := checkPathFields(rt); err != nil {
			panic("api: " + rt.Name + ": " + err.Error())
		}
		mux.Handle(rt.Method+" "+rt.Path, rt.handle(cfg))
	}
}

// cacheKey is Spec.Cache's key: the path, the query re-encoded so the order of
// its parameters does not matter, and the caller. mw.Cache hashes the result,
// so the cookie that may be in it never reaches a store in the clear.
func (cfg Config) cacheKey(r *http.Request) string {
	var who string
	if cfg.Identity != nil {
		who = cfg.Identity(r)
	} else if auth, cookie := r.Header.Get("Authorization"), r.Header.Get("Cookie"); auth != "" || cookie != "" {
		who = "credentials\x00" + auth + "\x00" + cookie
	}
	return "api\x00" + r.URL.Path + "?" + r.URL.Query().Encode() + "\x00" + who
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
