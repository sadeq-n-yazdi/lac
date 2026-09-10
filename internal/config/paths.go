package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// maxSocketPathLength is the practical limit for a Unix domain socket path. The kernel struct on
// macOS allows 104 bytes including the terminator, and Linux allows 108; we use the smaller so a
// configuration that works on one platform works on the other.
const maxSocketPathLength = 103

// DirectoryMode is the mode of every directory LAC creates. Only the owner may enter or list it.
const DirectoryMode os.FileMode = 0o700

// FileMode is the mode of every file LAC creates. Only the owner may read or write it.
const FileMode os.FileMode = 0o600

// Paths are the locations LAC reads and writes, resolved from the XDG base directory
// specification with the user's home directory as the fallback.
type Paths struct {
	// ConfigFile is the YAML configuration file.
	ConfigFile string
	// StateDir holds everything that must survive a reboot.
	StateDir string
	// RuntimeDir holds the socket, which need not survive a reboot.
	RuntimeDir string
}

// DefaultPaths resolves LAC's paths from the environment.
//
// On macOS XDG_RUNTIME_DIR is normally unset, so the socket falls back to a run directory inside
// the state directory rather than to a world-writable temporary directory.
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolving the home directory: %w", err)
	}

	configHome := environmentDir("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	stateHome := environmentDir("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	paths := Paths{
		ConfigFile: filepath.Join(configHome, applicationName, "config.yaml"),
		StateDir:   filepath.Join(stateHome, applicationName),
	}
	paths.RuntimeDir = environmentDir("XDG_RUNTIME_DIR", "")
	if paths.RuntimeDir == "" {
		paths.RuntimeDir = filepath.Join(paths.StateDir, "run")
	} else {
		paths.RuntimeDir = filepath.Join(paths.RuntimeDir, applicationName)
	}

	return paths, nil
}

// environmentDir returns the named environment variable when it holds an absolute path, and the
// fallback otherwise. A relative XDG value is undefined by the specification, so it is ignored.
func environmentDir(variable, fallback string) string {
	if value := os.Getenv(variable); filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return fallback
}

// EnsureDirectories creates the directories the configuration refers to, with owner-only
// permissions, and tightens any that already exist but are too permissive.
func EnsureDirectories(configuration Config) error {
	directories := []string{
		filepath.Dir(configuration.DatabasePath),
		filepath.Dir(configuration.SocketPath),
		filepath.Dir(configuration.SecretPath),
	}

	for _, directory := range directories {
		if err := os.MkdirAll(directory, DirectoryMode); err != nil {
			return fmt.Errorf("creating %s: %w", directory, err)
		}
		if err := os.Chmod(directory, DirectoryMode); err != nil {
			return fmt.Errorf("tightening permissions on %s: %w", directory, err)
		}
	}

	return nil
}

// checkPrivateFile reports whether a file that holds a secret is readable by anyone but its owner.
// A missing file is not an error: LAC creates it on first use with the right mode.
func checkPrivateFile(path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", path, err)
	}

	if permissions := info.Mode().Perm(); permissions&0o077 != 0 {
		return fmt.Errorf("%w: %s is mode %#o; it holds secrets and must not be readable by other "+
			"users. Run: chmod 600 %s", ErrInvalidConfig, path, permissions, path)
	}

	return nil
}
