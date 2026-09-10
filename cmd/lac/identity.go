package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"code.sadeq.uk/lac/pkg/lacclient"
)

// credentialFileMode keeps a saved token readable only by its owner.
const credentialFileMode os.FileMode = 0o600

// identity is how the CLI decides who it is when it talks to the daemon.
type identity struct {
	// token, when set, authenticates an agent that already exists.
	token string
	// name is used when registering. Empty derives one from the working directory.
	name string
	// kind labels the tool behind this agent.
	kind string
	// workdir is the directory being worked in.
	workdir string
}

// connect returns a client authenticated as this identity, registering if there is no token.
//
// The order — flag, environment, saved file, then register — is what lets an agent drop `lac run`
// in front of a command with no setup at all, while an operator who wants a stable identity can
// have one.
func (i identity) connect(ctx context.Context, socketPath string) (*lacclient.Client, error) {
	token := i.token
	if token == "" {
		token = os.Getenv("LAC_TOKEN")
	}
	if token == "" {
		saved, err := readSavedToken()
		if err != nil {
			return nil, err
		}
		token = saved
	}

	client, err := lacclient.Dial(ctx, lacclient.Options{SocketPath: socketPath, Token: token})
	if err != nil {
		return nil, err
	}
	if token != "" {
		return client, nil
	}

	name, err := i.resolveName()
	if err != nil {
		return nil, err
	}

	workdir := i.workdir
	if workdir == "" {
		workdir, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolving the working directory: %w", err)
		}
	}

	if _, err := client.Register(ctx, name, i.kind, workdir, os.Getpid()); err != nil {
		return nil, fmt.Errorf("registering as %q: %w", name, err)
	}

	return client, nil
}

// resolveName derives an agent name when none was given: the directory being worked in, plus this
// process's id, so two shells in the same project do not collide.
func (i identity) resolveName() (string, error) {
	if i.name != "" {
		return i.name, nil
	}

	workdir := i.workdir
	if workdir == "" {
		var err error
		if workdir, err = os.Getwd(); err != nil {
			return "", fmt.Errorf("resolving the working directory: %w", err)
		}
	}

	base := sanitiseName(filepath.Base(workdir))
	if base == "" {
		base = "agent"
	}

	return fmt.Sprintf("%s-%d", base, os.Getpid()), nil
}

// sanitiseName reduces a directory name to something an agent name may contain.
func sanitiseName(value string) string {
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

// credentialPath is where `lac register --save` keeps a token.
func credentialPath() (string, error) {
	if fromEnvironment := os.Getenv("LAC_TOKEN_FILE"); fromEnvironment != "" {
		return fromEnvironment, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving the home directory: %w", err)
	}

	stateHome := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(stateHome) {
		stateHome = filepath.Join(home, ".local", "state")
	}

	return filepath.Join(stateHome, "lac", "agent.token"), nil
}

// readSavedToken returns a token saved earlier, refusing one other users can read.
func readSavedToken() (string, error) {
	path, err := credentialPath()
	if err != nil {
		return "", err
	}

	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspecting %s: %w", path, err)
	}
	if permissions := info.Mode().Perm(); permissions&0o077 != 0 {
		return "", fmt.Errorf("%s is mode %#o; it holds an agent token and must not be readable by "+
			"other users. Run: chmod 600 %s", path, permissions, path)
	}

	contents, err := os.ReadFile(path) //nolint:gosec // the user's own state directory
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}

	return strings.TrimSpace(string(contents)), nil
}

// saveToken writes a token for later runs, with owner-only permissions.
func saveToken(token string) (string, error) {
	path, err := credentialPath()
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), credentialFileMode); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Chmod(path, credentialFileMode); err != nil {
		return "", fmt.Errorf("tightening permissions on %s: %w", path, err)
	}

	return path, nil
}
