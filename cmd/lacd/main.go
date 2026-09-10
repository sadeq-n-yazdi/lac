// Command lacd is the LAC coordination daemon. It listens on a Unix domain socket and arbitrates
// messaging, resource leases and reporting between the AI agents running on this machine.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"code.sadeq.uk/lac/internal/config"
	"code.sadeq.uk/lac/internal/daemon"
	"code.sadeq.uk/lac/internal/version"
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

	configuration, err := config.Load(config.Options{
		ConfigPath:   *configPath,
		SocketPath:   *socketPath,
		DatabasePath: *databasePath,
		LogLevel:     *logLevel,
	})
	if err != nil {
		return err
	}

	logger := newLogger(configuration.LogLevel)

	// Interrupt and terminate both mean "stop"; the second one arriving restores the default
	// behaviour, so an operator who is out of patience can always kill the process.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	instance, err := daemon.New(ctx, configuration, logger)
	if err != nil {
		return err
	}
	defer func() {
		if err := instance.Close(); err != nil {
			logger.Error("could not shut down cleanly", "error", err)
		}
	}()

	if err := instance.Run(ctx); err != nil {
		return fmt.Errorf("serving: %w", err)
	}

	return nil
}

func newLogger(level string) *slog.Logger {
	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(level)); err != nil {
		parsed = slog.LevelInfo
	}

	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parsed}))
}
