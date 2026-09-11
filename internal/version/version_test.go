package version

import (
	"runtime"
	"strings"
	"sync"
	"testing"
)

// A binary built without -ldflags must still report something meaningful, because that is what
// `go install` and `go run` produce and users paste the output into bug reports.
func TestStringIncludesBuildFacts(t *testing.T) {
	got := String()
	build := Current()

	for _, want := range []string{build.Version, build.Commit, build.BuildDate, runtime.GOOS, runtime.GOARCH} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}
}

// The details are read once and cached, so two calls must agree: a version that changed halfway
// through a session would be worse than useless.
func TestCurrentIsStable(t *testing.T) {
	first, second := Current(), Current()

	if first != second {
		t.Errorf("Current() returned %+v then %+v", first, second)
	}
}

// Nothing may be blank. A field that renders as "" in a bug report tells nobody anything, and the
// fallback exists precisely so that never happens.
func TestNoFieldIsEmpty(t *testing.T) {
	build := Current()

	if build.Version == "" {
		t.Error("Version is empty")
	}
	if build.Commit == "" {
		t.Error("Commit is empty")
	}
	if build.BuildDate == "" {
		t.Error("BuildDate is empty")
	}
}

// Under `go test` the binary is built from the local tree, so it is honestly not a release.
func TestReleasedIsFalseForALocalBuild(t *testing.T) {
	if Released() {
		t.Errorf("Released() = true for a test binary: %+v", Current())
	}
}

// A working tree with uncommitted changes must say so, or a build from somebody's half-finished
// edit reports the same version as the release it was branched from.
func TestStringMarksAModifiedTree(t *testing.T) {
	if !Current().Modified {
		t.Skip("this tree is clean, so there is nothing to mark")
	}

	if !strings.Contains(String(), "-dirty") {
		t.Errorf("String() = %q, want it to mark the modified tree", String())
	}
}

// A version that already says it is dirty must not be told again. `git describe --dirty` appends
// "-dirty" and the toolchain's build info appends "+dirty", so both arrive pre-marked.
func TestDirtyIsNotSaidTwice(t *testing.T) {
	original := Version
	t.Cleanup(func() {
		Version = original
		resolved = sync.OnceValue(details)
	})

	tests := map[string]string{
		"v0.1.0":       "v0.1.0-dirty",
		"v0.1.0-dirty": "v0.1.0-dirty",
		"v0.1.0+dirty": "v0.1.0+dirty",
	}

	for stamped, want := range tests {
		t.Run(stamped, func(t *testing.T) {
			build := Build{Version: stamped, Commit: "c81a40b", BuildDate: "unknown", Modified: true}
			resolved = func() Build { return build }

			got := String()

			if !strings.HasPrefix(got, want+" ") {
				t.Errorf("String() = %q, want it to start with %q", got, want)
			}
			if strings.Count(got, "dirty") != 1 {
				t.Errorf("String() = %q, want \"dirty\" exactly once", got)
			}
		})
	}
}

func TestShorten(t *testing.T) {
	tests := map[string]string{
		"c81a40bc951aa365f09bcf2f4990c8a7ba7d48fc": "c81a40b",
		"c81a40b": "c81a40b",
		"short":   "short",
		"":        "",
	}

	for revision, want := range tests {
		if got := shorten(revision); got != want {
			t.Errorf("shorten(%q) = %q, want %q", revision, got, want)
		}
	}
}

// The stamped values win over the embedded ones: that is how the released archives report their
// tag rather than whatever the toolchain happened to record.
func TestStampedValuesWin(t *testing.T) {
	build := details()
	if build.Version != Version && Version != unknownVersion {
		t.Errorf("Version = %q, want the stamped %q", build.Version, Version)
	}

	original := Version
	Version = "v9.9.9"
	t.Cleanup(func() { Version = original })

	if stamped := details(); stamped.Version != "v9.9.9" {
		t.Errorf("Version = %q, want the stamped value to be preferred", stamped.Version)
	}
}
