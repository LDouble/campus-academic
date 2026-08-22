// Package apperror defines stable application errors for transport layers.
package apperror

import "errors"

// Error is an error with a stable public code and HTTP status.
type Error struct {
	Code    string
	Message string
	Status  int
	Cause   error
}

// Error implements error.
func (e *Error) Error() string { return e.Message }

// Unwrap returns the internal cause.
func (e *Error) Unwrap() error { return e.Cause }

// New creates an application error.
func New(status int, code, message string) *Error {
	return &Error{Code: code, Message: message, Status: status}
}

// Wrap creates an application error retaining the internal cause.
func Wrap(status int, code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Status: status, Cause: cause}
}

// As returns an application error when one exists in the chain.
func As(err error) (*Error, bool) {
	var target *Error
	return target, errors.As(err, &target)
}
