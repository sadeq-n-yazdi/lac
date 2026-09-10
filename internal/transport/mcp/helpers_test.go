package mcp

import (
	"io"
	"log/slog"
)

// quietLogger keeps a passing test run readable: the server and the daemon both log at start-up.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
