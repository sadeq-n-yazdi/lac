package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"sadeq.uk/lac/pkg/lacclient"
)

// runRun is the command LAC exists for: wait for a slot on a resource, run something, give the
// slot back.
//
//	lac run --resource test -- make test
//
// Six agents can issue that at once on a four-slot resource; four run, two wait their turn. The
// slot is released however the command ends — success, failure, or a signal — and the command's
// exit status is passed straight through, so it can stand in front of an existing command without
// anything else changing.
func runRun(ctx context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)

	var (
		resource = flags.String("resource", "", "the resource to queue for (required)")
		reason   = flags.String("reason", "", "what the slot is for, shown in the queue")
		priority = flags.Int("priority", 0, "higher goes first")
		timeout  = flags.Duration("timeout", 0, "give up waiting after this long")
		quiet    = flags.Bool("quiet", false, "do not report waiting for a slot")
	)

	if err := parseAnywhere(flags, arguments); err != nil {
		return err
	}
	if *resource == "" {
		return errors.New("usage: lac run --resource <name> -- <command> [arguments]")
	}

	commandLine := flags.Args()
	if len(commandLine) == 0 {
		return errors.New("nothing to run: put the command after --, as in lac run --resource test -- make test")
	}

	client, release, _, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	if *reason == "" {
		*reason = strings.Join(commandLine, " ")
	}

	if !*quiet && !env.asJSON {
		reportWaiting(ctx, env, client, *resource)
	}

	lease, err := client.Acquire(ctx, lacclient.AcquireRequest{
		Resource: *resource,
		Reason:   *reason,
		Priority: *priority,
		Timeout:  *timeout,
		Metadata: map[string]any{"command": commandLine, "pid": os.Getpid()},
	})
	if err != nil {
		return fmt.Errorf("waiting for a slot on %q: %w", *resource, err)
	}

	// The slot goes back however this ends. A separate context, because ctx may already be
	// cancelled by the signal that stopped the command.
	defer func() {
		release, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		switch err := client.Release(release, lease.ID); {
		case errors.Is(err, lacclient.ErrNotConnected):
			// The daemon stopped while the command was running. The slot is not lost: the daemon
			// reclaims what a departed agent held when it comes back.
			fmt.Fprintf(os.Stderr,
				"lac: the daemon stopped before the slot on %s could be released; "+
					"it is reclaimed automatically\n", *resource)
		case err != nil:
			fmt.Fprintf(os.Stderr, "lac: could not release the slot on %s: %v\n", *resource, err)
		}
	}()

	// A long command must not lose its slot to the reaper while it is still working.
	stopRenewing := keepAlive(ctx, client, lease)
	defer stopRenewing()

	return execute(ctx, commandLine)
}

// reportWaiting tells the operator why nothing is happening, but only when something really is in
// the way. Printing "waiting" when the slot is free would be noise on every single run.
func reportWaiting(ctx context.Context, env *environment, client *lacclient.Client, resource string) {
	status, err := client.ResourceStatus(ctx, resource)
	if err != nil || status.Free > 0 {
		return
	}

	fmt.Fprintf(os.Stderr, "lac: waiting for a slot on %s (%d held, %d waiting)\n",
		resource, status.Held, status.Waiting)
	_ = env.output.Flush()
}

// keepAlive renews the lease in the background for as long as the command runs, and returns a
// function that stops it.
func keepAlive(ctx context.Context, client *lacclient.Client, lease lacclient.Lease) func() {
	interval := lease.RenewAfter()
	if interval <= 0 {
		interval = time.Minute
	}

	renewing, stop := context.WithCancel(ctx)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-renewing.Done():
				return
			case <-ticker.C:
				if _, err := client.Renew(renewing, lease.ID); err != nil {
					if renewing.Err() != nil {
						return
					}
					// Losing the slot mid-run matters: another agent may already be using it, so
					// say so rather than carrying on quietly.
					fmt.Fprintf(os.Stderr, "lac: could not renew the slot on %s: %v\n",
						lease.Resource, err)

					return
				}
			}
		}
	}()

	return stop
}

// execute runs the command with the caller's own streams, and reports its exit status as an error
// so main can exit with the same code.
func execute(ctx context.Context, commandLine []string) error {
	command := exec.CommandContext(ctx, commandLine[0], commandLine[1:]...) //nolint:gosec // the operator's own command
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr

	// A signal must reach the command so it can clean up, rather than being killed outright.
	command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
	command.WaitDelay = 10 * time.Second

	err := command.Run()
	if err == nil {
		return nil
	}

	var failure *exec.ExitError
	if errors.As(err, &failure) {
		return exitCode{code: failure.ExitCode()}
	}

	return fmt.Errorf("running %s: %w", commandLine[0], err)
}
