package config

import (
	"fmt"

	"code.sadeq.uk/lac/internal/core"
)

// CommandConfig is a command a worker may be asked to run.
//
// The whole point of naming commands here is that a requester cannot write one. An agent asks for
// "test"; what "test" means is the operator's decision, in the operator's file.
type CommandConfig struct {
	// Resource is the resource whose workers run this command, and whose capacity limits how many
	// run at once.
	Resource string `yaml:"resource"`
	// Run is the command and its arguments, already split. It is executed directly, with no shell,
	// so nothing in it is ever interpreted as a pipeline or a substitution.
	Run []string `yaml:"run"`
	// Description is shown to agents choosing what to ask for.
	Description string `yaml:"description"`
	// TimeLimit stops a command that will not finish. Empty means the resource's lease decides.
	TimeLimit Duration `yaml:"time_limit"`
}

// Commands returns the commands workers may run, keyed by the name an agent asks for.
func (c Config) Commands() map[string]CommandConfig { return c.WorkerCommands }

// Command returns one command by the key an agent asked for.
func (c Config) Command(key string) (CommandConfig, error) {
	command, found := c.WorkerCommands[key]
	if !found {
		return CommandConfig{}, fmt.Errorf("%w: no command called %q is configured on this machine",
			core.ErrNotFound, key)
	}

	return command, nil
}

// validateCommands reports whether the configured commands can be run.
func (c Config) validateCommands() error {
	for key, command := range c.WorkerCommands {
		if err := core.ValidateName("command", key); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
		if len(command.Run) == 0 {
			return fmt.Errorf("%w: the command %q has nothing to run", ErrInvalidConfig, key)
		}
		if command.Run[0] == "" {
			return fmt.Errorf("%w: the command %q has an empty program name", ErrInvalidConfig, key)
		}
		if err := core.ValidateName("command resource", command.Resource); err != nil {
			return fmt.Errorf("%w: command %q: %w", ErrInvalidConfig, key, err)
		}
		if command.TimeLimit < 0 {
			return fmt.Errorf("%w: the command %q has a negative time limit", ErrInvalidConfig, key)
		}
	}

	return nil
}
