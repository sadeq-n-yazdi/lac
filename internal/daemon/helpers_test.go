package daemon_test

import (
	"io"
	"log/slog"
)

// quietLogger keeps a passing test run readable; the daemon logs its start-up at info level.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
