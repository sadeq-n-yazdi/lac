package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sadeq.uk/lac/internal/config"
)

// isolate points the XDG variables at a temporary home so tests never read or write the
// developer's real configuration.
//
// The home is created under the system temporary directory with a short name rather than through
// t.TempDir(), whose paths on macOS are long enough on their own to breach the socket path limit.
func isolate(t *testing.T) (home string) {
	t.Helper()

	home = shortTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_RUNTIME_DIR", "")
	for _, variable := range []string{"LAC_CONFIG", "LAC_SOCKET", "LAC_DATABASE", "LAC_SECRET", "LAC_LOG_LEVEL"} {
		t.Setenv(variable, "")
	}

	return home
}

// shortTempDir returns a temporary directory with a short path, and removes it afterwards.
func shortTempDir(t *testing.T) string {
	t.Helper()

	directory, err := os.MkdirTemp("", "lac")
	if err != nil {
		t.Fatalf("creating a temporary directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Errorf("removing %s: %v", directory, err)
		}
	})

	return directory
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(shortTempDir(t), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing the test configuration: %v", err)
	}

	return path
}

// LAC must run with no configuration at all, and land its files where the XDG specification says.
func TestLoadDefaults(t *testing.T) {
	home := isolate(t)

	configuration, err := config.Load(config.Options{})
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	wantDatabase := filepath.Join(home, "state", "lac", "lac.db")
	if configuration.DatabasePath != wantDatabase {
		t.Errorf("DatabasePath = %q, want %q", configuration.DatabasePath, wantDatabase)
	}

	wantSocket := filepath.Join(home, "state", "lac", "run", "lacd.sock")
	if configuration.SocketPath != wantSocket {
		t.Errorf("SocketPath = %q, want %q", configuration.SocketPath, wantSocket)
	}

	if configuration.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", configuration.LogLevel, "info")
	}
	if time.Duration(configuration.AgentTimeToLive) != config.DefaultAgentTimeToLive {
		t.Errorf("AgentTimeToLive = %s, want %s", configuration.AgentTimeToLive, config.DefaultAgentTimeToLive)
	}
}

// XDG_RUNTIME_DIR is normally set on Linux and absent on macOS; the socket must follow it when it
// is there, because that is the directory the system cleans up on logout.
func TestLoadUsesRuntimeDirectoryWhenSet(t *testing.T) {
	isolate(t)
	runtimeDir := shortTempDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	configuration, err := config.Load(config.Options{})
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	want := filepath.Join(runtimeDir, "lac", "lacd.sock")
	if configuration.SocketPath != want {
		t.Errorf("SocketPath = %q, want %q", configuration.SocketPath, want)
	}
}

// A relative XDG value is undefined by the specification. Honouring it would put the database
// somewhere that depends on the daemon's working directory, which is never what anyone means.
func TestLoadIgnoresRelativeXDGValues(t *testing.T) {
	home := isolate(t)
	t.Setenv("XDG_STATE_HOME", "state-relative")

	configuration, err := config.Load(config.Options{})
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	want := filepath.Join(home, ".local", "state", "lac", "lac.db")
	if configuration.DatabasePath != want {
		t.Errorf("DatabasePath = %q, want the home fallback %q", configuration.DatabasePath, want)
	}
}

// Later sources must win over earlier ones, or an operator's flag would be quietly ignored.
func TestLoadPrecedence(t *testing.T) {
	isolate(t)
	socketFromFile := filepath.Join(shortTempDir(t), "file.sock")
	path := writeConfig(t, "socket_path: "+socketFromFile+"\nlog_level: warn\n")

	t.Run("file over defaults", func(t *testing.T) {
		configuration, err := config.Load(config.Options{ConfigPath: path})
		if err != nil {
			t.Fatalf("Load() = %v, want nil", err)
		}
		if configuration.SocketPath != socketFromFile {
			t.Errorf("SocketPath = %q, want the file value %q", configuration.SocketPath, socketFromFile)
		}
		if configuration.LogLevel != "warn" {
			t.Errorf("LogLevel = %q, want the file value %q", configuration.LogLevel, "warn")
		}
	})

	t.Run("environment over file", func(t *testing.T) {
		socketFromEnvironment := filepath.Join(shortTempDir(t), "env.sock")
		t.Setenv("LAC_SOCKET", socketFromEnvironment)

		configuration, err := config.Load(config.Options{ConfigPath: path})
		if err != nil {
			t.Fatalf("Load() = %v, want nil", err)
		}
		if configuration.SocketPath != socketFromEnvironment {
			t.Errorf("SocketPath = %q, want the environment value %q", configuration.SocketPath, socketFromEnvironment)
		}
	})

	t.Run("options over environment", func(t *testing.T) {
		t.Setenv("LAC_SOCKET", filepath.Join(shortTempDir(t), "env.sock"))
		socketFromFlag := filepath.Join(shortTempDir(t), "flag.sock")

		configuration, err := config.Load(config.Options{ConfigPath: path, SocketPath: socketFromFlag})
		if err != nil {
			t.Fatalf("Load() = %v, want nil", err)
		}
		if configuration.SocketPath != socketFromFlag {
			t.Errorf("SocketPath = %q, want the flag value %q", configuration.SocketPath, socketFromFlag)
		}
	})
}

// A file the operator named must exist. Silently falling back to the defaults would run the daemon
// with settings nobody asked for.
func TestLoadNamedConfigFileMustExist(t *testing.T) {
	isolate(t)

	_, err := config.Load(config.Options{ConfigPath: filepath.Join(shortTempDir(t), "absent.yaml")})
	if err == nil {
		t.Fatal("Load() with a missing named file = nil, want an error")
	}
}

// A misspelled key that quietly does nothing is how security settings get lost.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	isolate(t)
	path := writeConfig(t, "sockit_path: /tmp/typo.sock\n")

	_, err := config.Load(config.Options{ConfigPath: path})
	if !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("Load() = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), "sockit_path") {
		t.Errorf("the error does not name the offending key: %v", err)
	}
}

// The configuration file can hold the Telegram bot token, so another user being able to read it is
// a credential leak, not a style problem.
func TestLoadRejectsWorldReadableConfigFile(t *testing.T) {
	isolate(t)
	path := writeConfig(t, "log_level: debug\n")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("relaxing the test file permissions: %v", err)
	}

	_, err := config.Load(config.Options{ConfigPath: path})
	if !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("Load() = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("the error does not tell the operator how to fix it: %v", err)
	}
}

func TestValidate(t *testing.T) {
	isolate(t)
	valid, err := config.Load(config.Options{})
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	tests := map[string]func(config.Config) config.Config{
		"relative socket": func(c config.Config) config.Config {
			c.SocketPath = "lacd.sock"
			return c
		},
		"relative database": func(c config.Config) config.Config {
			c.DatabasePath = "lac.db"
			return c
		},
		"overlong socket": func(c config.Config) config.Config {
			c.SocketPath = "/" + strings.Repeat("a", 200)
			return c
		},
		"relative workdir root": func(c config.Config) config.Config {
			c.AllowedWorkdirRoots = []string{"code"}
			return c
		},
		"zero agent time to live": func(c config.Config) config.Config {
			c.AgentTimeToLive = 0
			return c
		},
		"negative shutdown grace": func(c config.Config) config.Config {
			c.ShutdownGrace = config.Duration(-time.Second)
			return c
		},
		"unknown log level": func(c config.Config) config.Config {
			c.LogLevel = "chatty"
			return c
		},
		"resource without capacity": func(c config.Config) config.Config {
			c.Resources = []config.ResourceConfig{{Name: "test"}}
			return c
		},
		"duplicate resource": func(c config.Config) config.Config {
			c.Resources = []config.ResourceConfig{{Name: "test", Capacity: 4}, {Name: "test", Capacity: 2}}
			return c
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			if err := mutate(valid).Validate(); !errors.Is(err, config.ErrInvalidConfig) {
				t.Errorf("Validate() = %v, want ErrInvalidConfig", err)
			}
		})
	}

	t.Run("the loaded configuration is valid", func(t *testing.T) {
		if err := valid.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})
}

// The socket path limit is a kernel constraint, so the daemon must reject an overlong one with an
// explanation rather than fail deep inside the listener with "invalid argument".
func TestValidateExplainsTheSocketPathLimit(t *testing.T) {
	isolate(t)
	configuration, err := config.Load(config.Options{})
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	configuration.SocketPath = "/" + strings.Repeat("a", 200)

	err = configuration.Validate()
	if !strings.Contains(err.Error(), "XDG_RUNTIME_DIR") {
		t.Errorf("the error does not suggest a fix: %v", err)
	}
}

// A resource declared in the file must reach the daemon exactly as written, and inherit the
// default expiry when it does not set one.
func TestResourceConfigConversion(t *testing.T) {
	isolate(t)
	path := writeConfig(t, `
resources:
  - name: test
    capacity: 4
    description: concurrent test runs
  - name: reviewer
    capacity: 1
    lease_time_to_live: 30m
`)

	configuration, err := config.Load(config.Options{ConfigPath: path})
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if len(configuration.Resources) != 2 {
		t.Fatalf("got %d resources, want 2", len(configuration.Resources))
	}

	defaultTimeToLive := time.Duration(configuration.DefaultLeaseTimeToLive)

	test := configuration.Resources[0].Resource(defaultTimeToLive)
	if test.Capacity != 4 || test.Description != "concurrent test runs" {
		t.Errorf("test resource = %+v, want capacity 4 with its description", test)
	}
	if test.LeaseTimeToLive != defaultTimeToLive {
		t.Errorf("test lease time to live = %s, want the default %s", test.LeaseTimeToLive, defaultTimeToLive)
	}

	reviewer := configuration.Resources[1].Resource(defaultTimeToLive)
	if reviewer.LeaseTimeToLive != 30*time.Minute {
		t.Errorf("reviewer lease time to live = %s, want 30m", reviewer.LeaseTimeToLive)
	}
}

func TestDurationParsing(t *testing.T) {
	tests := []struct {
		text string
		want time.Duration
		bad  bool
	}{
		{text: "15m", want: 15 * time.Minute},
		{text: "90s", want: 90 * time.Second},
		{text: "1h30m", want: 90 * time.Minute},
		{text: "45", want: 45 * time.Second},
		{text: "soon", bad: true},
		{text: "", bad: true},
	}

	for _, test := range tests {
		t.Run(test.text, func(t *testing.T) {
			got, err := config.ParseDuration(test.text)

			if test.bad {
				if !errors.Is(err, config.ErrInvalidConfig) {
					t.Fatalf("ParseDuration(%q) error = %v, want ErrInvalidConfig", test.text, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDuration(%q) = %v, want nil", test.text, err)
			}
			if time.Duration(got) != test.want {
				t.Errorf("ParseDuration(%q) = %s, want %s", test.text, got, test.want)
			}
		})
	}
}

// The daemon creates its own directories, and they must not be readable by other users: the
// database holds every message the agents exchanged.
func TestEnsureDirectories(t *testing.T) {
	isolate(t)
	configuration, err := config.Load(config.Options{})
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	if err := config.EnsureDirectories(configuration); err != nil {
		t.Fatalf("EnsureDirectories() = %v, want nil", err)
	}

	for _, directory := range []string{
		filepath.Dir(configuration.DatabasePath),
		filepath.Dir(configuration.SocketPath),
	} {
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatalf("stat %s: %v", directory, err)
		}
		if permissions := info.Mode().Perm(); permissions != config.DirectoryMode {
			t.Errorf("%s is mode %#o, want %#o", directory, permissions, config.DirectoryMode)
		}
	}
}

// An existing directory left too open by an earlier version, or by a careless operator, must be
// tightened rather than accepted.
func TestEnsureDirectoriesTightensExistingPermissions(t *testing.T) {
	isolate(t)
	configuration, err := config.Load(config.Options{})
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	stateDir := filepath.Dir(configuration.DatabasePath)
	if err = os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("creating the loose directory: %v", err)
	}

	if err = config.EnsureDirectories(configuration); err != nil {
		t.Fatalf("EnsureDirectories() = %v, want nil", err)
	}

	info, err := os.Stat(stateDir)
	if err != nil {
		t.Fatalf("stat %s: %v", stateDir, err)
	}
	if permissions := info.Mode().Perm(); permissions != config.DirectoryMode {
		t.Errorf("%s is mode %#o, want %#o", stateDir, permissions, config.DirectoryMode)
	}
}

func TestWorkdirRootsFallsBackToHome(t *testing.T) {
	home := isolate(t)
	configuration, err := config.Load(config.Options{})
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	roots, err := configuration.WorkdirRoots()
	if err != nil {
		t.Fatalf("WorkdirRoots() = %v, want nil", err)
	}
	if len(roots) != 1 || roots[0] != home {
		t.Errorf("WorkdirRoots() = %v, want [%s]", roots, home)
	}
}
