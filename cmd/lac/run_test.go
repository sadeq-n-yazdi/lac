package main

import (
	"flag"
	"testing"
)

// A coverage run is queued behind ordinary test runs, so an agent waiting on a plain test result is
// not held up by a slower report nobody is sitting in front of. Missing a coverage command lets it
// jump the queue; flagging a plain test run pushes real work to the back.
func TestCoverageRunsAreRecognised(t *testing.T) {
	cases := []struct {
		name        string
		commandLine []string
		coverage    bool
	}{
		{"go test with a profile", []string{"go", "test", "-coverprofile=coverage.out", "./..."}, true},
		{"go test with -cover", []string{"go", "test", "-cover", "./..."}, true},
		{"make cover", []string{"make", "cover"}, true},
		{"make test-coverage", []string{"make", "test-coverage"}, true},
		{"pytest-cov", []string{"uv", "run", "pytest", "--cov=app", "--cov-report=term"}, true},
		{"coverage.py", []string{"coverage", "run", "-m", "pytest"}, true},
		{"make test", []string{"make", "test"}, false},
		{"go test", []string{"go", "test", "-race", "./..."}, false},
		{"pytest", []string{"uv", "run", "pytest", "tests/"}, false},
		{"a word that only contains cover", []string{"./scripts/recover.sh"}, false},
	}

	for _, each := range cases {
		t.Run(each.name, func(t *testing.T) {
			if got := isCoverageRun(each.commandLine); got != each.coverage {
				t.Errorf("isCoverageRun(%q) = %v, want %v", each.commandLine, got, each.coverage)
			}
		})
	}
}

// The background priority is only a default: a caller who passes --priority, even 0, gets what
// they asked for.
func TestExplicitPriorityIsDetected(t *testing.T) {
	for _, each := range []struct {
		arguments []string
		set       bool
	}{
		{[]string{"--resource", "test", "--", "make", "cover"}, false},
		{[]string{"--resource", "test", "--priority", "0", "--", "make", "cover"}, true},
	} {
		flags := flag.NewFlagSet("run", flag.ContinueOnError)
		flags.String("resource", "", "")
		flags.Int("priority", 0, "")

		if err := parseAnywhere(flags, each.arguments); err != nil {
			t.Fatal(err)
		}
		if got := flagWasSet(flags, "priority"); got != each.set {
			t.Errorf("flagWasSet(%q) = %v, want %v", each.arguments, got, each.set)
		}
	}
}
