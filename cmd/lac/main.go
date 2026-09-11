// Command lac is the command-line client for the LAC coordination daemon.
//
// Agents and their operator use it to register, exchange messages, queue for shared resources and
// see what everyone is doing. The command that matters most is `lac run`: put it in front of
// anything heavy and it waits its turn before running.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"text/tabwriter"

	"sadeq.uk/lac/internal/version"
)

// exitCode carries a specific exit status out of a command, so `lac run` can return whatever the
// command it ran returned.
type exitCode struct {
	code int
}

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// environment is everything a command needs from the outside world.
type environment struct {
	socketPath string
	asJSON     bool
	identity   identity
	output     *tabwriter.Writer
}

// command is one subcommand.
type command struct {
	name    string
	summary string
	// run receives the arguments after the subcommand name.
	run func(ctx context.Context, env *environment, arguments []string) error
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		var requested exitCode
		if errors.As(err, &requested) {
			os.Exit(requested.code)
		}

		fmt.Fprintf(os.Stderr, "lac: %v\n", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("lac", flag.ContinueOnError)
	flags.Usage = func() { usage(flags) }

	var (
		socketPath = flags.String("socket", "", "the daemon's socket (default: the XDG location)")
		asJSON     = flags.Bool("json", false, "print machine-readable JSON instead of a table")
		token      = flags.String("token", "", "authenticate with this token instead of registering")
		name       = flags.String("name", "", "register under this name (default: the directory and pid)")
		kind       = flags.String("kind", "cli", "the tool behind this agent")
		workdir    = flags.String("workdir", "", "the directory being worked in (default: the current one)")
	)

	if err := flags.Parse(arguments); err != nil {
		return err //nolint:wrapcheck // the flag package already printed the problem
	}

	if flags.NArg() == 0 {
		usage(flags)
		return errors.New("no command given")
	}

	output := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	defer func() { _ = output.Flush() }()

	env := &environment{
		socketPath: *socketPath,
		asJSON:     *asJSON,
		identity:   identity{token: *token, name: *name, kind: *kind, workdir: *workdir},
		output:     output,
	}

	// Interrupt and terminate must reach a command that is waiting in a queue or running a child,
	// so both are turned into a cancelled context rather than killing the process outright.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	requested := flags.Arg(0)
	for _, candidate := range commands() {
		if candidate.name == requested {
			return candidate.run(ctx, env, flags.Args()[1:])
		}
	}

	usage(flags)

	return fmt.Errorf("unknown command %q", requested)
}

func usage(flags *flag.FlagSet) {
	fmt.Fprintf(os.Stderr, `lac %s — talk to the local agents coordinator

Usage:
  lac [flags] <command> [arguments]

Commands:
`, version.Version)

	available := commands()
	sort.Slice(available, func(i, j int) bool { return available[i].name < available[j].name })

	writer := tabwriter.NewWriter(os.Stderr, 0, 8, 2, ' ', 0)
	for _, candidate := range available {
		fmt.Fprintf(writer, "  %s\t%s\n", candidate.name, candidate.summary)
	}
	_ = writer.Flush()

	fmt.Fprint(os.Stderr, "\nFlags:\n")
	flags.PrintDefaults()

	fmt.Fprint(os.Stderr, `
Examples:
  lac run --resource test -- make test     wait for a test slot, then run the tests
  lac agents                               who else is working right now
  lac send claude-b question '{"ask":"are you on the parser?"}'
  lac queue test                           who is waiting for a test slot
`)
}
