package api

// The methods an endpoint may declare. This file carries no build constraint:
// the generated client speaks the same vocabulary as the server, and it
// compiles for wasm.
//
// Written as literals rather than as http.MethodGet and friends, because
// net/http under GOOS=js is 2.05 MB gzipped of TLS and certificate handling to
// re-implement what the browser already does — the reason core/dom has fetch
// instead. Seven string constants are not worth that.

// Method is an HTTP method. The constants below are the ones an endpoint may
// declare; anything else panics at registration, which is startup rather than
// the first request that quietly fails to match.
//
// A named string type, so an untyped constant still assigns — Spec{Method:
// "POST"} and api.At("POST", …) both compile unchanged — while a variable of
// the wrong type does not.
type Method string

// The methods an endpoint may be registered under. TRACE and CONNECT are
// absent deliberately: neither is something an application implements, and
// TRACE is a reflection hazard worth not making easy.
const (
	GET     Method = "GET"
	HEAD    Method = "HEAD"
	POST    Method = "POST"
	PUT     Method = "PUT"
	PATCH   Method = "PATCH"
	DELETE  Method = "DELETE"
	OPTIONS Method = "OPTIONS"
)

// String makes a Method usable wherever a pattern or a log line wants one.
func (m Method) String() string { return string(m) }

// Valid reports whether m is one of the methods an endpoint may declare.
func (m Method) Valid() bool {
	switch m {
	case GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS:
		return true
	default:
		return false
	}
}

// Methods is every method an endpoint may declare, in the order they are
// listed above — for an error message, and for the file-name modifiers the
// generator accepts.
func Methods() []Method { return []Method{GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS} }
