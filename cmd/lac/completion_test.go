package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeLookup stands in for the daemon, so completion is tested without one running.
type fakeLookup struct{}

func (fakeLookup) lookup(_ context.Context, source valueSource) []candidate {
	switch source {
	case sourceResources:
		return []candidate{{value: "test"}, {value: "reviewer"}}
	case sourceAgents:
		return []candidate{{value: "sadeq"}, {value: "claude-server-1"}}
	case sourceLeases:
		return []candidate{{value: "lease_1", description: "test held by claude-server-1"}}
	}

	return nil
}

func valuesOf(candidates []candidate) []string {
	values := make([]string, 0, len(candidates))
	for _, offered := range candidates {
		values = append(values, offered.value)
	}

	return values
}

// A command added without a completion entry would silently offer nothing after its name.
func TestEveryCommandHasACompletionSpec(t *testing.T) {
	for _, available := range commands() {
		if available.hidden {
			continue
		}
		if _, known := completionSpecs[available.name]; !known {
			t.Errorf("command %q has no entry in completionSpecs", available.name)
		}
	}
}

func TestComplete(t *testing.T) {
	cases := []struct {
		name  string
		words []string
		want  []string
	}{
		{"a prefix narrows the commands", []string{"rel"}, []string{"reload", "release"}},
		{"the operator finds the lease to force-release", []string{"release", "--force", ""}, []string{"lease_1"}},
		{"command flags", []string{"release", "--f"}, []string{"--force"}},
		{"a flag's value comes from the daemon", []string{"run", "--resource", "te"}, []string{"test"}},
		{"global flags are skipped to find the command", []string{"--json", "queue", ""}, []string{"test", "reviewer"}},
		{"a global flag's value", []string{"--name", "sa"}, []string{"sadeq"}},
		{"global flags", []string{"--js"}, []string{"--json"}},
		{"the second positional of send", []string{"send", "sadeq", "q"}, []string{"question"}},
		{"fixed choices", []string{"completion", ""}, []string{"bash", "zsh", "fish"}},
		{"nothing past the last positional", []string{"pr", "watch", ""}, nil},
		{"nothing after -- in run", []string{"run", "--resource", "test", "--", "ma"}, nil},
		{"an unknown command offers nothing", []string{"nonsense", ""}, nil},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := valuesOf(complete(t.Context(), fakeLookup{}, testCase.words))
			if !slices.Equal(got, testCase.want) && (len(got) != 0 || len(testCase.want) != 0) {
				t.Errorf("complete(%q) = %q, want %q", testCase.words, got, testCase.want)
			}
		})
	}
}

// The hidden helper is for the shell scripts; offering it on Tab would only confuse a person.
func TestHiddenCommandsAreNotOffered(t *testing.T) {
	got := valuesOf(complete(t.Context(), fakeLookup{}, []string{""}))
	if slices.Contains(got, completeCommandName) {
		t.Errorf("complete() offered the hidden %q command", completeCommandName)
	}
	if !slices.Contains(got, "completion") {
		t.Errorf("complete() = %q, want it to offer completion", got)
	}
}

// Every script must call back into lac, or the completion it installs offers nothing.
func TestScriptsCallTheHelper(t *testing.T) {
	for shell, script := range completionScripts {
		if !strings.Contains(script, "lac "+completeCommandName) {
			t.Errorf("the %s script does not call lac %s", shell, completeCommandName)
		}
	}
	if !strings.HasPrefix(zshCompletionScript, "#compdef lac lacd\n") {
		t.Error("the zsh script must start with #compdef, or autoloading it from fpath does nothing")
	}
}

// The files must land where each shell looks without editing its startup files.
func TestInstallWritesWhereTheShellLooks(t *testing.T) {
	dataHome, configHome := t.TempDir(), t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	want := map[string][]string{
		"bash": {
			filepath.Join(dataHome, "bash-completion", "completions", "lac"),
			filepath.Join(dataHome, "bash-completion", "completions", "lacd"),
		},
		"zsh": {filepath.Join(dataHome, "zsh", "site-functions", "_lac")},
		"fish": {
			filepath.Join(configHome, "fish", "completions", "lac.fish"),
			filepath.Join(configHome, "fish", "completions", "lacd.fish"),
		},
	}

	for shell, paths := range want {
		installed, err := installCompletion(shell, completionScripts[shell])
		if err != nil {
			t.Fatalf("installCompletion(%s) = %v, want nil", shell, err)
		}
		if !slices.Equal(installed, paths) {
			t.Errorf("installCompletion(%s) wrote %q, want %q", shell, installed, paths)
		}
		for _, path := range paths {
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			if string(content) != completionScripts[shell] {
				t.Errorf("%s does not hold the %s script", path, shell)
			}
		}
	}
}
