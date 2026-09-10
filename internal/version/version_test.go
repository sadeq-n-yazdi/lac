package version

import (
	"runtime"
	"strings"
	"testing"
)

// A binary built without -ldflags must still report something meaningful, because that is what
// `go install` and `go run` produce and users paste the output into bug reports.
func TestStringIncludesBuildFacts(t *testing.T) {
	got := String()

	for _, want := range []string{Version, Commit, BuildDate, runtime.GOOS, runtime.GOARCH} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}
}
