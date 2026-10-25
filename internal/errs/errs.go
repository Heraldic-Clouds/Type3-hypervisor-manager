// Package errs defines the typed errors Type3 uses internally. The HTTP layer
// maps each Kind to a status code so every failure produces the same JSON
// error shape.
package errs

import (
	"errors"
	"fmt"
)

// Kind classifies an error for callers and for HTTP status mapping.
type Kind string

const (
	KindInvalid      Kind = "invalid"
	KindNotFound     Kind = "not_found"
	KindConflict     Kind = "conflict"
	KindUnauthorized Kind = "unauthorized"
	KindForbidden    Kind = "forbidden"
	KindUnavailable  Kind = "unavailable"
	KindTimeout      Kind = "timeout"
	KindPrecondition Kind = "precondition_failed"
	KindInternal     Kind = "internal"
)

// Error is a classified error with a stable machine-readable code.
type Error struct {
	Kind    Kind
	Code    string
	Message string
	Details map[string]any
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// New builds an error of the given kind.
func New(kind Kind, code, format string, args ...any) *Error {
	return &Error{Kind: kind, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap builds an error of the given kind that wraps cause.
func Wrap(cause error, kind Kind, code, format string, args ...any) *Error {
	return &Error{Kind: kind, Code: code, Message: fmt.Sprintf(format, args...), Err: cause}
}

// WithDetails returns e with extra structured details attached.
func (e *Error) WithDetails(kv map[string]any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	for k, v := range kv {
		e.Details[k] = v
	}
	return e
}

// Invalid is shorthand for a validation error.
func Invalid(code, format string, args ...any) *Error {
	return New(KindInvalid, code, format, args...)
}

// NotFound is shorthand for a missing resource.
func NotFound(code, format string, args ...any) *Error {
	return New(KindNotFound, code, format, args...)
}

// Conflict is shorthand for a state conflict.
func Conflict(code, format string, args ...any) *Error {
	return New(KindConflict, code, format, args...)
}

// Unavailable is shorthand for a dependency (usually an LXD host) being unreachable.
func Unavailable(code, format string, args ...any) *Error {
	return New(KindUnavailable, code, format, args...)
}

// As extracts an *Error from err.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// KindOf returns the kind of err, or KindInternal for unclassified errors.
func KindOf(err error) Kind {
	if e, ok := As(err); ok {
		return e.Kind
	}
	return KindInternal
}

// Is reports whether err is of the given kind.
func Is(err error, kind Kind) bool {
	return err != nil && KindOf(err) == kind
}
