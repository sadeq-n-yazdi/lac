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
	return strings.Contains(existing, "name: "+Name) &&
		strings.Contains(existing, "local agents coordinator")
}
