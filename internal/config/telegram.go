package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TelegramConfig is the optional bridge to a Telegram bot. It is off unless it is configured.
//
// This is the only part of LAC that reaches outside the machine, and it does so in one direction:
// the daemon calls Telegram, Telegram never calls in.
type TelegramConfig struct {
	// Enabled turns the bridge on. It stays off without a token and an allowlist whatever this says.
	Enabled bool `yaml:"enabled"`
	// Token is the bot token. Prefer TokenFile or the LAC_TELEGRAM_TOKEN environment variable:
	// a token written here lives in a file that is easy to copy by accident.
	Token string `yaml:"token"`
	// TokenFile holds the token on its own. It must not be readable by other users.
	TokenFile string `yaml:"token_file"`
	// AllowedChatIDs are the Telegram chats the bridge will talk to. Without at least one, anyone
	// who found the bot could drive this machine, so the bridge refuses to start.
	AllowedChatIDs []int64 `yaml:"allowed_chat_ids"`
	// PollTimeout is how long each long poll waits. Empty means thirty seconds.
	PollTimeout Duration `yaml:"poll_timeout"`
	// ReportDeadline is how long /report waits for the agents to answer. Empty means thirty
	// seconds. It is a wait on a phone, so shorter is usually better.
	ReportDeadline Duration `yaml:"report_deadline"`
}

// ResolveToken returns the bot token from the environment, the token file, or the configuration,
// in that order of preference.
//
// A token file other users can read is refused rather than used: a leaked bot token lets a stranger
// read everything the operator asks and tell the agents whatever they like.
func (t TelegramConfig) ResolveToken() (string, error) {
	if fromEnvironment := strings.TrimSpace(os.Getenv("LAC_TELEGRAM_TOKEN")); fromEnvironment != "" {
		return fromEnvironment, nil
	}

	if t.TokenFile != "" {
		if !filepath.IsAbs(t.TokenFile) {
			return "", fmt.Errorf("%w: telegram token_file %q must be an absolute path",
				ErrInvalidConfig, t.TokenFile)
		}
		if err := checkPrivateFile(t.TokenFile); err != nil {
			return "", err
		}

		contents, err := os.ReadFile(t.TokenFile)
		if err != nil {
			return "", fmt.Errorf("reading the telegram token from %s: %w", t.TokenFile, err)
		}

		return strings.TrimSpace(string(contents)), nil
	}

	return strings.TrimSpace(t.Token), nil
}

// validate reports whether the bridge can be started as configured.
func (t TelegramConfig) validate() error {
	if !t.Enabled {
		return nil
	}

	token, err := t.ResolveToken()
	if err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("%w: telegram is enabled but no token was given. Set token_file, "+
			"or LAC_TELEGRAM_TOKEN in the daemon's environment", ErrInvalidConfig)
	}

	if len(t.AllowedChatIDs) == 0 {
		return fmt.Errorf("%w: telegram is enabled but allowed_chat_ids is empty. Without it, "+
			"anyone who finds the bot could drive this machine. Message the bot and read the "+
			"chat id from the daemon's log", ErrInvalidConfig)
	}

	if t.PollTimeout < 0 {
		return fmt.Errorf("%w: telegram poll_timeout must not be negative", ErrInvalidConfig)
	}

	return nil
}
