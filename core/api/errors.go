package api

import "errors"

// ---------------------------------------------------------------------------
// The wire contract, shared by both halves.
//
// Everything in this file compiles in the browser. That is the constraint that
// shapes it: no net/http, not even for its status constants. Importing that
// package for `http.StatusNotFound` would link its package-level state and its
// init, and with it the TLS stack — 2.05 MB gzipped for six integers.
// ---------------------------------------------------------------------------

// None is the type for an endpoint with no query, or no body, or no response.
type None struct{}

// Error is a deliberate failure: its message is meant for the caller.
//
// The same type on both sides of the wire. A handler returns it, the transport
// decodes it back out of the response, and the caller reads Code and Field
// instead of matching on a message string.
type Error struct {
	Code    int
	Message string
	// Field names the input that was wrong, for a form to highlight.
	Field string
}

func (e *Error) Error() string {
	if e.Field != "" {
		return e.Field + ": " + e.Message
	}
	return e.Message
}

// Status codes, spelled out rather than imported. See the note above.
const (
	statusBadRequest         = 400
	statusUnauthorized       = 401
	statusForbidden          = 403
	statusNotFound           = 404
	statusConflict           = 409
	statusInternalError      = 500
	statusServiceUnavailable = 503
)

func Invalid(field, message string) *Error {
	return &Error{Code: statusBadRequest, Message: message, Field: field}
}
func BadRequest(message string) *Error { return &Error{Code: statusBadRequest, Message: message} }
func Unauthorized(msg string) *Error   { return &Error{Code: statusUnauthorized, Message: msg} }
func Forbidden(message string) *Error  { return &Error{Code: statusForbidden, Message: message} }
func NotFound(message string) *Error   { return &Error{Code: statusNotFound, Message: message} }
func Conflict(message string) *Error   { return &Error{Code: statusConflict, Message: message} }
func Unavailable(message string) *Error {
	return &Error{Code: statusServiceUnavailable, Message: message}
}

// badRequest keeps a deliberate *Error as it is and promotes anything else to
// a 400.
func badRequest(err error) error {
	var deliberate *Error
	if errors.As(err, &deliberate) {
		return err
	}
	return &Error{Code: statusBadRequest, Message: err.Error()}
}

// errorBody is what a failed request returns. The correlation id is the bridge
// between what the caller saw and what the server logged: an unexpected error
// says nothing useful on the wire, and everything in the log line under that id.
type errorBody struct {
	Error         string `json:"error"`
	Field         string `json:"field,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// Status lets a response type choose its own status code — 201 for a create,
// 202 for something queued. Anything not implementing it is a 200, or a 204
// when the endpoint returns None.
type Status interface{ Status() int }

// Validator is the validation hook. Implement it on a query or body type and it
// runs after decoding, before the handler.
//
//	func (q PingQuery) Validate() error {
//	    if q.Limit > 100 { return api.Invalid("limit", "at most 100") }
//	    return nil
//	}
//
// It lives here rather than with the server half because the browser can run it
// too: the client can reject input before spending a round-trip on it, using
// the same rule the server will apply.
type Validator interface{ Validate() error }

func validate(v any) error {
	if val, ok := v.(Validator); ok {
		return val.Validate()
	}
	return nil
}
