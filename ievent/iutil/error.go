package iutil

import "errors"

// Error is a sentinel-based error type that supports context enrichment.
type Error struct {
	sentinel error
	by       string
	wrapped  error
}

// NewError creates a new sentinel error.
func NewError(text string) *Error {
	return &Error{sentinel: errors.New(text)}
}

// WithBy names who raised the error, keeping the sentinel identity.
func (e *Error) WithBy(by string) *Error {
	return &Error{
		sentinel: e.sentinel,
		by:       by,
		wrapped:  e.wrapped,
	}
}

// By is who raised the error, empty where nobody said.
func (e *Error) By() string {
	return e.by
}

// Error implements the error interface.
func (e *Error) Error() string {
	text := e.sentinel.Error()

	if e.by != "" {
		text += ": " + e.by
	}

	if e.wrapped != nil {
		text += ": " + e.wrapped.Error()
	}
	return text
}

// Unwrap returns the wrapped error for errors.Unwrap() support.
func (e *Error) Unwrap() error {
	return e.wrapped
}

// Wrap wraps another error, preserving the sentinel identity and who raised it.
func (e *Error) Wrap(wrapped error) *Error {
	return &Error{sentinel: e.sentinel, by: e.by, wrapped: wrapped}
}

// Is implements errors.Is() support by comparing sentinel pointers.
func (e *Error) Is(target error) bool {
	if other, ok := target.(*Error); ok {
		return other.sentinel == e.sentinel
	}
	return false
}

// As implements errors.As() support by copying to target if it's *Error.
func (e *Error) As(target any) bool {
	if t, ok := target.(**Error); ok {
		*t = e
		return true
	}
	return false
}

// The two ways an event stops acting; both keep walking so observers still see them.
var (
	ErrFailed  = NewError("Failed")
	ErrDropped = NewError("Dropped")
)
