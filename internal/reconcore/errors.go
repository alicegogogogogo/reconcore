package reconcore

import "fmt"

// Error is the single error type that leaves the service layer. The HTTP layer
// maps its Code and Status directly onto the documented error body.
type Error struct {
	Code    string
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// ValidationError reports a rejected request (HTTP 400).
func ValidationError(format string, arguments ...any) *Error {
	return &Error{Code: "validation_error", Status: 400, Message: fmt.Sprintf(format, arguments...)}
}

// NotFoundError reports a missing resource (HTTP 404).
func NotFoundError(format string, arguments ...any) *Error {
	return &Error{Code: "not_found", Status: 404, Message: fmt.Sprintf(format, arguments...)}
}

// ConflictError reports a state conflict (HTTP 409).
func ConflictError(format string, arguments ...any) *Error {
	return &Error{Code: "conflict", Status: 409, Message: fmt.Sprintf(format, arguments...)}
}

// InternalError reports a broken internal invariant (HTTP 500).
func InternalError(format string, arguments ...any) *Error {
	return &Error{Code: "internal_error", Status: 500, Message: fmt.Sprintf(format, arguments...)}
}
