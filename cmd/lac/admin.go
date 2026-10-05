package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"sadeq.uk/lac/pkg/lacclient"
)

// runStatus is the operator's overview: every resource with who holds it and who is waiting, and
// every agent with how much it holds and waits for. It is everything needed to decide what to
// release, cancel or evict, on one screen.
func runStatus(ctx context.Context, env *environment, _ []string) error {
	client, release, _, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	resources, err := client.Resources(ctx)
	if err != nil {
		return err
	}

	statuses := make([]lacclient.QueueStatus, 0, len(resources))
	for _, resource := range resources {
		status, err := client.Queue(ctx, resource.Name)
		if err != nil {
			return err
		}
		statuses = append(statuses, status)
	}

	agents, err := client.Agents(ctx)
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, map[string]any{"resources": statuses, "agents": agents})
	}

	holds := map[string]int{}
	waits := map[string]int{}

	fmt.Fprintln(env.output, "RESOURCE\tHELD\tWAITING\tDESCRIPTION")
	for _, status := range statuses {
		fmt.Fprintf(env.output, "%s\t%d/%d\t%d\t%s\n", status.Resource.Name, status.Resource.Held,
			status.Resource.Capacity, status.Resource.Waiting, status.Resource.Description)
		for _, holder := range status.Holders {
			holds[holder.AgentID]++
		}
		for _, entry := range status.Waiting {
			waits[entry.AgentID]++
		}
	}

	if len(holds) > 0 {
		fmt.Fprintln(env.output, "\nHOLDING\tLEASE\tAGENT\tSINCE\tEXPIRES\tREASON")
		for _, status := range statuses {
			for _, holder := range status.Holders {
				fmt.Fprintf(env.output, "%s\t%s\t%s\t%s\t%s\t%s\n", holder.Resource, holder.ID, holder.AgentName,
					shortTime(holder.AcquiredAt), shortTime(holder.ExpiresAt), holder.Reason)
			}
		}
	}

	if len(waits) > 0 {
		fmt.Fprintln(env.output, "\nWAITING\tPOSITION\tENTRY\tAGENT\tSINCE\tREASON")
		for _, status := range statuses {
			for _, entry := range status.Waiting {
				fmt.Fprintf(env.output, "%s\t%d\t%s\t%s\t%s\t%s\n", entry.Resource, entry.Position, entry.ID,
					entry.AgentName, shortTime(entry.RequestedAt), entry.Reason)
			}
		}
	}

	fmt.Fprintln(env.output, "\nAGENT\tKIND\tHOLDS\tWAITS\tLAST SEEN\tWORKDIR")
	for _, agent := range agents {
		fmt.Fprintf(env.output, "%s\t%s\t%d\t%d\t%s\t%s\n", agent.Name, agent.Kind, holds[agent.ID],
			waits[agent.ID], shortTime(agent.LastHeartbeatAt), agent.Workdir)
	}

	return nil
}

// runCancel withdraws queued requests: your own by entry id, another agent's with --force, or every
// request waiting for a resource with --resource.
func runCancel(ctx context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("cancel", flag.ContinueOnError)
	var (
		force    = flags.Bool("force", false, "withdraw another agent's request (operators only)")
		resource = flags.String("resource", "", "withdraw every request waiting for this resource (operators only)")
	)
	if err := parseAnywhere(flags, arguments); err != nil {
		return err
	}
	if (flags.NArg() == 1) == (*resource != "") || flags.NArg() > 1 {
		return errors.New("usage: lac cancel [--force] <entry-id> or lac cancel --resource <name>")
	}

	// Acting on other agents' requests is an operator power, granted against the operator's name.
	if *force || *resource != "" {
		env.identity.personal = true
	}

	client, release, _, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	if *resource != "" {
		cancelled, err := client.ClearQueue(ctx, *resource)
		if err != nil {
			return err
		}
		if env.asJSON {
			return writeJSON(env, map[string]any{"cancelled": cancelled})
		}
		fmt.Fprintf(env.output, "cancelled\t%d request(s) waiting for %s\n", cancelled, *resource)

		return nil
	}

	entryID := flags.Arg(0)
	cancelEntry := client.CancelQueueEntry
	if *force {
		cancelEntry = client.ForceCancelQueueEntry
	}
	if err := cancelEntry(ctx, entryID); err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, map[string]any{"cancelled": true, "entry_id": entryID})
	}
	fmt.Fprintf(env.output, "cancelled\t%s\n", entryID)

	return nil
}

// runEvict retires another agent: what it holds goes back, its token stops working and it leaves
// the roster. A process it started keeps running; only its standing with the daemon ends.
func runEvict(ctx context.Context, env *environment, arguments []string) error {
	if len(arguments) != 1 {
		return errors.New("usage: lac evict <agent>")
	}

	client, release, _, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	released, err := client.Evict(ctx, arguments[0])
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, map[string]any{"evicted": arguments[0], "released": released})
	}
	fmt.Fprintf(env.output, "evicted\t%s, %d slot(s) released\n", arguments[0], released)

	return nil
}
