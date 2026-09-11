package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"time"

	"sadeq.uk/lac/internal/transport/mcp"
)

// runMCP serves LAC over the Model Context Protocol on stdin and stdout.
//
// It is meant to be launched by an AI tool, not typed by a person: the tool starts it as a
// subprocess and talks to it in JSON. Registering this session with the daemon happens on the
// first tool call, and everything it holds is given back when the tool closes stdin.
func runMCP(ctx context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("mcp", flag.ContinueOnError)

	var (
		name    = flags.String("name", "", "how this session appears to the other agents")
		kind    = flags.String("kind", "", "the tool on the other end (default: taken from the client)")
		workdir = flags.String("workdir", "", "the directory this session works in (default: the current one)")
		wait    = flags.Duration("acquire-timeout", 5*time.Minute,
			"how long a slot request waits before telling the model to try again")
		verbose = flags.Bool("verbose", false, "log to stderr")
	)

	if err := parseAnywhere(flags, arguments); err != nil {
		return err
	}

	// stdout carries the protocol, so every diagnostic goes to stderr.
	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if *name == "" {
		*name = env.identity.name
	}
	if *workdir == "" {
		*workdir = env.identity.workdir
	}

	server := mcp.NewServer(mcp.Options{
		SocketPath:     env.socketPath,
		AgentName:      *name,
		AgentKind:      *kind,
		Workdir:        *workdir,
		AcquireTimeout: *wait,
		Logger:         logger,
	})

	return server.Serve(ctx) //nolint:wrapcheck // the server's errors already say what failed
}
