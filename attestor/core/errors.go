package core

import (
	"errors"
	"fmt"
)

// ErrorKind is the stable domain classification consumed by the shared gRPC
// adapter. Plugins may retain native errors for logs and metrics by wrapping
// them in Error.
type ErrorKind uint8

const (
	ErrorInvalidArgument ErrorKind = iota + 1
	ErrorNotFound
	ErrorFailedPrecondition
	ErrorUnavailable
	ErrorInternal
	// ErrorUnimplemented: this daemon does not serve that capability for this
	// route -- no signer, no block verifier, no commitment feed. Distinct from
	// ErrorFailedPrecondition on purpose: a precondition clears on its own when
	// the replica catches up, and this one never does until an operator edits
	// the config. Collapsing the two makes the consumer retry a configuration
	// mistake forever. See 04-Attestor §"grpc sở hữu mapping thống nhất".
	ErrorUnimplemented
)

// Error carries a stable class without erasing the underlying error.
type Error struct {
	Kind ErrorKind
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err == nil {
		return e.Msg
	}
	if e.Msg == "" {
		return e.Err.Error()
	}
	return fmt.Sprintf("%s: %v", e.Msg, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// NewError creates a classified error. A nil cause is valid for pure input or
// route errors.
//
// The message is what a gRPC caller sees; the cause is not (see mapError in
// attestor/grpc). Put what was being done in the message and the upstream error
// in the cause. A message that quotes an upstream error would hand its text,
// and any API key in the endpoint URL it quotes, to every peer that can reach
// the unauthenticated API.
func NewError(kind ErrorKind, message string, cause error) error {
	return &Error{Kind: kind, Msg: message, Err: cause}
}

// ErrorKindOf returns the stable class of err, defaulting to Internal so a
// new plugin cannot accidentally expose an unclassified upstream error as a
// successful or retryable response.
func ErrorKindOf(err error) ErrorKind {
	var classified *Error
	if errors.As(err, &classified) && classified != nil {
		return classified.Kind
	}
	return ErrorInternal
}
