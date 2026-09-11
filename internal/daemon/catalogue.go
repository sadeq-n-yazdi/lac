package daemon

import (
	"sort"
	"time"

	"sadeq.uk/lac/internal/config"
	"sadeq.uk/lac/internal/service/dispatch"
)

// catalogue exposes the operator's configured commands to the dispatch service.
//
// It reads the configuration and nothing else, which is the point: a requester names a key, and
// only the operator's file decides what that key runs. It reads it live, so a reloaded command
// list takes effect without a restart.
type catalogue struct {
	daemon *Daemon
}

// compile-time proof that the contract is satisfied.
var _ dispatch.Catalogue = catalogue{}

// Command returns one command by the key an agent asked for.
func (c catalogue) Command(key string) (dispatch.Command, error) {
	declared, err := c.daemon.settings().Command(key)
	if err != nil {
		return dispatch.Command{}, err
	}

	return commandOf(key, declared), nil
}

// Commands returns every configured command, by key.
func (c catalogue) Commands() []dispatch.Command {
	declared := c.daemon.settings().Commands()

	keys := make([]string, 0, len(declared))
	for key := range declared {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	commands := make([]dispatch.Command, 0, len(keys))
	for _, key := range keys {
		commands = append(commands, commandOf(key, declared[key]))
	}

	return commands
}

func commandOf(key string, declared config.CommandConfig) dispatch.Command {
	return dispatch.Command{
		Key:         key,
		Resource:    declared.Resource,
		Run:         declared.Run,
		Description: declared.Description,
		TimeLimit:   time.Duration(declared.TimeLimit),
	}
}
