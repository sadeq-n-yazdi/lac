// Package version exposes the build metadata stamped into the binaries at link time.
package version

import (
	"fmt"
	"runtime"
)

// Values injected with -ldflags -X at build time. The defaults describe a plain `go build`.
var (
	// Version is the release tag, or "dev" for an untagged build.
	Version = "dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "unknown"
	// BuildDate is the RFC 3339 timestamp of the build.
	BuildDate = "unknown"
)

// String returns a one-line, human-readable description of this build.
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s/%s, %s)",
		Version, Commit, BuildDate, runtime.GOOS, runtime.GOARCH, runtime.Version())
}
