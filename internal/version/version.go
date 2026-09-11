// Package version exposes the build metadata of the running binary.
//
// It comes from one of two places. `make build` stamps it in with -ldflags, which is how the
// released archives are built. A binary from `go install sadeq.uk/lac/cmd/lac@v0.1.0` has no such
// stamp, so the details are read from the build information the toolchain embeds instead — without
// that fallback, everybody who installed by module path would report "dev" and have no way to say
// which version they were running.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
)

// Values injected with -ldflags -X at build time. Left at these defaults, the build information
// embedded by the toolchain is used instead.
var (
	// Version is the release tag, or "dev" for an untagged build.
	Version = "dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "unknown"
	// BuildDate is the RFC 3339 timestamp of the build.
	BuildDate = "unknown"
)

// unknown values, named so the fallback can recognise what was never set.
const (
	unknownVersion = "dev"
	unknownValue   = "unknown"
	// shortSHALength is how much of a git revision is worth printing.
	shortSHALength = 7
)

// resolved caches the details, which never change while the process runs.
var resolved = sync.OnceValue(details)

// Build describes the running binary.
type Build struct {
	// Version is the release it was built from, such as "v0.1.0", or "dev".
	Version string
	// Commit is the short git revision, or "unknown".
	Commit string
	// BuildDate is when it was built, or "unknown".
	BuildDate string
	// Modified is true when the working tree had uncommitted changes at build time.
	Modified bool
}

// Current returns the details of the running binary.
func Current() Build { return resolved() }

// details works out what this binary is, preferring what was stamped in at link time and falling
// back to the information the toolchain embeds.
func details() Build {
	build := Build{Version: Version, Commit: Commit, BuildDate: BuildDate}

	info, available := debug.ReadBuildInfo()
	if !available {
		return build
	}

	if build.Version == unknownVersion && info.Main.Version != "" {
		// A module installed by version reports it here; a local build reports "(devel)", which is
		// no more useful than "dev".
		if info.Main.Version != "(devel)" {
			build.Version = info.Main.Version
		}
	}

	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if build.Commit == unknownValue && setting.Value != "" {
				build.Commit = shorten(setting.Value)
			}
		case "vcs.time":
			if build.BuildDate == unknownValue && setting.Value != "" {
				build.BuildDate = setting.Value
			}
		case "vcs.modified":
			build.Modified = setting.Value == "true"
		}
	}

	return build
}

func shorten(revision string) string {
	if len(revision) <= shortSHALength {
		return revision
	}

	return revision[:shortSHALength]
}

// String returns a one-line, human-readable description of this build, the kind of thing a person
// pastes into a bug report.
func String() string {
	build := Current()

	// The version may already say it: `git describe --dirty` appends "-dirty", and the toolchain's
	// own build info appends "+dirty". Saying it twice looks like a bug, because it was one.
	version := build.Version
	if build.Modified && !strings.Contains(version, "dirty") {
		version += "-dirty"
	}

	return fmt.Sprintf("%s (commit %s, built %s, %s/%s, %s)",
		version, build.Commit, build.BuildDate, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// Released reports whether this binary came from a tagged release, rather than somebody's checkout.
func Released() bool {
	build := Current()

	return build.Version != unknownVersion && build.Version != "" && !build.Modified
}
