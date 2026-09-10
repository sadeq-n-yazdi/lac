package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// currentDirectory is where this session is working, unless it was told otherwise.
func currentDirectory() (string, error) {
	workdir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolving the working directory: %w", err)
	}

	return workdir, nil
}

// deriveName invents a name for a session that was not given one: the tool, the directory it is
// working in, and the process id, so two Claude sessions in the same project are still distinct.
func deriveName(workdir, kind string) string {
	project := sanitiseSegment(filepath.Base(workdir))
	if project == "" {
		project = "session"
	}

	return fmt.Sprintf("%s-%s-%d", sanitiseSegment(kind), project, os.Getpid())
}

// sanitiseKind reduces a client's self-reported name — "Claude Code", "claude-ai/mcp" — to a label
// the daemon will accept as an agent kind.
func sanitiseKind(clientName string) string {
	kind := sanitiseSegment(clientName)
	if kind == "" {
		return "mcp"
	}

	// "claude-code" and "claude-ai" are the same tool as far as an operator reading the roster is
	// concerned, and a short kind keeps `lac agents` readable.
	if index := strings.IndexAny(kind, "-."); index > 0 {
		kind = kind[:index]
	}

	return kind
}

// sanitiseSegment keeps only what an agent name may contain.
func sanitiseSegment(value string) string {
	var builder strings.Builder

	for _, character := range strings.ToLower(value) {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			builder.WriteRune(character)
		case character == '-', character == '_', character == '.':
			builder.WriteRune(character)
		default:
			builder.WriteRune('-')
		}
	}

	return strings.Trim(builder.String(), "-._")
}
