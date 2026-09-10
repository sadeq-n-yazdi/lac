package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code.sadeq.uk/lac/internal/core"
)

// A derived name must be one the daemon will accept, whatever the directory is called — otherwise
// `lac run` fails at registration in exactly the projects with awkward names.
func TestDerivedNamesAreValid(t *testing.T) {
	directories := []string{
		"ai-agent-manager",
		"My Project",
		"project.v2",
		"UPPERCASE",
		"weird!@#$%chars",
		"...",
		"",
	}

	for _, directory := range directories {
		t.Run(directory, func(t *testing.T) {
			subject := identity{workdir: filepath.Join(string(filepath.Separator), "tmp", "root", directory)}

			name, err := subject.resolveName()
			if err != nil {
				t.Fatalf("resolveName() = %v, want nil", err)
			}
			if err := core.ValidateName("agent name", name); err != nil {
				t.Errorf("the derived name %q is not one the daemon accepts: %v", name, err)
			}
		})
	}
}

// Two shells in the same project must not collide, or the second one is refused the name.
func TestDerivedNamesIncludeTheProcess(t *testing.T) {
	subject := identity{workdir: "/tmp/root/project"}

	name, err := subject.resolveName()
	if err != nil {
		t.Fatalf("resolveName() = %v, want nil", err)
	}
	if !strings.HasPrefix(name, "project-") {
		t.Errorf("name = %q, want it to start with the directory", name)
	}
	if name == "project-" {
		t.Error("the name carries no process id, so two shells would collide")
	}
}

func TestExplicitNameWins(t *testing.T) {
	subject := identity{name: "claude-a", workdir: "/tmp/root/project"}

	name, err := subject.resolveName()
	if err != nil {
		t.Fatalf("resolveName() = %v, want nil", err)
	}
	if name != "claude-a" {
		t.Errorf("name = %q, want the one that was given", name)
	}
}

// A message body may be JSON or plain text; both must reach the daemon as JSON, because a shell
// user should not have to quote braces to say something simple.
func TestBodyOf(t *testing.T) {
	tests := map[string]string{
		`{"ask":"are you on the parser?"}`: `{"ask":"are you on the parser?"}`,
		`are you on the parser?`:           `{"text":"are you on the parser?"}`,
		`123`:                              `{"text":"123"}`,
		``:                                 `{"text":""}`,
	}

	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			encoded, err := json.Marshal(bodyOf(input))
			if err != nil {
				t.Fatalf("encoding the body: %v", err)
			}
			if string(encoded) != want {
				t.Errorf("bodyOf(%q) encoded to %s, want %s", input, encoded, want)
			}
		})
	}
}

// A saved token is a credential. If another user can read it, they can act as this agent.
func TestSavedTokensArePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.token")
	t.Setenv("LAC_TOKEN_FILE", path)

	written, err := saveToken("lac_example")
	if err != nil {
		t.Fatalf("saveToken() = %v, want nil", err)
	}
	if written != path {
		t.Errorf("saveToken() wrote to %q, want %q", written, path)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		t.Errorf("the token file is mode %#o, want 0600", permissions)
	}

	token, err := readSavedToken()
	if err != nil {
		t.Fatalf("readSavedToken() = %v, want nil", err)
	}
	if token != "lac_example" {
		t.Errorf("readSavedToken() = %q, want the saved token", token)
	}
}

func TestALooseTokenFileIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.token")
	t.Setenv("LAC_TOKEN_FILE", path)

	if _, err := saveToken("lac_example"); err != nil {
		t.Fatalf("saveToken() = %v, want nil", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("relaxing the permissions: %v", err)
	}

	_, err := readSavedToken()
	if err == nil {
		t.Fatal("a world-readable token file was accepted")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

// A missing token file simply means "not registered yet", which is the normal first run.
func TestAMissingTokenFileIsNotAnError(t *testing.T) {
	t.Setenv("LAC_TOKEN_FILE", filepath.Join(t.TempDir(), "absent.token"))

	token, err := readSavedToken()
	if err != nil {
		t.Fatalf("readSavedToken() = %v, want nil", err)
	}
	if token != "" {
		t.Errorf("readSavedToken() = %q, want an empty token", token)
	}
}
