package daemon

import (
	"context"
	"fmt"
	"time"

	"sadeq.uk/lac/internal/api"
	"sadeq.uk/lac/internal/auth"
	"sadeq.uk/lac/internal/core"
	"sadeq.uk/lac/internal/service/dispatch"
	"sadeq.uk/lac/internal/service/leasing"
	"sadeq.uk/lac/internal/service/messaging"
	"sadeq.uk/lac/internal/service/registry"
	"sadeq.uk/lac/internal/service/reporting"
	"sadeq.uk/lac/internal/transport/telegram"
)

// housekeepingInterval is how often the daemon looks for abandoned leases and silent agents. It is
// a safety net rather than the mechanism: a released slot wakes its waiters immediately, and this
// only catches what nobody told us about.
const housekeepingInterval = 10 * time.Second

// attachServices builds the registry, messaging and leasing services and registers their methods.
func (d *Daemon) attachServices(ctx context.Context) error {
	settings := d.settings()

	secret, err := auth.LoadOrCreateSecret(settings.SecretPath)
	if err != nil {
		return err
	}

	authenticator, err := auth.New(d.store, secret, auth.Options{Logger: d.logger})
	if err != nil {
		return err
	}

	workdirRoots, err := settings.WorkdirRoots()
	if err != nil {
		return err
	}

	d.registry = registry.New(d.store, authenticator, registry.Options{
		TimeToLive:   time.Duration(settings.AgentTimeToLive),
		WorkdirRoots: workdirRoots,
		// Read through settings() rather than capturing the policy, so a reloaded capability or
		// operator list applies to the next agent that registers.
		CapabilitiesFor: func(name string) core.Capabilities { return d.settings().CapabilitiesFor(name) },
		Logger:          d.logger,
	})

	d.leasing = leasing.New(d.store, leasing.Options{
		DefaultTimeToLive: time.Duration(settings.DefaultLeaseTimeToLive),
		Logger:            d.logger,
	})

	// Arrivals are pushed to whichever front end can deliver them: the JSON-RPC server for a
	// connected agent, and the Telegram bridge for the operator's phone.
	d.notifier = newFanOutNotifier(d.server)

	d.messaging = messaging.New(d.store, messaging.Options{
		Notifier: d.notifier,
		Logger:   d.logger,
	})

	d.reporting = reporting.New(d.store, d.registry, d.messaging, reporting.Options{Logger: d.logger})

	// The catalogue reads through settings() too, so a reloaded command list is live.
	d.dispatch = dispatch.New(d.store, catalogue{daemon: d}, dispatch.Options{
		WorkdirRoots: workdirRoots,
		Logger:       d.logger,
	})

	api.New(d.registry, d.messaging, d.leasing, d.reporting, d.dispatch, authenticator, api.Options{
		Logger:   d.logger,
		Notifier: d.notifier,
		Reload:   d.Reload,
	}).Register(d.router)

	if err := d.attachTelegram(); err != nil {
		return err
	}

	return d.defineConfiguredResources(ctx)
}

// attachTelegram builds the optional bridge to the operator's phone. A misconfigured bridge is a
// start-up failure rather than a silent absence: an operator who asked for it should be told why
// they are not getting it.
func (d *Daemon) attachTelegram() error {
	settings := d.settings()
	if !settings.Telegram.Enabled {
		return nil
	}

	token, err := settings.Telegram.ResolveToken()
	if err != nil {
		return err
	}

	bridge, err := telegram.New(telegram.Services{
		Registry:  d.registry,
		Messaging: d.messaging,
		Leasing:   d.leasing,
		Reporting: d.reporting,
		Audit:     d.store.Audit(),
	}, telegram.Options{
		Token:          token,
		AllowedChatIDs: settings.Telegram.AllowedChatIDs,
		PollTimeout:    time.Duration(settings.Telegram.PollTimeout),
		ReportDeadline: time.Duration(settings.Telegram.ReportDeadline),
		Logger:         d.logger,
	})
	if err != nil {
		return fmt.Errorf("setting up the telegram bridge: %w", err)
	}

	d.telegram = bridge
	d.notifier.add(bridge)

	return nil
}

// housekeeping reclaims what crashed agents left behind, until the context ends.
//
// Everything it does is also done at the moment it becomes true — a release wakes waiters, a
// deregistration gives slots back — so this loop exists for the cases where nobody was around to
// tell us: a killed process, a laptop that slept, a daemon that restarted.
func (d *Daemon) housekeeping(ctx context.Context) {
	ticker := time.NewTicker(housekeepingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.runHousekeeping(ctx)
		}
	}
}

func (d *Daemon) runHousekeeping(ctx context.Context) {
	if reclaimed, err := d.leasing.ReapExpired(ctx); err != nil {
		d.logger.Error("could not reclaim expired leases", "error", err)
	} else if reclaimed > 0 {
		d.logger.Info("reclaimed expired leases", "count", reclaimed)
	}

	stale, err := d.registry.ReapStale(ctx)
	if err != nil {
		d.logger.Error("could not find stale agents", "error", err)
		return
	}

	for _, agent := range stale {
		released, err := d.leasing.ReleaseEverythingHeldBy(ctx, agent.ID, "the agent went stale")
		if err != nil {
			d.logger.Error("could not reclaim what a stale agent held",
				"agent", agent.Name, "error", err)
			continue
		}
		if released > 0 {
			d.logger.Info("reclaimed what a stale agent held",
				"agent", agent.Name, "leases", released)
		}

		// A worker that went away mid-task leaves work that still needs doing.
		if requeued, err := d.dispatch.ReleaseAbandoned(ctx, agent.ID); err != nil {
			d.logger.Error("could not requeue an absent worker's tasks",
				"agent", agent.Name, "error", err)
		} else if requeued > 0 {
			d.logger.Info("requeued an absent worker's tasks",
				"agent", agent.Name, "tasks", requeued)
		}
	}

	if pruned, err := d.messaging.Prune(ctx); err != nil {
		d.logger.Error("could not prune messages", "error", err)
	} else if pruned > 0 {
		d.logger.Debug("pruned acknowledged messages", "count", pruned)
	}
}
