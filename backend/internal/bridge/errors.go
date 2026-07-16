package bridge

import (
	"errors"
	"fmt"

	"github.com/AokiAx/grok2api/backend/internal/compat"
)

// ErrorClass distinguishes request validation from conversion/gateway failures.
type ErrorClass int

const (
	// ClassInvalidRequest maps to HTTP 400.
	ClassInvalidRequest ErrorClass = iota
	// ClassBadGateway maps to HTTP 502 for conversion failures after upstream success.
	ClassBadGateway
)

// Error is a bridge-level failure with an HTTP-oriented class.
type Error struct {
	Class   ErrorClass
	Message string
	Cause   error
	// Param/Code mirror OpenAI error fields when known (from compat.RequestError).
	Param string
	Code  string
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// AsError extracts a bridge.Error.
func AsError(err error) (*Error, bool) {
	var bridgeErr *Error
	if errors.As(err, &bridgeErr) {
		return bridgeErr, true
	}
	return nil, false
}

func invalidRequest(message string, cause error) error {
	err := &Error{Class: ClassInvalidRequest, Message: message, Cause: cause}
	if re, ok := compat.AsRequestError(cause); ok {
		err.Param = re.Param
		err.Code = re.Code
		if re.Message != "" {
			// Prefer the specific validation message for clients.
			err.Message = re.Message
			if re.Param != "" {
				err.Message = re.Param + ": " + re.Message
			}
		}
	}
	return err
}

func badGateway(message string, cause error) error {
	return &Error{Class: ClassBadGateway, Message: message, Cause: cause}
}
