// Package daemon wires LAC's parts together: configuration, storage, the socket and the JSON-RPC
// server. It owns start-up order and shutdown order, and nothing else — the behaviour lives in the
// services it hosts.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"code.sadeq.uk/lac/internal/config"
	"code.sadeq.uk/lac/internal/core"
	"code.sadeq.uk/lac/internal/service/leasing"
	"code.sadeq.uk/lac/internal/service/messaging"
	"code.sadeq.uk/lac/internal/service/registry"
	"code.sadeq.uk/lac/internal/service/reporting"
	"code.sadeq.uk/lac/internal/store/sqlite"
	"code.sadeq.uk/lac/internal/transport/jsonrpc"
	"code.sadeq.uk/lac/internal/transport/telegram"
	"code.sadeq.uk/lac/internal/transport/unixsock"
)

// Daemon is a running LAC instance.
type Daemon struct {
	configuration config.Config
	logger        *slog.Logger
	store         *sqlite.Store
	listener      *unixsock.Listener
	server        *jsonrpc.Server
	router        *jsonrpc.Router
	startedAt     time.Time

	registry  *registry.Service
	messaging *messaging.Service
	leasing   *leasing.Service
	reporting *reporting.Service
	notifier  *fanOutNotifier
	telegram  *telegram.Bridge
}

// New prepares a daemon: it creates the directories, opens the database, applies migrations and
// claims the socket. It does not serve anything yet, so a caller can still register methods.
//
// The socket is claimed last, so a start-up that fails for any other reason never disturbs a
// daemon that is already running.
func New(ctx context.Context, configuration config.Config, logger *slog.Logger) (*Daemon, error) {
	if logger == nil {
		logger = slog.Default()
	}

	if err := config.EnsureDirectories(configuration); err != nil {
		return nil, err
	}

	store, err := sqlite.Open(ctx, configuration.DatabasePath)
	if err != nil {
		return nil, err
	}

	daemon := &Daemon{
		configuration: configuration,
		logger:        logger,
		store:         store,
		router:        jsonrpc.NewRouter(),
		startedAt:     time.Now().UTC(),
	}

	// The server exists before the services, because the messaging service pushes notifications
	// through it.
	daemon.server = jsonrpc.NewServer(daemon.router, jsonrpc.Options{
		Logger:        logger,
		ShutdownGrace: time.Duration(configuration.ShutdownGrace),
	})

	daemon.registerMethods()

	if err := daemon.attachServices(ctx); err != nil {
		return nil, errors.Join(err, store.Close())
	}

	// The socket is claimed last, so a start-up that fails for any other reason never disturbs a
	// daemon that is already running.
	listener, err := unixsock.Listen(ctx, unixsock.Options{Path: configuration.SocketPath, Logger: logger})
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	daemon.listener = listener

	return daemon, nil
}

// Store exposes the storage layer, so services can be attached before serving starts.
func (d *Daemon) Store() core.Store { return d.store }

// Router exposes the method router, so services can register their methods.
func (d *Daemon) Router() *jsonrpc.Router { return d.router }

// SocketPath is where clients should connect.
func (d *Daemon) SocketPath() string { return d.listener.Path() }

// Run serves until the context is cancelled, then shuts down within the configured grace period.
func (d *Daemon) Run(ctx context.Context) error {
	d.logger.Info("lac daemon started",
		"socket", d.listener.Path(),
		"database", d.configuration.DatabasePath,
		"pid", os.Getpid())

	// Housekeeping and the optional Telegram bridge run alongside serving and stop with it.
	go d.housekeeping(ctx)
	go d.runTelegram(ctx)

	err := d.server.Serve(ctx, d.listener)

	// Shutdown gets its own context: the one that just ended is no use for waiting on anything.
	shutdownErr := d.server.Shutdown(context.WithoutCancel(ctx))
	d.logger.Info("lac daemon stopped")

	return errors.Join(err, shutdownErr)
}

// runTelegram relays to the operator's phone until the daemon stops. A bridge that cannot start —
// a rejected token, a network that is down — must not take the daemon with it: the agents on this
// machine still need coordinating.
func (d *Daemon) runTelegram(ctx context.Context) {
	if d.telegram == nil {
		return
	}

	if err := d.telegram.Run(ctx); err != nil && ctx.Err() == nil {
		d.logger.Error("the telegram bridge stopped", "error", err)
	}
}

// Close releases the socket and the database. It is safe to call after Run.
func (d *Daemon) Close() error {
	var closeErr error

	if d.listener != nil {
		if err := d.listener.Close(); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	if d.store != nil {
		if err := d.store.Close(); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}

	return closeErr
}

// defineConfiguredResources applies the resources declared in the configuration file, so a fresh
// install already knows this machine's limits without anyone having to run a command.
func (d *Daemon) defineConfiguredResources(ctx context.Context) error {
	defaultTimeToLive := time.Duration(d.configuration.DefaultLeaseTimeToLive)

	for _, declared := range d.configuration.Resources {
		resource := declared.Resource(defaultTimeToLive)
		if err := d.store.Resources().Define(ctx, resource); err != nil {
			return fmt.Errorf("defining the configured resource %q: %w", resource.Name, err)
		}

		d.logger.Debug("resource defined from configuration",
			"resource", resource.Name, "capacity", resource.Capacity)
	}

	return nil
}
