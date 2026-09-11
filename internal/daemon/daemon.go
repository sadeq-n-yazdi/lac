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
	"sync"
	"time"

	"sadeq.uk/lac/internal/config"
	"sadeq.uk/lac/internal/core"
	"sadeq.uk/lac/internal/service/dispatch"
	"sadeq.uk/lac/internal/service/leasing"
	"sadeq.uk/lac/internal/service/messaging"
	"sadeq.uk/lac/internal/service/prwatch"
	"sadeq.uk/lac/internal/service/registry"
	"sadeq.uk/lac/internal/service/reporting"
	"sadeq.uk/lac/internal/singleton"
	"sadeq.uk/lac/internal/store/sqlite"
	"sadeq.uk/lac/internal/transport/jsonrpc"
	"sadeq.uk/lac/internal/transport/telegram"
	"sadeq.uk/lac/internal/transport/unixsock"
)

// Daemon is a running LAC instance.
type Daemon struct {
	// configuration is replaced wholesale by a reload, so everything that reads it does so through
	// settings() rather than holding a copy from start-up.
	configurationMutex sync.RWMutex
	configuration      config.Config

	// source is how the configuration was loaded, so a reload re-reads the same file.
	source config.Options

	logger    *slog.Logger
	lock      *singleton.Lock
	store     *sqlite.Store
	listener  *unixsock.Listener
	server    *jsonrpc.Server
	router    *jsonrpc.Router
	startedAt time.Time

	registry  *registry.Service
	messaging *messaging.Service
	leasing   *leasing.Service
	reporting *reporting.Service
	dispatch  *dispatch.Service
	watcher   *prwatch.Service
	notifier  *fanOutNotifier
	telegram  *telegram.Bridge
}

// New prepares a daemon: it takes the single-instance lock, creates the directories, opens the
// database, applies migrations and claims the socket. It does not serve anything yet, so a caller
// can still register methods.
//
// source says how the configuration was loaded, so a reload can re-read the same file. The lock is
// taken first and the socket claimed last, so a start-up that fails for any reason never disturbs a
// daemon that is already running.
func New(
	ctx context.Context, configuration config.Config, source config.Options, logger *slog.Logger,
) (*Daemon, error) {
	if logger == nil {
		logger = slog.Default()
	}

	if err := config.EnsureDirectories(configuration); err != nil {
		return nil, err
	}

	// Before anything else: two daemons against one database and one socket would each serve half
	// the agents and disagree about who holds what.
	lock, err := singleton.Acquire(configuration.LockPath())
	if err != nil {
		return nil, err
	}

	store, err := sqlite.Open(ctx, configuration.DatabasePath)
	if err != nil {
		return nil, errors.Join(err, lock.Release())
	}

	daemon := &Daemon{
		configuration: configuration,
		source:        source,
		logger:        logger,
		lock:          lock,
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
		return nil, errors.Join(err, store.Close(), lock.Release())
	}

	// The socket is claimed last, so a start-up that fails for any other reason never disturbs a
	// daemon that is already running.
	listener, err := unixsock.Listen(ctx, unixsock.Options{Path: configuration.SocketPath, Logger: logger})
	if err != nil {
		return nil, errors.Join(err, store.Close(), lock.Release())
	}
	daemon.listener = listener

	return daemon, nil
}

// Store exposes the storage layer, so services can be attached before serving starts.
func (d *Daemon) Store() core.Store { return d.store }

// settings returns the configuration as it stands, which a reload may have replaced.
func (d *Daemon) settings() config.Config {
	d.configurationMutex.RLock()
	defer d.configurationMutex.RUnlock()

	return d.configuration
}

// configPath is the configuration file to watch, or empty when the daemon was started without one.
func (d *Daemon) configPath() string {
	path, exists := config.SourcePath(d.source)
	if !exists {
		return ""
	}

	return path
}

// Commands returns the jobs a shared worker can be asked to run, as the daemon currently
// understands them — which a reload may have changed.
func (d *Daemon) Commands() []dispatch.Command { return d.dispatch.Commands() }

// Router exposes the method router, so services can register their methods.
func (d *Daemon) Router() *jsonrpc.Router { return d.router }

// SocketPath is where clients should connect.
func (d *Daemon) SocketPath() string { return d.listener.Path() }

// Run serves until the context is cancelled, then shuts down within the configured grace period.
func (d *Daemon) Run(ctx context.Context) error {
	d.logger.Info("lac daemon started",
		"socket", d.listener.Path(),
		"database", d.settings().DatabasePath,
		"lock", d.lock.Path(),
		"pid", os.Getpid())

	// Housekeeping, the configuration watcher and the optional Telegram bridge run alongside
	// serving and stop with it.
	go d.housekeeping(ctx)
	go d.watchConfiguration(ctx)
	go d.runTelegram(ctx)
	go d.runPullRequestWatcher(ctx)

	err := d.server.Serve(ctx, d.listener)

	// Shutdown gets its own context: the one that just ended is no use for waiting on anything.
	shutdownErr := d.server.Shutdown(context.WithoutCancel(ctx))
	d.logger.Info("lac daemon stopped")

	return errors.Join(err, shutdownErr)
}

// runPullRequestWatcher follows the pull requests agents asked about, until the daemon stops.
//
// It registers itself on the roster first, so a change arrives from something with a name rather
// than from the agent to itself.
func (d *Daemon) runPullRequestWatcher(ctx context.Context) {
	if d.watcher == nil {
		return
	}

	registration, err := d.registry.RegisterInternal(ctx, registry.RegisterRequest{
		Name: prwatch.AgentName, Kind: "watcher", ProcessID: 0,
	})
	if err != nil {
		d.logger.Error("the pull request watcher could not register", "error", err)
	} else {
		d.watcher.SetSender(registration.Agent.ID)
	}

	d.watcher.Run(ctx)
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

// Close releases the socket, the database and the single-instance lock, in that order: nothing
// else may start until this daemon has let go of everything it held.
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
	if d.lock != nil {
		if err := d.lock.Release(); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}

	return closeErr
}

// defineConfiguredResources applies the resources declared in the configuration file, so a fresh
// install already knows this machine's limits without anyone having to run a command.
func (d *Daemon) defineConfiguredResources(ctx context.Context) error {
	settings := d.settings()
	defaultTimeToLive := time.Duration(settings.DefaultLeaseTimeToLive)

	for _, declared := range settings.Resources {
		resource := declared.Resource(defaultTimeToLive)
		if err := d.store.Resources().Define(ctx, resource); err != nil {
			return fmt.Errorf("defining the configured resource %q: %w", resource.Name, err)
		}

		d.logger.Debug("resource defined from configuration",
			"resource", resource.Name, "capacity", resource.Capacity)
	}

	return nil
}
