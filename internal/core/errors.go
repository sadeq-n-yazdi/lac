// Package core holds LAC's domain vocabulary: the types every layer agrees on and the storage
// contracts the services depend on. It performs no I/O and knows nothing about SQL, JSON or
// sockets, so the business rules can be read and tested without a database or a transport.
package core

import "errors"

// The domain errors. Services return these; transports map them onto protocol error codes and
// storage implementations wrap them, so callers always compare with errors.Is.
var (
	// ErrNotFound means the named record does not exist.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyExists means a record with that identity is already present.
	ErrAlreadyExists = errors.New("already exists")
	// ErrInvalidArgument means the caller supplied a value the domain rejects.
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrUnauthorised means the caller is not who it claims to be, or may not do this.
	ErrUnauthorised = errors.New("unauthorised")
	// ErrCapacityReached means a resource has no free slot right now. It is a normal, expected
	// outcome of a non-blocking acquire, not a failure.
	ErrCapacityReached = errors.New("resource capacity reached")
	// ErrConflict means the record changed underneath the caller and the operation was refused.
	ErrConflict = errors.New("conflict")
)
