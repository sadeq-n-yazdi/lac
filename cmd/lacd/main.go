// Command lacd is the LAC coordination daemon. It listens on a Unix domain socket and arbitrates
// messaging, resource leases and reporting between the AI agents running on this machine.
//
// Only one daemon runs against a given state directory: a second one refuses to start rather than
// share a database and a socket with the first.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"sadeq.uk/lac/internal/config"
	"sadeq.uk/lac/internal/daemon"
	"sadeq.uk/lac/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "lacd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		showVersion  = flag.Bool("version", false, "print the version and exit")
		configPath   = flag.String("config", "", "configuration file (default: $XDG_CONFIG_HOME/lac/config.yaml)")
		socketPath   = flag.String("socket", "", "socket to listen on (overrides the configuration)")
		databasePath = flag.String("database", "", "database file (overrides the configuration)")
		logLevel     = flag.String("log-level", "", "debug, info, warn or error")
	)
	flag.Parse()

	if *showVersion {
		fmt.Fprintf(os.Stdout, "lacd %s\n", version.String())
		return nil
	}

	source := config.Options{
		ConfigPath:   *configPath,
		SocketPath:   *socketPath,
		DatabasePath: *databasePath,
		LogLevel:     *logLevel,
	}

	configuration, err := config.Load(source)
	if err != nil {
		return err
	}

	logger := newLogger(configuration.LogLevel)

	// Interrupt and terminate both mean "stop"; the second one arriving restores the default
	// behaviour, so an operator who is out of patience can always kill the process.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	instance, err := daemon.New(ctx, configuration, source, logger)
	if err != nil {
		return err
	}
	defer func() {
		if err := instance.Close(); err != nil {
			logger.Error("could not shut down cleanly", "error", err)
		}
	}()

	// A hang-up reloads the configuration, which is the conventional way to ask a daemon to
	// re-read itself without stopping what it is doing.
	go reloadOnHangUp(ctx, instance, logger)

	if err := instance.Run(ctx); err != nil {
		return fmt.Errorf("serving: %w", err)
	}

	return nil
}

// reloadOnHangUp re-reads the configuration each time SIGHUP arrives, until the daemon stops.
func reloadOnHangUp(ctx context.Context, instance *daemon.Daemon, logger *slog.Logger) {
	hangUps := make(chan os.Signal, 1)
	signal.Notify(hangUps, syscall.SIGHUP)
	defer signal.Stop(hangUps)

	for {
		select {
		case <-ctx.Done():
			return
		case <-hangUps:
			logger.Info("reloading the configuration on SIGHUP")

			if err := instance.Reload(ctx); err != nil {
				// A bad configuration must not stop a daemon that is working: it carries on with
				// what it already had.
				logger.Error("the configuration was not reloaded", "error", err)
			}
		}
	}
}

func newLogger(level string) *slog.Logger {
	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(level)); err != nil {
		parsed = slog.LevelInfo
	}

	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parsed}))
}
