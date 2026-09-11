// Package skill carries the LAC skill inside the binary and installs it where an AI tool will
// find it.
//
// The file is embedded rather than read from a checkout, so `go install sadeq.uk/lac/cmd/lac`
// gives an operator everything they need without cloning anything.
package skill

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed SKILL.md
var content string

// Name is the directory the skill is installed under.
const Name = "lac"

// OwnershipMarker is the line that says a skill file is lac's own copy and may be replaced by a
// later `lac skill install`. It is deliberately one short line: anything longer risks being wrapped
// by an editor or a formatter, and a marker that can be split is a marker that stops working.
const OwnershipMarker = "<!-- Installed by `lac skill install`. Edit the skill in lac, not this copy. -->"

// directoryMode and fileMode match what the surrounding skill directories normally use: readable
// by the tools that look for them, writable only by their owner.
const (
	directoryMode os.FileMode = 0o755
	fileMode      os.FileMode = 0o644
)

// Content returns the skill as it will be written.
func Content() string { return content }

// DefaultDirectory is where Claude Code and tools that follow it look for skills.
func DefaultDirectory() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving the home directory: %w", err)
	}

	return filepath.Join(home, ".claude", "skills"), nil
}

// Result describes what an install did.
type Result struct {
	// Path is the file that was written or would have been.
	Path string
	// Updated is true when a previous copy was replaced.
	Updated bool
	// Unchanged is true when the installed copy already matched.
	Unchanged bool
}

// Install writes the skill into directory/lac/SKILL.md.
//
// It refuses to overwrite a file it did not write, so an operator who has customised their own
// skill of the same name does not silently lose it. Installing the same version twice is a no-op.
func Install(directory string) (Result, error) {
	if directory == "" {
		resolved, err := DefaultDirectory()
		if err != nil {
			return Result{}, err
		}
		directory = resolved
	}

	target := filepath.Join(directory, Name, "SKILL.md")

	existing, err := os.ReadFile(target) //nolint:gosec // the operator's own skill directory
	switch {
	case err == nil:
		if string(existing) == content {
			return Result{Path: target, Unchanged: true}, nil
		}
		if !isOurSkill(string(existing)) {
			return Result{}, fmt.Errorf("%s already exists and was not written by lac; "+
				"move it aside first if you want the bundled skill", target)
		}

	case !errors.Is(err, os.ErrNotExist):
		return Result{}, fmt.Errorf("reading %s: %w", target, err)
	}

	if err := os.MkdirAll(filepath.Dir(target), directoryMode); err != nil {
		return Result{}, fmt.Errorf("creating %s: %w", filepath.Dir(target), err)
	}
	if err := os.WriteFile(target, []byte(content), fileMode); err != nil {
		return Result{}, fmt.Errorf("writing %s: %w", target, err)
	}

	return Result{Path: target, Updated: existing != nil}, nil
}

// isOurSkill reports whether a file looks like a copy of this skill rather than somebody's own
// work that happens to share the name.
func isOurSkill(existing string) bool {
	if !strings.Contains(existing, "name: "+Name) {
		return false
	}

	// The marker is a single short line precisely so that it cannot be broken by the wrapping of
	// the prose around it. An earlier version looked for a phrase from the description, which wraps
	// across two lines and so never matched — leaving lac unable to update its own copy.
	if strings.Contains(existing, OwnershipMarker) {
		return true
	}

	// Copies installed before the marker existed are still ours to replace. A tool name is a safe
	// way to recognise them: it is one word, so it cannot wrap, and nobody writing their own skill
	// of this name would document lac's tools without meaning this skill.
	return strings.Contains(existing, "lac_acquire_slot")
}
