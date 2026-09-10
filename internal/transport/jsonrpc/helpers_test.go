package jsonrpc_test

import (
	"io"
	"log/slog"
)

// quietLogger silences the deliberate failures a test provokes, so a passing run stays readable.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
