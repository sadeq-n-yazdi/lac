// Package jsonrpc implements the JSON-RPC 2.0 server LAC speaks over its Unix socket.
//
// The same framing MCP uses, so the MCP transport can be a thin adapter over the same handlers
// rather than a parallel implementation. Requests on one connection are handled concurrently —
// a blocking call such as waiting for a resource slot must not stop the caller from doing anything
// else — and every response is written through one serialised writer.
package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"code.sadeq.uk/lac/internal/core"
)

// Version is the only protocol version this server speaks.
const Version = "2.0"

// The standard JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// LAC's own error codes, from the range JSON-RPC reserves for implementations. They exist so a
// client can react to a failure without parsing English.
const (
	// CodeUnauthorised means the caller is not who it claims to be, or may not do this.
	CodeUnauthorised = -32000
	// CodeNotFound means the named thing does not exist.
	CodeNotFound = -32001
	// CodeAlreadyExists means something with that identity is already there.
	CodeAlreadyExists = -32002
	// CodeConflict means the state changed underneath the caller.
	CodeConflict = -32003
	// CodeCapacityReached means the resource has no free slot right now. It is an ordinary answer
	// to a non-blocking acquire, not a malfunction.
	CodeCapacityReached = -32004
	// CodeShuttingDown means the daemon stopped accepting calls. The work was not started, so the
	// caller can safely retry once the daemon is back.
	CodeShuttingDown = -32005
)

// Request is an incoming call. A request with no id is a notification: it is handled, but nothing
// is ever sent back, not even an error.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether the caller expects no reply.
func (r Request) IsNotification() bool { return len(r.ID) == 0 || string(r.ID) == "null" }

// Response is a reply to a request. Exactly one of Result and Error is set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Notification is a message the server sends without being asked, such as a lease being granted or
// a message arriving for the agent on the other end.
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Error makes Error an error, so a handler can simply return one when it wants a specific code.
func (e *Error) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

// NewError builds an error object.
func NewError(code int, message string) *Error { return &Error{Code: code, Message: message} }

// Errorf builds an error object with a formatted message.
func Errorf(code int, format string, arguments ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, arguments...)}
}

// errorFrom converts a handler's error into a wire error.
//
// Domain errors are mapped onto their codes so clients can react programmatically, and the
// message is passed through because it was written for a human to read. Anything unrecognised
// becomes an internal error with its detail preserved: LAC is a local tool, and the operator is
// the only audience, so hiding the cause would help nobody.
func errorFrom(err error) *Error {
	var wireError *Error
	if errors.As(err, &wireError) {
		return wireError
	}

	switch {
	case errors.Is(err, core.ErrUnauthorised):
		return NewError(CodeUnauthorised, err.Error())
	case errors.Is(err, core.ErrNotFound):
		return NewError(CodeNotFound, err.Error())
	case errors.Is(err, core.ErrAlreadyExists):
		return NewError(CodeAlreadyExists, err.Error())
	case errors.Is(err, core.ErrConflict):
		return NewError(CodeConflict, err.Error())
	case errors.Is(err, core.ErrCapacityReached):
		return NewError(CodeCapacityReached, err.Error())
	case errors.Is(err, core.ErrInvalidArgument):
		return NewError(CodeInvalidParams, err.Error())
	default:
		return NewError(CodeInternalError, err.Error())
	}
}

// ParseParams decodes a request's parameters into target, rejecting unknown fields so a
// misspelled parameter is reported rather than silently ignored.
func ParseParams(params json.RawMessage, target any) error {
	if len(params) == 0 {
		return nil
	}

	decoder := json.NewDecoder(bytes.NewReader(params))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		return Errorf(CodeInvalidParams, "invalid parameters: %v", err)
	}

	return nil
}
