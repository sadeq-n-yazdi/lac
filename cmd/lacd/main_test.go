package main

import (
	"bytes"
	"strings"
	"testing"

	"sadeq.uk/lac/internal/version"
)

// "lacd version" must print the version and stop. Before it was recognised, the word was ignored
// and a second daemon started next to the running one, which is the last thing someone checking a
// version wants.
func TestVersionSubcommandPrintsTheVersion(t *testing.T) {
	for _, arguments := range [][]string{{"version"}, {"-version"}, {"--version"}} {
		var output bytes.Buffer

		if err := run(arguments, &output); err != nil {
			t.Fatalf("run(%q) = %v", arguments, err)
		}
		if want := "lacd " + version.String() + "\n"; output.String() != want {
			t.Errorf("run(%q) printed %q, want %q", arguments, output.String(), want)
		}
	}
}

// Any other word is a mistake. Refusing it is the only safe answer: carrying on would start a
// daemon the caller never asked for.
func TestStrayArgumentIsRefused(t *testing.T) {
	for _, arguments := range [][]string{{"status"}, {"version", "extra"}} {
		var output bytes.Buffer

		err := run(arguments, &output)
		if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
			t.Errorf("run(%q) = %v, want an unexpected argument error", arguments, err)
		}
		if output.Len() != 0 {
			t.Errorf("run(%q) printed %q, want nothing", arguments, output.String())
		}
	}
}
