package daemon

import (
	"context"
	"time"

	"code.sadeq.uk/lac/internal/api"
	"code.sadeq.uk/lac/internal/auth"
	"code.sadeq.uk/lac/internal/service/leasing"
	"code.sadeq.uk/lac/internal/service/messaging"
	"code.sadeq.uk/lac/internal/service/registry"
)

// housekeepingInterval is how often the daemon looks for abandoned leases and silent agents. It is
// a safety net rather than the mechanism: a released slot wakes its waiters immediately, and this
// only catches what nobody told us about.
const housekeepingInterval = 10 * time.Second

// attachServices builds the registry, messaging and leasing services and registers their methods.
func (d *Daemon) attachServices(ctx context.Context) error {
	secret, err := auth.LoadOrCreateSecret(d.configuration.SecretPath)
	if err != nil {
		return err
	}

	authenticator, err := auth.New(d.store, secret, auth.Options{Logger: d.logger})
	if err != nil {
		return err
	}

	workdirRoots, err := d.configuration.WorkdirRoots()
	if err != nil {
		return err
	}

	d.registry = registry.New(d.store, authenticator, registry.Options{
		TimeToLive:      time.Duration(d.configuration.AgentTimeToLive),
		WorkdirRoots:    workdirRoots,
		CapabilitiesFor: d.configuration.CapabilitiesFor,
		Logger:          d.logger,
	})

	d.leasing = leasing.New(d.store, leasing.Options{
		DefaultTimeToLive: time.Duration(d.configuration.DefaultLeaseTimeToLive),
		Logger:            d.logger,
	})

	// The messaging service pushes arrivals through the JSON-RPC server, which is how a connected
	// agent hears about a message without asking for it.
	d.messaging = messaging.New(d.store, messaging.Options{
		Notifier: d.server,
		Logger:   d.logger,
	})

	api.New(d.registry, d.messaging, d.leasing, authenticator, api.Options{Logger: d.logger}).
		Register(d.router)

	return d.defineConfiguredResources(ctx)
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
	}

	if pruned, err := d.messaging.Prune(ctx); err != nil {
		d.logger.Error("could not prune messages", "error", err)
	} else if pruned > 0 {
		d.logger.Debug("pruned acknowledged messages", "count", pruned)
	}
}
