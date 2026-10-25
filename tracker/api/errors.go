package api

import (
	"errors"
	"fmt"
)

// Kind is the class of an API failure. The five kinds are the wire contract:
// clients map them to behavior (retry, give up, treat as a lost race), so
// they are named in the design (section 5) and change only deliberately.
type Kind string

const (
	// KindNotFound: the named issue or resource does not exist.
	KindNotFound Kind = "not_found"
	// KindGuardFailed: an if_assignee or if_status guard did not hold; nothing was written.
	KindGuardFailed Kind = "guard_failed"
	// KindConflict: the request lost a race or collides with existing state (a duplicate id, an open child).
	KindConflict Kind = "conflict"
	// KindInvalid: the request is malformed or refused by a rule (a missing title, a bad id prefix).
	KindInvalid Kind = "invalid"
	// KindUnavailable: the service cannot answer now (database down, shutting down); retrying may help.
	KindUnavailable Kind = "unavailable"
)

// Error is an API failure: a kind and a message for a person. It is the JSON
// body of every non-2xx response.
type Error struct {
	Kind    Kind   `json:"kind"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return string(e.Kind) + ": " + e.Message }

// Is makes errors.Is(err, ErrNotFound) true for any not_found Error: the
// sentinels below carry no message and match on kind alone.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Message == "" && t.Kind == e.Kind
}

// HTTPStatus is the status the server answers an Error with: 500 for a kind
// it does not know, which is a server bug and not a client's to handle.
func (e *Error) HTTPStatus() int {
	switch e.Kind {
	case KindNotFound:
		return 404
	case KindGuardFailed, KindConflict:
		return 409
	case KindInvalid:
		return 400
	case KindUnavailable:
		return 503
	}
	return 500
}

// Sentinels for errors.Is.
var (
	ErrNotFound    = &Error{Kind: KindNotFound}
	ErrGuardFailed = &Error{Kind: KindGuardFailed}
	ErrConflict    = &Error{Kind: KindConflict}
	ErrInvalid     = &Error{Kind: KindInvalid}
	ErrUnavailable = &Error{Kind: KindUnavailable}
)

// NotFound is a not_found Error naming what was not found.
func NotFound(what string) *Error { return &Error{Kind: KindNotFound, Message: what + " not found"} }

// GuardFailed is a guard_failed Error; msg says which guard and what held.
func GuardFailed(msg string) *Error { return &Error{Kind: KindGuardFailed, Message: msg} }

// Conflict is a conflict Error.
func Conflict(format string, args ...any) *Error {
	return &Error{Kind: KindConflict, Message: fmt.Sprintf(format, args...)}
}

// Invalid is an invalid Error.
func Invalid(format string, args ...any) *Error {
	return &Error{Kind: KindInvalid, Message: fmt.Sprintf(format, args...)}
}

// Unavailable is an unavailable Error.
func Unavailable(format string, args ...any) *Error {
	return &Error{Kind: KindUnavailable, Message: fmt.Sprintf(format, args...)}
}

// Headers the protocol uses.
const (
	// SchemaHeader carries the caller's schema level on every /v1 request; the
	// server refuses a mismatch and a missing header.
	SchemaHeader = "X-Tracker-Schema"
	// RequestTagHeader optionally names the caller's unit of work (a tick id)
	// so the server can count calls per tag.
	RequestTagHeader = "X-Tracker-Tag"
)

// ErrUnknownToken is what token lookup returns for a token that is not known,
// was revoked, or has expired. The server answers 401 for it.
var ErrUnknownToken = errors.New("api: unknown or expired token")
