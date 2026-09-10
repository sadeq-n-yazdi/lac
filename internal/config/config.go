// Package config resolves where LAC keeps its files and how the daemon behaves, from defaults, an
// optional YAML file, the environment and explicit overrides — in that order of increasing
// precedence. It refuses to return a configuration the daemon cannot run safely.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"code.sadeq.uk/lac/internal/core"
)

// applicationName is the directory name LAC uses under each XDG base directory.
const applicationName = "lac"

// ErrInvalidConfig means the configuration cannot be used as it stands. It always carries an
// explanation of which setting is wrong and what to do about it.
var ErrInvalidConfig = errors.New("invalid configuration")

// Default durations. They are deliberately short enough that a crashed agent frees its slot while
// its operator is still looking at the screen, and long enough not to punish a slow test run.
const (
	// DefaultAgentTimeToLive is how long an agent may stay silent before it is treated as gone.
	DefaultAgentTimeToLive = 90 * time.Second
	// DefaultLeaseTimeToLive is the fallback expiry for a resource that does not set its own.
	DefaultLeaseTimeToLive = 15 * time.Minute
	// DefaultShutdownGrace is how long in-flight calls have to finish when the daemon stops.
	DefaultShutdownGrace = 5 * time.Second
)

// Config is the daemon's complete, validated configuration.
type Config struct {
	// SocketPath is the Unix domain socket the daemon listens on.
	SocketPath string `yaml:"socket_path"`
	// DatabasePath is the SQLite database file.
	DatabasePath string `yaml:"database_path"`
	// SecretPath is the per-install key used to hash agent tokens.
	SecretPath string `yaml:"secret_path"`
	// AllowedWorkdirRoots limits the working directories an agent may claim. An agent registering
	// a directory outside every root is refused, which stops a stray registration from advertising
	// somewhere it has no business being. Empty means the user's home directory.
	AllowedWorkdirRoots []string `yaml:"allowed_workdir_roots"`
	// AgentTimeToLive is how long an agent may go without a heartbeat before it is marked stale
	// and everything it holds is released.
	AgentTimeToLive Duration `yaml:"agent_time_to_live"`
	// DefaultLeaseTimeToLive is used for resources that do not specify their own.
	DefaultLeaseTimeToLive Duration `yaml:"default_lease_time_to_live"`
	// ShutdownGrace is how long in-flight calls have to finish on shutdown.
	ShutdownGrace Duration `yaml:"shutdown_grace"`
	// LogLevel is one of debug, info, warn or error.
	LogLevel string `yaml:"log_level"`
	// Resources are defined at startup, so a fresh install already knows about the machine's
	// limits without anyone having to run a command.
	Resources []ResourceConfig `yaml:"resources"`
	// Capabilities is what an ordinary agent is allowed to do. An agent never chooses its own
	// capabilities: it says who it is, and the daemon decides what that means.
	Capabilities CapabilitiesConfig `yaml:"capabilities"`
	// Operators are the agent names that additionally may define resources and ask everyone for a
	// report. These are the operator's own tools, not the AI sessions.
	Operators []string `yaml:"operators"`
}

// CapabilitiesConfig is the policy applied to every agent that is not an operator.
type CapabilitiesConfig struct {
	// Resources are the resource names an agent may queue for. The single entry "*" means all of
	// them, which is the sensible default on a machine with one user and a handful of agents.
	Resources []string `yaml:"resources"`
	// CanBroadcast allows sending to a topic or to every agent at once.
	CanBroadcast bool `yaml:"can_broadcast"`
	// CanDefineResources allows creating and reconfiguring resources. Off for ordinary agents: the
	// machine's limits are the operator's decision, not an agent's.
	CanDefineResources bool `yaml:"can_define_resources"`
	// CanRequestReports allows asking every other agent to report. Off for ordinary agents.
	CanRequestReports bool `yaml:"can_request_reports"`
}

// CapabilitiesFor returns what an agent registering under this name is allowed to do.
//
// Capabilities come from this policy alone. An agent that could name its own would be able to
// grant itself the run of the machine simply by asking.
func (c Config) CapabilitiesFor(agentName string) core.Capabilities {
	capabilities := core.Capabilities{
		Resources:          c.Capabilities.Resources,
		CanBroadcast:       c.Capabilities.CanBroadcast,
		CanDefineResources: c.Capabilities.CanDefineResources,
		CanRequestReports:  c.Capabilities.CanRequestReports,
	}

	if slices.Contains(c.Operators, agentName) {
		capabilities.Resources = []string{core.WildcardResource}
		capabilities.CanBroadcast = true
		capabilities.CanDefineResources = true
		capabilities.CanRequestReports = true
	}

	return capabilities
}

// ResourceConfig is a resource declared in the configuration file.
type ResourceConfig struct {
	// Name is what agents queue for.
	Name string `yaml:"name"`
	// Capacity is how many agents may hold a slot at once.
	Capacity int `yaml:"capacity"`
	// LeaseTimeToLive overrides the daemon default for this resource. Zero uses the default.
	LeaseTimeToLive Duration `yaml:"lease_time_to_live"`
	// Description is shown to operators and agents.
	Description string `yaml:"description"`
}

// Options are the overrides a caller supplies, normally from command-line flags. Empty fields are
// left to the file, the environment or the defaults.
type Options struct {
	// ConfigPath is the configuration file to read. Empty means the default location, and a
	// missing file there is not an error.
	ConfigPath string
	// SocketPath overrides Config.SocketPath.
	SocketPath string
	// DatabasePath overrides Config.DatabasePath.
	DatabasePath string
	// LogLevel overrides Config.LogLevel.
	LogLevel string
}

// Load resolves the configuration from defaults, the file, the environment and opts.
//
// A configuration file named explicitly through opts.ConfigPath or LAC_CONFIG must exist; one
// found at the default location is optional, so LAC runs out of the box.
func Load(opts Options) (Config, error) {
	paths, err := DefaultPaths()
	if err != nil {
		return Config{}, err
	}

	configuration := defaults(paths)

	configPath, explicit := configFilePath(opts, paths)
	if err := applyFile(&configuration, configPath, explicit); err != nil {
		return Config{}, err
	}
	applyEnvironment(&configuration)
	applyOptions(&configuration, opts)

	if err := configuration.Validate(); err != nil {
		return Config{}, err
	}

	return configuration, nil
}

// defaults returns the configuration LAC uses when nothing else says otherwise.
func defaults(paths Paths) Config {
	return Config{
		SocketPath:             filepath.Join(paths.RuntimeDir, "lacd.sock"),
		DatabasePath:           filepath.Join(paths.StateDir, "lac.db"),
		SecretPath:             filepath.Join(paths.StateDir, "secret.key"),
		AgentTimeToLive:        Duration(DefaultAgentTimeToLive),
		DefaultLeaseTimeToLive: Duration(DefaultLeaseTimeToLive),
		ShutdownGrace:          Duration(DefaultShutdownGrace),
		LogLevel:               "info",
		Capabilities: CapabilitiesConfig{
			Resources:    []string{core.WildcardResource},
			CanBroadcast: true,
		},
		Operators: []string{"operator"},
	}
}

// configFilePath returns the file to read and whether the caller named it explicitly.
func configFilePath(opts Options, paths Paths) (path string, explicit bool) {
	if opts.ConfigPath != "" {
		return opts.ConfigPath, true
	}
	if fromEnvironment := os.Getenv("LAC_CONFIG"); fromEnvironment != "" {
		return fromEnvironment, true
	}
	return paths.ConfigFile, false
}

// applyEnvironment overlays the LAC_* environment variables.
func applyEnvironment(configuration *Config) {
	overlayString(&configuration.SocketPath, os.Getenv("LAC_SOCKET"))
	overlayString(&configuration.DatabasePath, os.Getenv("LAC_DATABASE"))
	overlayString(&configuration.SecretPath, os.Getenv("LAC_SECRET"))
	overlayString(&configuration.LogLevel, os.Getenv("LAC_LOG_LEVEL"))
}

// applyOptions overlays the caller's explicit overrides, which win over everything else.
func applyOptions(configuration *Config, opts Options) {
	overlayString(&configuration.SocketPath, opts.SocketPath)
	overlayString(&configuration.DatabasePath, opts.DatabasePath)
	overlayString(&configuration.LogLevel, opts.LogLevel)
}

func overlayString(target *string, value string) {
	if value != "" {
		*target = value
	}
}

// Validate reports whether the configuration can be used, explaining anything that cannot.
func (c Config) Validate() error {
	for _, field := range []struct{ name, path string }{
		{"socket_path", c.SocketPath},
		{"database_path", c.DatabasePath},
		{"secret_path", c.SecretPath},
	} {
		if !filepath.IsAbs(field.path) {
			return fmt.Errorf("%w: %s must be an absolute path, got %q", ErrInvalidConfig, field.name, field.path)
		}
	}

	if length := len(c.SocketPath); length > maxSocketPathLength {
		return fmt.Errorf("%w: socket_path is %d bytes; the operating system allows at most %d. "+
			"Set socket_path or XDG_RUNTIME_DIR to something shorter",
			ErrInvalidConfig, length, maxSocketPathLength)
	}

	for _, root := range c.AllowedWorkdirRoots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("%w: allowed_workdir_roots entry %q must be an absolute path", ErrInvalidConfig, root)
		}
	}

	for _, field := range []struct {
		name  string
		value Duration
	}{
		{"agent_time_to_live", c.AgentTimeToLive},
		{"default_lease_time_to_live", c.DefaultLeaseTimeToLive},
		{"shutdown_grace", c.ShutdownGrace},
	} {
		if field.value <= 0 {
			return fmt.Errorf("%w: %s must be positive, got %s", ErrInvalidConfig, field.name, field.value)
		}
	}

	for _, name := range c.Operators {
		if err := core.ValidateName("operator name", name); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
	}

	for _, resource := range c.Capabilities.Resources {
		if resource == core.WildcardResource {
			continue
		}
		if err := core.ValidateName("capabilities resource", resource); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("%w: log_level must be debug, info, warn or error, got %q", ErrInvalidConfig, c.LogLevel)
	}

	return c.validateResources()
}

func (c Config) validateResources() error {
	seen := make(map[string]struct{}, len(c.Resources))

	for _, declared := range c.Resources {
		resource := declared.Resource(time.Duration(c.DefaultLeaseTimeToLive))
		if err := resource.Validate(); err != nil {
			return fmt.Errorf("%w: resource %q: %w", ErrInvalidConfig, declared.Name, err)
		}
		if _, duplicate := seen[resource.Name]; duplicate {
			return fmt.Errorf("%w: resource %q is declared twice", ErrInvalidConfig, resource.Name)
		}
		seen[resource.Name] = struct{}{}
	}

	return nil
}

// Resource converts a declared resource into its domain form, filling in the daemon default when
// the declaration does not set its own time to live.
func (r ResourceConfig) Resource(defaultTimeToLive time.Duration) core.Resource {
	timeToLive := time.Duration(r.LeaseTimeToLive)
	if timeToLive == 0 {
		timeToLive = defaultTimeToLive
	}

	return core.Resource{
		Name:            r.Name,
		Capacity:        r.Capacity,
		LeaseTimeToLive: timeToLive,
		Description:     r.Description,
	}
}

// WorkdirRoots returns the directories an agent may register a working directory under, falling
// back to the user's home directory when the configuration names none.
func (c Config) WorkdirRoots() ([]string, error) {
	if len(c.AllowedWorkdirRoots) > 0 {
		return c.AllowedWorkdirRoots, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving the home directory: %w", err)
	}

	return []string{home}, nil
}

// Duration is a time.Duration that reads from YAML as a string such as "15m", because a bare
// number in a configuration file is ambiguous about its unit.
type Duration time.Duration

// String renders the duration the same way time.Duration does.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML parses a duration string, or a plain number of seconds for convenience.
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var text string
	if err := unmarshal(&text); err == nil {
		parsed, err := time.ParseDuration(text)
		if err != nil {
			return fmt.Errorf("%w: %q is not a duration such as \"90s\" or \"15m\"", ErrInvalidConfig, text)
		}
		*d = Duration(parsed)
		return nil
	}

	var seconds int64
	if err := unmarshal(&seconds); err != nil {
		return fmt.Errorf("%w: expected a duration such as \"15m\"", ErrInvalidConfig)
	}
	*d = Duration(time.Duration(seconds) * time.Second)

	return nil
}

// MarshalYAML renders the duration as a string, so a written configuration reads back unchanged.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// ParseDuration is a small helper for callers holding a duration as text, such as a flag value.
func ParseDuration(text string) (Duration, error) {
	if seconds, err := strconv.ParseInt(text, 10, 64); err == nil {
		return Duration(time.Duration(seconds) * time.Second), nil
	}

	parsed, err := time.ParseDuration(text)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a duration such as \"90s\" or \"15m\"", ErrInvalidConfig, text)
	}

	return Duration(parsed), nil
}
