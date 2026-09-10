package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"code.sadeq.uk/lac/pkg/lacclient"
)

// outputFlushInterval is how often a running task's output is sent back, so a requester watching it
// sees progress without the daemon being written to for every line.
const outputFlushInterval = time.Second

// runWorker turns this process into a shared worker: it waits for work other agents submitted,
// takes a slot on the resource, runs what the daemon tells it to, and reports the result.
//
//	lac worker --resource test
//
// The worker never decides what to run. It asks for work, and the daemon replies with a command
// from the operator's configuration. That is the whole reason dispatch is safe to have: an agent
// can ask for "the tests" and cannot say what "the tests" means.
func runWorker(ctx context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("worker", flag.ContinueOnError)
	var (
		resource = flags.String("resource", "", "the resource this worker serves (required)")
		once     = flags.Bool("once", false, "do one piece of work and stop")
		quiet    = flags.Bool("quiet", false, "do not echo what is running")
	)

	if err := flags.Parse(arguments); err != nil {
		return err //nolint:wrapcheck // the flag package already printed the problem
	}
	if *resource == "" {
		return errors.New("usage: lac worker --resource <name>")
	}

	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	if !*quiet {
		fmt.Fprintf(os.Stderr, "lac: working on %q; waiting for something to do\n", *resource)
	}

	for {
		if err := workOnce(ctx, client, *resource, *quiet); err != nil {
			if ctx.Err() != nil || errors.Is(err, lacclient.ErrNotConnected) {
				return nil
			}

			return err
		}

		if *once {
			return nil
		}
	}
}

// workOnce waits for work, takes a slot, runs the job, reports it, and gives the slot back.
//
// The order matters. Waiting for work happens *before* taking a slot, because an idle worker
// holding a slot occupies capacity it is not using — two idle workers on a two-slot resource would
// leave nobody else able to run anything. The slot is then held for exactly as long as the work
// runs, so dispatched work counts against capacity exactly as `lac run` does.
func workOnce(ctx context.Context, client *lacclient.Client, resource string, quiet bool) error {
	if err := client.WaitForWork(ctx, resource); err != nil {
		return fmt.Errorf("waiting for work on %q: %w", resource, err)
	}

	lease, err := client.Acquire(ctx, lacclient.AcquireRequest{
		Resource: resource, Reason: "running dispatched work",
	})
	if err != nil {
		return fmt.Errorf("taking a slot on %q: %w", resource, err)
	}

	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		if err := client.Release(releaseCtx, lease.ID); err != nil {
			fmt.Fprintf(os.Stderr, "lac: could not release the slot on %s: %v\n", resource, err)
		}
	}()

	// Another worker may have taken it while we were queueing for a slot. Say so and loop, rather
	// than holding the slot open waiting for the next piece of work to arrive.
	claimed, err := client.ClaimTask(ctx, resource, lease.ID, true)
	if err != nil {
		if lacclient.ErrorCode(err) == lacclient.CodeNotFound {
			return nil
		}

		return fmt.Errorf("claiming work on %q: %w", resource, err)
	}

	if !quiet {
		fmt.Fprintf(os.Stderr, "lac: running %q for %s in %s\n",
			claimed.Task.Command, claimed.Task.Requester, claimed.Task.Workdir)
	}

	stopRenewing := keepAlive(ctx, client, lease)
	defer stopRenewing()

	exitCode, failure := runTask(ctx, client, claimed)

	if _, err := client.CompleteTask(ctx, claimed.Task.ID, exitCode, failure); err != nil {
		return fmt.Errorf("reporting the result of %s: %w", claimed.Task.ID, err)
	}

	if !quiet {
		fmt.Fprintf(os.Stderr, "lac: finished %q with exit status %d\n", claimed.Task.Command, exitCode)
	}

	return nil
}

// runTask executes the command the daemon supplied, streaming its output back as it goes.
func runTask(
	ctx context.Context, client *lacclient.Client, claimed lacclient.ClaimedTask,
) (exitCode int, failure string) {
	if len(claimed.Run) == 0 {
		return -1, "the daemon supplied no command to run"
	}

	if claimed.TimeLimit != "" {
		limit, err := time.ParseDuration(claimed.TimeLimit)
		if err == nil && limit > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, limit)
			defer cancel()
		}
	}

	// The command comes from the daemon's configuration, and the working directory was checked
	// against the allowed roots when the task was submitted. No shell is involved, so nothing here
	// is ever interpreted as a pipeline or a substitution.
	command := exec.CommandContext(ctx, claimed.Run[0], claimed.Run[1:]...) //nolint:gosec // from the operator's own configuration
	command.Dir = claimed.Task.Workdir
	command.Stdin = nil

	reader, writer := io.Pipe()
	command.Stdout = writer
	command.Stderr = writer

	// A signal should let the command clean up rather than killing it outright.
	command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
	command.WaitDelay = 10 * time.Second

	if err := command.Start(); err != nil {
		_ = writer.Close()
		_ = reader.Close()

		return -1, fmt.Sprintf("could not start %s: %v", claimed.Run[0], err)
	}

	var streaming sync.WaitGroup
	streaming.Add(1)

	go func() {
		defer streaming.Done()
		streamOutput(ctx, client, claimed.Task.ID, reader)
	}()

	waitErr := command.Wait()
	_ = writer.Close()
	streaming.Wait()
	_ = reader.Close()

	switch {
	case waitErr == nil:
		return 0, ""

	case ctx.Err() != nil:
		return -1, "the command ran out of time and was stopped"
	}

	var exited *exec.ExitError
	if errors.As(waitErr, &exited) {
		return exited.ExitCode(), ""
	}

	return -1, fmt.Sprintf("running %s: %v", claimed.Run[0], waitErr)
}

// streamOutput sends the command's output back in batches, so the requester sees progress without
// a call to the daemon for every line.
func streamOutput(ctx context.Context, client *lacclient.Client, taskID string, reader io.Reader) {
	var (
		pending   strings.Builder
		lastFlush = time.Now()
	)

	flush := func() {
		if pending.Len() == 0 {
			return
		}

		chunk := pending.String()
		pending.Reset()
		lastFlush = time.Now()

		sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()

		if err := client.AppendTaskOutput(sendCtx, taskID, chunk); err != nil {
			fmt.Fprintf(os.Stderr, "lac: could not send task output: %v\n", err)
		}
	}

	lines := bufio.NewScanner(reader)
	lines.Buffer(make([]byte, 0, 4096), 1<<20)

	for lines.Scan() {
		pending.WriteString(lines.Text())
		pending.WriteString("\n")

		if time.Since(lastFlush) >= outputFlushInterval || pending.Len() > 16<<10 {
			flush()
		}
	}

	flush()
}
