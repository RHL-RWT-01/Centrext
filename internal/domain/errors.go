package domain

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound        = errors.New("not found")
	ErrInvalid         = errors.New("invalid input")
	ErrForbidden       = errors.New("forbidden")
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrStaleVersion    = errors.New("stale base_version")
	ErrConflict        = errors.New("conflict")
	ErrSecretDetected  = errors.New("content looks like it contains a secret")
)

// Invalidf wraps ErrInvalid with a message.
func Invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Forbiddenf wraps ErrForbidden with a message.
func Forbiddenf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrForbidden, fmt.Sprintf(format, args...))
}
