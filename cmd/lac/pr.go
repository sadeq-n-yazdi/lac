package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"sadeq.uk/lac/pkg/lacclient"
)

// runPR is the operator's and the agent's window onto a pull request: watch one, see where it has
// got to, and stop hearing about it.
//
//	lac pr watch sadeq-n-yazdi/lac#31
//	lac pr status sadeq-n-yazdi/lac#31 --refresh
//	lac pr list
//	lac pr unwatch sadeq-n-yazdi/lac#31
func runPR(ctx context.Context, env *environment, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("usage: lac pr <watch|status|list|unwatch> [pull request]")
	}

	switch arguments[0] {
	case "watch":
		return runPRWatch(ctx, env, arguments[1:])
	case "status":
		return runPRStatus(ctx, env, arguments[1:])
	case "list":
		return runPRList(ctx, env, arguments[1:])
	case "unwatch":
		return runPRUnwatch(ctx, env, arguments[1:])
	default:
		return fmt.Errorf("unknown pr command %q; try watch, status, list or unwatch", arguments[0])
	}
}

func runPRWatch(ctx context.Context, env *environment, arguments []string) error {
	if len(arguments) < 1 {
		return errors.New("usage: lac pr watch <owner/repository#number>")
	}

	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	watch, err := client.WatchPullRequest(ctx, arguments[0])
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, watch)
	}

	fmt.Fprintf(env.output, "watching\t%s\n", watch.Reference)
	printWatchSummary(env, watch)

	// Changes are delivered to the agent that subscribed. A command run from a shell registers an
	// identity for the length of the command and then lets it go, so say where the changes actually
	// end up rather than letting somebody wait for a message that is not coming.
	if env.identity.token == "" && env.identity.name == "" {
		fmt.Fprintf(env.output, "\n\tthis shell has no lasting identity, so changes are recorded "+
			"but not delivered anywhere\n")
		fmt.Fprintf(env.output, "\tsee them with: lac pr status %s\n", watch.Reference)
		fmt.Fprintf(env.output, "\tor register one first: lac --name me register --save\n")
	}

	return nil
}

func runPRStatus(ctx context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	var (
		refresh = flags.Bool("refresh", false, "read github now rather than reporting what is known")
		wait    = flags.Bool("wait", false, "wait for the next reading, for use just after pushing")
		threads = flags.Bool("threads", false, "show the review conversations in full")
	)
	if err := flags.Parse(arguments); err != nil {
		return err //nolint:wrapcheck // the flag package already printed the problem
	}
	if flags.NArg() < 1 {
		return errors.New("usage: lac pr status [flags] <owner/repository#number>")
	}

	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	detail, err := client.PullRequestStatus(ctx, flags.Arg(0), *refresh, *wait)
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, detail)
	}

	fmt.Fprintf(env.output, "%s\t%s\n", detail.Watch.Reference, detail.Watch.Title)
	printWatchSummary(env, detail.Watch)

	if len(detail.Checks) > 0 {
		fmt.Fprintln(env.output, "\nCHECK\tSTATE")
		for _, check := range detail.Checks {
			fmt.Fprintf(env.output, "%s\t%s\n", check.Name, check.State)
		}
	}

	unresolved := detail.UnresolvedThreads()
	if len(unresolved) > 0 {
		fmt.Fprintf(env.output, "\n%d review conversation(s) waiting on somebody:\n", len(unresolved))
		for _, thread := range unresolved {
			where := thread.Path
			if thread.Line > 0 {
				where = fmt.Sprintf("%s:%d", thread.Path, thread.Line)
			}

			fmt.Fprintf(env.output, "\n%s\t%s\n", where, firstLineOf(thread.Comments))
			if *threads {
				for _, comment := range thread.Comments {
					fmt.Fprintf(env.output, "\t%s: %s\n", comment.Author, comment.Body)
				}
			}
		}
	}

	if len(detail.Changes) > 0 {
		fmt.Fprintln(env.output, "\nRECENTLY\t")
		for index, change := range detail.Changes {
			if index >= 5 {
				break
			}
			fmt.Fprintf(env.output, "%s\t%s\n", shortTime(change.At), change.Summary)
		}
	}

	return nil
}

func runPRList(ctx context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("list", flag.ContinueOnError)
	// Everything by default: a watch belongs to the machine, and a one-shot command that registered
	// a passing identity of its own would otherwise see an empty list a moment after creating one.
	mine := flags.Bool("mine", false, "only the pull requests this agent subscribed to")
	if err := flags.Parse(arguments); err != nil {
		return err //nolint:wrapcheck // the flag package already printed the problem
	}

	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	watches, err := client.WatchedPullRequests(ctx, !*mine)
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, watches)
	}

	if len(watches) == 0 {
		fmt.Fprintln(env.output, "nothing is being watched\n\n"+
			"  watch one with: lac pr watch owner/repository#number")

		return nil
	}

	fmt.Fprintln(env.output, "PULL REQUEST\tSTATE\tCI\tTHREADS\tSEEN\tTITLE")
	for _, watch := range watches {
		state := watch.State
		if watch.Draft {
			state += " (draft)"
		}

		seen := shortTime(watch.ObservedAt)
		if watch.Stale {
			seen += " (stale)"
		}

		fmt.Fprintf(env.output, "%s\t%s\t%s\t%d\t%s\t%s\n",
			watch.Reference, state, watch.Checks, watch.Unresolved, seen, watch.Title)
	}

	return nil
}

func runPRUnwatch(ctx context.Context, env *environment, arguments []string) error {
	if len(arguments) < 1 {
		return errors.New("usage: lac pr unwatch <owner/repository#number>")
	}

	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	if err := client.UnwatchPullRequest(ctx, arguments[0]); err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, map[string]any{"watching": false, "pull_request": arguments[0]})
	}

	fmt.Fprintf(env.output, "stopped watching\t%s\n", arguments[0])

	return nil
}

// printWatchSummary is the line an operator actually reads: where it is, whether CI is happy, how
// much review is outstanding, and whether any of it can be trusted.
func printWatchSummary(env *environment, watch lacclient.Watch) {
	state := watch.State
	if watch.Draft {
		state += " (draft)"
	}

	fmt.Fprintf(env.output, "state\t%s\n", state)
	fmt.Fprintf(env.output, "ci\t%s\n", watch.Checks)

	if watch.Unresolved > 0 {
		fmt.Fprintf(env.output, "review\t%d conversation(s) waiting on somebody\n", watch.Unresolved)
	}

	if watch.Stale {
		seen := watch.ObservedAt
		if seen == "" {
			seen = "never"
		} else {
			seen = shortTime(seen)
		}

		fmt.Fprintf(env.output, "stale\tlast read from github at %s\n", seen)
		if watch.LastError != "" {
			fmt.Fprintf(env.output, "\t%s\n", watch.LastError)
		}
	}
}

func firstLineOf(comments []lacclient.PullRequestComment) string {
	if len(comments) == 0 {
		return ""
	}

	body := strings.TrimSpace(comments[0].Body)
	if line, _, found := strings.Cut(body, "\n"); found {
		return line
	}

	return body
}
