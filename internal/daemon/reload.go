package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"sadeq.uk/lac/internal/config"
	"sadeq.uk/lac/internal/core"
)

// stableChecks is how many consecutive unchanged readings mean the file has settled.
//
// An editor writing a file often produces several changes in quick succession — a truncate, a
// write, a rename — and half a configuration is worse than none. Waiting for three quiet readings
// at the restart delay means a change is applied about three delays after the last edit.
const stableChecks = 3

// Reloadable reports whether a setting can be applied to a running daemon. The socket, the
// database and the secret are fixed at start-up: changing where the daemon listens or what it
// remembers is a restart, not a reload.
type reloadOutcome struct {
	// Applied lists what changed.
	Applied []string
	// Deferred lists settings that changed in the file but need a restart to take effect.
	Deferred []string
}

// Reload re-reads the configuration file and applies what can be applied to a running daemon.
//
// What cannot — the socket path, the database, the secret — is reported rather than silently
// ignored, because an operator who edited a setting and saw nothing happen would reasonably assume
// it had taken effect.
func (d *Daemon) Reload(ctx context.Context) error {
	updated, err := config.Load(d.source)
	if err != nil {
		// A broken configuration must not take the daemon down: it is still coordinating agents
		// with the configuration it already has.
		return fmt.Errorf("the configuration was not reloaded: %w", err)
	}

	outcome := d.apply(ctx, updated)

	for _, setting := range outcome.Deferred {
		d.logger.Warn("this setting changed but needs a restart to take effect", "setting", setting)
	}

	if len(outcome.Applied) == 0 {
		d.logger.Info("configuration reloaded; nothing that can change at run time had changed")
		return nil
	}

	d.logger.Info("configuration reloaded", "applied", outcome.Applied)

	return nil
}

// apply installs the parts of a new configuration that a running daemon can honour.
func (d *Daemon) apply(ctx context.Context, updated config.Config) reloadOutcome {
	var outcome reloadOutcome

	d.configurationMutex.Lock()
	previous := d.configuration
	d.configuration = updated
	d.configurationMutex.Unlock()

	// Fixed at start-up: report the change rather than pretending to honour it.
	for _, fixed := range []struct{ name, before, after string }{
		{"socket_path", previous.SocketPath, updated.SocketPath},
		{"database_path", previous.DatabasePath, updated.DatabasePath},
		{"secret_path", previous.SecretPath, updated.SecretPath},
	} {
		if fixed.before != fixed.after {
			outcome.Deferred = append(outcome.Deferred, fixed.name)
		}
	}
	if !sameTelegram(previous.Telegram, updated.Telegram) {
		outcome.Deferred = append(outcome.Deferred, "telegram")
	}

	// Resources are the point of reloading: this is how an operator changes what the machine will
	// do at once without stopping the agents already working.
	if applied := d.applyResources(ctx, previous, updated); applied != "" {
		outcome.Applied = append(outcome.Applied, applied)
	}

	// The registry reads its policy through this function on every registration, so a changed
	// capability or operator list takes effect for the next agent that registers.
	if !sameStrings(previous.Operators, updated.Operators) ||
		!sameCapabilities(previous.Capabilities, updated.Capabilities) {
		outcome.Applied = append(outcome.Applied, "capabilities")
	}

	// The dispatch catalogue reads the configuration on every lookup, so commands are live too.
	if !sameCommands(previous.WorkerCommands, updated.WorkerCommands) {
		outcome.Applied = append(outcome.Applied, "commands")
	}

	if previous.LogLevel != updated.LogLevel {
		outcome.Applied = append(outcome.Applied, "log_level")
		d.logger.Warn("log_level changed; it takes effect at the next restart", "level", updated.LogLevel)
	}

	return outcome
}

// applyResources defines what the new configuration declares. A resource that was removed from the
// file is left alone: agents may be holding slots on it, and taking those away underneath them is
// worse than an unused definition lingering.
func (d *Daemon) applyResources(ctx context.Context, previous, updated config.Config) string {
	defaultTimeToLive := time.Duration(updated.DefaultLeaseTimeToLive)

	changed := 0
	for _, declared := range updated.Resources {
		resource := declared.Resource(defaultTimeToLive)

		if existing, found := findResource(previous.Resources, declared.Name); found {
			if existing.Resource(time.Duration(previous.DefaultLeaseTimeToLive)) == resource {
				continue
			}
		}

		if err := d.leasing.Define(ctx, core.SystemActor, resource); err != nil {
			d.logger.Error("could not apply a resource from the reloaded configuration",
				"resource", resource.Name, "error", err)

			continue
		}

		d.logger.Info("resource applied from the reloaded configuration",
			"resource", resource.Name, "capacity", resource.Capacity)
		changed++
	}

	for _, declared := range previous.Resources {
		if _, found := findResource(updated.Resources, declared.Name); !found {
			d.logger.Warn("a resource was removed from the configuration but is left defined; "+
				"agents may be holding slots on it", "resource", declared.Name)
		}
	}

	if changed == 0 {
		return ""
	}

	return fmt.Sprintf("resources (%d)", changed)
}

func findResource(resources []config.ResourceConfig, name string) (config.ResourceConfig, bool) {
	for _, resource := range resources {
		if resource.Name == name {
			return resource, true
		}
	}

	return config.ResourceConfig{}, false
}

// sameTelegram compares the bridge settings. The allowlist is a slice, so the structs cannot simply
// be compared with ==.
func sameTelegram(before, after config.TelegramConfig) bool {
	if before.Enabled != after.Enabled || before.Token != after.Token ||
		before.TokenFile != after.TokenFile || before.PollTimeout != after.PollTimeout ||
		before.ReportDeadline != after.ReportDeadline {
		return false
	}
	if len(before.AllowedChatIDs) != len(after.AllowedChatIDs) {
		return false
	}
	for index := range before.AllowedChatIDs {
		if before.AllowedChatIDs[index] != after.AllowedChatIDs[index] {
			return false
		}
	}

	return true
}

// sameCapabilities compares the capability policy, whose resource list is a slice.
func sameCapabilities(before, after config.CapabilitiesConfig) bool {
	return before.CanBroadcast == after.CanBroadcast &&
		before.CanDefineResources == after.CanDefineResources &&
		before.CanRequestReports == after.CanRequestReports &&
		sameStrings(before.Resources, after.Resources)
}

func sameStrings(before, after []string) bool {
	if len(before) != len(after) {
		return false
	}
	for index := range before {
		if before[index] != after[index] {
			return false
		}
	}

	return true
}

func sameCommands(before, after map[string]config.CommandConfig) bool {
	if len(before) != len(after) {
		return false
	}
	for key, command := range before {
		other, found := after[key]
		if !found || other.Resource != command.Resource || other.Description != command.Description ||
			other.TimeLimit != command.TimeLimit || !sameStrings(command.Run, other.Run) {
			return false
		}
	}

	return true
}

// watchConfiguration reloads the configuration a while after the file stops changing.
//
// It waits for quiet rather than reacting to the first write: an editor saving a file often
// produces several changes in a row, and reloading half of one is worse than reloading none. The
// file has to read the same for stableChecks readings, one restart delay apart, before it is
// applied.
func (d *Daemon) watchConfiguration(ctx context.Context) {
	path := d.configPath()
	if path == "" {
		return
	}

	delay := time.Duration(d.settings().RestartDelay)
	if delay <= 0 {
		delay = config.DefaultRestartDelay
	}

	ticker := time.NewTicker(delay)
	defer ticker.Stop()

	loaded := fingerprintOf(path)

	var (
		candidate  fingerprint
		quietReads int
	)

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			current := fingerprintOf(path)

			switch current {
			case loaded:
				// Back to what is already running — an edit that was undone, or a touch.
				candidate, quietReads = fingerprint{}, 0

			case candidate:
				quietReads++
				if quietReads < stableChecks {
					continue
				}

				d.logger.Info("the configuration file has settled; reloading",
					"path", path, "after", (time.Duration(stableChecks) * delay).String())

				if err := d.Reload(ctx); err != nil {
					d.logger.Error("could not reload the configuration", "error", err)
					// Keep the fingerprint as the candidate rather than the loaded one, so a
					// corrected file is picked up without the operator touching it again.
					candidate, quietReads = fingerprint{}, 0

					continue
				}

				loaded, candidate, quietReads = current, fingerprint{}, 0

			default:
				// It changed again; start counting the quiet from here.
				candidate, quietReads = current, 1
			}
		}
	}
}

// fingerprint identifies the contents of the configuration file. The hash rather than the
// modification time, so `touch` does not cause a reload and two writes a second apart do.
type fingerprint struct {
	digest  [sha256.Size]byte
	missing bool
}

func fingerprintOf(path string) fingerprint {
	contents, err := os.ReadFile(path) //nolint:gosec // the operator's own configuration file
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fingerprint{missing: true}
		}

		// Unreadable for any other reason counts as "no change": the next reading will tell.
		return fingerprint{missing: true}
	}

	return fingerprint{digest: sha256.Sum256(contents)}
}
