// Package failure gives an error the kind that decides the exit code of a
// command (docs/DESIGN.md, "Agent interface").
package failure

import (
	"errors"
	"fmt"
)

type Kind string

const (
	Failed            Kind = "failed"
	Usage             Kind = "usage"
	NotFound          Kind = "not_found"
	Conflict          Kind = "conflict"
	NeedsConfirmation Kind = "needs_confirmation"
	NoCapacity        Kind = "no_capacity"
	Timeout           Kind = "timeout"
	ChecksFailed      Kind = "checks_failed"
	Unsupported       Kind = "unsupported"
	SecretUnavailable Kind = "secret_unavailable"
)

// Error is an error with a kind. The message of a wrapped error follows the
// message of Error, as fmt.Errorf with %w does it.
type Error struct {
	Kind    Kind
	Message string
	Err     error
}

func (e *Error) Error() string {
	switch {
	case e.Err == nil:
		return e.Message
	case e.Message == "":
		return e.Err.Error()
	}
	return e.Message + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

func New(kind Kind, format string, args ...any) error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// Wrap gives err a kind and a context message. It returns nil for a nil err.
func Wrap(kind Kind, err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...), Err: err}
}

// KindOf is the kind of the outermost Error in the chain of err, Failed when
// the chain has none, and empty for a nil err.
func KindOf(err error) Kind {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return Failed
}

func Is(err error, kind Kind) bool { return err != nil && KindOf(err) == kind }
