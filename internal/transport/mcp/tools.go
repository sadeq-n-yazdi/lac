package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"sadeq.uk/lac/pkg/lacclient"
)

// toolHandler runs one tool. It never returns an error: a failure the model should read about
// comes back as an error result, because a protocol error never reaches the model at all.
type toolHandler func(ctx context.Context, server *Server, client *lacclient.Client, arguments json.RawMessage) callToolResult

// toolDefinitions describes the tools to the model.
//
// The descriptions are written for an AI reader deciding whether to call something, not for a
// developer reading an API reference: they say when to use the tool and what happens if you skip
// it, because that is the decision the model is actually making.
func toolDefinitions() []tool {
	return []tool{
		{
			Name:  "lac_agents",
			Title: "Who else is working",
			Description: "List the other AI agents working on this machine right now, with the " +
				"directory each is in. Call this before starting on an area of the codebase, so you " +
				"do not duplicate or undo somebody else's work, and whenever the user asks what " +
				"everyone is doing.",
			InputSchema: object(nil, nil),
		},
		{
			Name:  "lac_resources",
			Title: "Shared resources and how busy they are",
			Description: "List the scarce things on this machine — concurrent test runs, a shared " +
				"reviewer session — with how many slots are in use and how many agents are waiting. " +
				"Call this to find out what you must queue for before running heavy work.",
			InputSchema: object(nil, nil),
		},
		{
			Name:  "lac_acquire_slot",
			Title: "Wait for a slot before doing heavy work",
			Description: "Reserve a slot on a shared resource and wait your turn. Call this BEFORE " +
				"running a test suite, a build, or anything else the machine can only do a few of at " +
				"once — starting without a slot is what makes the machine thrash and everybody's " +
				"work slower. The call blocks until the slot is yours. Hold it while you work, then " +
				"call lac_release_slot. If it times out, that is not a failure: call it again.",
			InputSchema: object(map[string]any{
				"resource": property("string",
					"The resource to queue for, such as \"test\". Use lac_resources to see what exists."),
				"reason": property("string",
					"What you are about to do, shown to the operator in the queue. For example \"running the parser tests\"."),
				"priority": property("integer",
					"Higher goes first. Leave unset for normal work; 10 is for something the user is waiting on."),
				"no_wait": property("boolean",
					"Return immediately if the resource is full instead of queueing. Use this only when you have something else useful to do."),
			}, []string{"resource"}),
		},
		{
			Name:  "lac_release_slot",
			Title: "Give a slot back",
			Description: "Release a slot you were granted, as soon as the work is finished — even " +
				"if it failed. Another agent is waiting for it. If you lost the lease id, call " +
				"lac_my_slots.",
			InputSchema: object(map[string]any{
				"lease_id": property("string", "The lease id lac_acquire_slot gave you."),
			}, []string{"lease_id"}),
		},
		{
			Name:  "lac_my_slots",
			Title: "Slots you are holding",
			Description: "List the slots you currently hold, with their lease ids and when they " +
				"expire. Use it to find a lease id you need to release, or to check you are not " +
				"holding something you have finished with.",
			InputSchema: object(nil, nil),
		},
		{
			Name:  "lac_queue",
			Title: "Who is waiting for a resource",
			Description: "Show who holds a resource and who is queued for it, in the order they " +
				"will be served. Useful for telling the user why something is waiting.",
			InputSchema: object(map[string]any{
				"resource": property("string", "The resource to inspect, such as \"test\"."),
			}, []string{"resource"}),
		},
		{
			Name:  "lac_send_message",
			Title: "Tell another agent something",
			Description: "Send a message to one agent by name, or to every agent at once. Use it to " +
				"say what you are taking on before you start, to answer a question, or to warn " +
				"others about something you changed. Messages wait for the recipient, so it is safe " +
				"to send to an agent that is busy.",
			InputSchema: object(map[string]any{
				"to": property("string",
					"The agent to tell, by name from lac_agents. Leave unset and set broadcast to reach everyone."),
				"broadcast": property("boolean",
					"Send to every agent instead of one. Use sparingly: it interrupts everybody."),
				"kind": property("string",
					"A short label for what this is: \"status\", \"question\", \"answer\", \"warning\"."),
				"text": property("string", "What you want to say, in plain language."),
			}, []string{"kind", "text"}),
		},
		{
			Name:  "lac_ask_worker",
			Title: "Ask a shared worker to run something for you",
			Description: "Hand a job to the machine's shared worker instead of running it yourself. " +
				"Use it when you want the work done but do not need to watch it: the worker queues " +
				"for its own slot, runs the job in your working directory, and gives you the output " +
				"and exit status. Call lac_worker_commands first to see what can be asked for — you " +
				"name a configured job, you cannot supply a command line.",
			InputSchema: object(map[string]any{
				"command": property("string",
					"The job to ask for, from lac_worker_commands. For example \"test\"."),
				"wait": property("boolean",
					"Wait for the result. True by default. Set false to get a task id back at once and follow it with lac_task."),
			}, []string{"command"}),
		},
		{
			Name:  "lac_worker_commands",
			Title: "What a shared worker can be asked to do",
			Description: "List the jobs this machine's workers will run, with what each one actually " +
				"executes. The operator decides these; you can only name one.",
			InputSchema: object(nil, nil),
		},
		{
			Name:  "lac_task",
			Title: "Follow a job you asked for",
			Description: "Show the state and output of a job you handed to a worker, optionally " +
				"waiting for it to finish.",
			InputSchema: object(map[string]any{
				"task_id": property("string", "The task id lac_ask_worker gave you."),
				"wait":    property("boolean", "Wait until it finishes rather than reporting where it has got to."),
			}, []string{"task_id"}),
		},
		{
			Name:  "lac_watch_pr",
			Title: "Watch a pull request",
			Description: "Follow a pull request and be told when something happens to it: CI " +
				"finishing, a review arriving, a conversation being resolved, it being merged. Use " +
				"it after opening a pull request so you learn that CI failed without sitting and " +
				"asking. The updates arrive in lac_inbox.",
			InputSchema: object(map[string]any{
				"pull_request": property("string",
					"The pull request, as owner/repository#number or a github.com URL."),
			}, []string{"pull_request"}),
		},
		{
			Name:  "lac_pr_status",
			Title: "Where a pull request has got to",
			Description: "Show a watched pull request: its state, each CI check, the review " +
				"conversations still waiting on somebody, and what changed recently. Use it to find " +
				"out what is left to do before a pull request can land. If the answer is marked " +
				"stale, GitHub could not be reached and you are seeing the last thing that was true.",
			InputSchema: object(map[string]any{
				"pull_request": property("string", "The pull request, as owner/repository#number."),
				"refresh": property("boolean",
					"Read GitHub now rather than reporting what is already known. Use it right after pushing."),
			}, []string{"pull_request"}),
		},
		{
			Name:  "lac_report",
			Title: "Answer the operator's report request",
			Description: "Answer a request from the operator asking what you are working on. You " +
				"will find the request in lac_inbox as a message of kind \"report-request\", carrying " +
				"a request_id. Answer promptly and in one or two sentences: the operator is waiting, " +
				"and an agent that does not answer is listed as silent.",
			InputSchema: object(map[string]any{
				"request_id": property("string", "The request_id from the report-request message."),
				"text": property("string",
					"What you are working on right now, in a sentence or two. Say what you are changing and roughly how far along you are."),
			}, []string{"request_id", "text"}),
		},
		{
			Name:  "lac_inbox",
			Title: "Read messages sent to you",
			Description: "Read the messages other agents have sent you. Call this when you start " +
				"work and between tasks: another agent may have told you they are already changing " +
				"the file you were about to touch. Messages come back until you acknowledge them, " +
				"which happens by default.",
			InputSchema: object(map[string]any{
				"keep_unread": property("boolean",
					"Leave the messages in the inbox instead of acknowledging them, so they appear again next time."),
			}, nil),
		},
	}
}

func toolHandlers() map[string]toolHandler {
	return map[string]toolHandler{
		"lac_agents":          handleAgents,
		"lac_resources":       handleResources,
		"lac_acquire_slot":    handleAcquireSlot,
		"lac_release_slot":    handleReleaseSlot,
		"lac_my_slots":        handleMySlots,
		"lac_queue":           handleQueue,
		"lac_send_message":    handleSendMessage,
		"lac_inbox":           handleInbox,
		"lac_report":          handleReport,
		"lac_ask_worker":      handleAskWorker,
		"lac_worker_commands": handleWorkerCommands,
		"lac_task":            handleTask,
		"lac_watch_pr":        handleWatchPullRequest,
		"lac_pr_status":       handlePullRequestStatus,
	}
}

func handleAgents(ctx context.Context, server *Server, client *lacclient.Client, _ json.RawMessage) callToolResult {
	agents, err := client.Agents(ctx)
	if err != nil {
		return errorResult("Could not read the agent roster: %v", err)
	}

	others := make([]lacclient.Agent, 0, len(agents))
	for _, agent := range agents {
		if agent.ID != server.agent.ID {
			others = append(others, agent)
		}
	}

	if len(others) == 0 {
		return textResult("No other agents are working on this machine right now. " +
			"You are registered as \"" + server.agent.Name + "\".")
	}

	var report strings.Builder
	fmt.Fprintf(&report, "%d other agent(s) working right now (you are %q):\n\n",
		len(others), server.agent.Name)

	for _, agent := range others {
		fmt.Fprintf(&report, "- %s (%s) in %s\n", agent.Name, agent.Kind, agent.Workdir)
	}

	return textResult(report.String())
}

func handleResources(ctx context.Context, _ *Server, client *lacclient.Client, _ json.RawMessage) callToolResult {
	resources, err := client.Resources(ctx)
	if err != nil {
		return errorResult("Could not read the resources: %v", err)
	}
	if len(resources) == 0 {
		return textResult("No shared resources are defined on this machine, so nothing needs " +
			"queueing for. The operator defines them in ~/.config/lac/config.yaml.")
	}

	var report strings.Builder
	report.WriteString("Shared resources on this machine:\n\n")

	for _, item := range resources {
		fmt.Fprintf(&report, "- %s: %d of %d slots in use, %d waiting",
			item.Name, item.Held, item.Capacity, item.Waiting)
		if item.Description != "" {
			fmt.Fprintf(&report, " — %s", item.Description)
		}
		report.WriteString("\n")
	}

	report.WriteString("\nAcquire a slot with lac_acquire_slot before running work that needs one.")

	return textResult(report.String())
}

// acquireArguments is what the model asks for when it wants a slot.
type acquireArguments struct {
	Resource string `json:"resource"`
	Reason   string `json:"reason"`
	Priority int    `json:"priority"`
	NoWait   bool   `json:"no_wait"`
}

func handleAcquireSlot(
	ctx context.Context, server *Server, client *lacclient.Client, arguments json.RawMessage,
) callToolResult {
	var request acquireArguments
	if err := decode(arguments, &request); err != nil {
		return errorResult("%v", err)
	}
	if request.Resource == "" {
		return errorResult("Which resource? Give resource, for example \"test\". " +
			"lac_resources lists what exists.")
	}

	lease, err := client.Acquire(ctx, lacclient.AcquireRequest{
		Resource: request.Resource,
		Reason:   request.Reason,
		Priority: request.Priority,
		NoWait:   request.NoWait,
		Timeout:  server.options.AcquireTimeout,
	})

	switch {
	case lacclient.IsCapacityReached(err):
		return textResult(fmt.Sprintf(
			"%q is fully in use right now and you asked not to wait. Either do something else and "+
				"try again, or call this again without no_wait to take your place in the queue.",
			request.Resource))

	case err != nil && isTimeout(err):
		return textResult(fmt.Sprintf(
			"Still waiting for a slot on %q after %s — other agents are ahead of you. This is "+
				"normal. Call lac_acquire_slot again to keep waiting, or lac_queue to see the line.",
			request.Resource, server.options.AcquireTimeout))

	case err != nil:
		return errorResult("Could not get a slot on %q: %v", request.Resource, err)
	}

	return textResult(fmt.Sprintf(
		"You have a slot on %q. Lease id: %s (expires %s; it is renewed for you while this session "+
			"lives). Do the work now, then call lac_release_slot with that lease id — even if the "+
			"work fails.",
		lease.Resource, lease.ID, lease.ExpiresAt))
}

type leaseArguments struct {
	LeaseID string `json:"lease_id"`
}

func handleReleaseSlot(
	ctx context.Context, _ *Server, client *lacclient.Client, arguments json.RawMessage,
) callToolResult {
	var request leaseArguments
	if err := decode(arguments, &request); err != nil {
		return errorResult("%v", err)
	}
	if request.LeaseID == "" {
		return errorResult("Which slot? Give lease_id. lac_my_slots lists the ones you hold.")
	}

	if err := client.Release(ctx, request.LeaseID); err != nil {
		return errorResult("Could not release %s: %v", request.LeaseID, err)
	}

	return textResult("Slot released. Whoever was next in the queue can start.")
}

func handleMySlots(ctx context.Context, _ *Server, client *lacclient.Client, _ json.RawMessage) callToolResult {
	leases, err := client.Held(ctx)
	if err != nil {
		return errorResult("Could not read your slots: %v", err)
	}
	if len(leases) == 0 {
		return textResult("You are not holding any slots.")
	}

	var report strings.Builder
	report.WriteString("You are holding:\n\n")

	for _, lease := range leases {
		fmt.Fprintf(&report, "- %s on %q, expires %s\n", lease.ID, lease.Resource, lease.ExpiresAt)
	}

	report.WriteString("\nRelease each one with lac_release_slot as soon as its work is done.")

	return textResult(report.String())
}

type queueArguments struct {
	Resource string `json:"resource"`
}

func handleQueue(ctx context.Context, _ *Server, client *lacclient.Client, arguments json.RawMessage) callToolResult {
	var request queueArguments
	if err := decode(arguments, &request); err != nil {
		return errorResult("%v", err)
	}
	if request.Resource == "" {
		return errorResult("Which resource? Give resource, for example \"test\".")
	}

	status, err := client.Queue(ctx, request.Resource)
	if err != nil {
		return errorResult("Could not read the queue for %q: %v", request.Resource, err)
	}

	var report strings.Builder
	fmt.Fprintf(&report, "%s: %d of %d slots in use, %d waiting.\n",
		status.Resource.Name, status.Resource.Held, status.Resource.Capacity, len(status.Waiting))

	for _, entry := range status.Waiting {
		fmt.Fprintf(&report, "\n%d. %s", entry.Position, entry.AgentName)
		if entry.Reason != "" {
			fmt.Fprintf(&report, " — %s", entry.Reason)
		}
	}

	return textResult(report.String())
}

type sendArguments struct {
	To        string `json:"to"`
	Broadcast bool   `json:"broadcast"`
	Kind      string `json:"kind"`
	Text      string `json:"text"`
}

func handleSendMessage(
	ctx context.Context, _ *Server, client *lacclient.Client, arguments json.RawMessage,
) callToolResult {
	var request sendArguments
	if err := decode(arguments, &request); err != nil {
		return errorResult("%v", err)
	}
	if request.Text == "" {
		return errorResult("Nothing to send: give text.")
	}
	if request.Kind == "" {
		request.Kind = "status"
	}
	if request.To == "" && !request.Broadcast {
		return errorResult("Who should hear this? Give to with an agent name from lac_agents, " +
			"or set broadcast to reach everyone.")
	}

	body := map[string]string{"text": request.Text}

	var (
		sent lacclient.Sent
		err  error
	)
	if request.Broadcast {
		sent, err = client.SendToTopic(ctx, "all", request.Kind, body)
	} else {
		sent, err = client.SendTo(ctx, request.To, request.Kind, body)
	}

	switch {
	case err != nil && lacclient.ErrorCode(err) == lacclient.CodeNotFound:
		return errorResult("Nobody is listening: %v. Use lac_agents to see who is here.", err)
	case err != nil:
		return errorResult("Could not send the message: %v", err)
	}

	if sent.Recipients == 1 {
		return textResult("Sent. It is waiting for them whether or not they are looking right now.")
	}

	return textResult(fmt.Sprintf("Sent to %d agents.", sent.Recipients))
}

type askArguments struct {
	Command string `json:"command"`
	Wait    *bool  `json:"wait"`
}

func handleAskWorker(
	ctx context.Context, _ *Server, client *lacclient.Client, arguments json.RawMessage,
) callToolResult {
	var request askArguments
	if err := decode(arguments, &request); err != nil {
		return errorResult("%v", err)
	}
	if request.Command == "" {
		return errorResult("Which job? Give command. lac_worker_commands lists what can be asked for.")
	}

	wait := request.Wait == nil || *request.Wait

	task, err := client.SubmitTask(ctx, request.Command, "", wait, 0)
	switch {
	case err != nil && lacclient.ErrorCode(err) == lacclient.CodeNotFound:
		return errorResult("%v. Call lac_worker_commands to see what this machine offers; you name a "+
			"configured job rather than a command line.", err)
	case err != nil:
		return errorResult("Could not ask for %q: %v", request.Command, err)
	}

	if !task.Finished() {
		return textResult(fmt.Sprintf(
			"Queued as %s. Nobody may have picked it up yet — a worker has to be running for this "+
				"resource. Follow it with lac_task(task_id: %q).", task.ID, task.ID))
	}

	return textResult(describeTask(task))
}

func describeTask(task lacclient.Task) string {
	var report strings.Builder

	fmt.Fprintf(&report, "%s: %s", task.Command, task.State)
	if task.Worker != "" {
		fmt.Fprintf(&report, ", run by %s", task.Worker)
	}
	if task.State == "failed" {
		fmt.Fprintf(&report, " (exit status %d)", task.ExitCode)
	}
	if task.Failure != "" {
		fmt.Fprintf(&report, "\n%s", task.Failure)
	}
	if task.Output != "" {
		fmt.Fprintf(&report, "\n\n%s", task.Output)
	}

	return report.String()
}

func handleWorkerCommands(
	ctx context.Context, _ *Server, client *lacclient.Client, _ json.RawMessage,
) callToolResult {
	commands, err := client.Commands(ctx)
	if err != nil {
		return errorResult("Could not read the commands: %v", err)
	}
	if len(commands) == 0 {
		return textResult("No jobs are configured for workers on this machine, so there is nothing " +
			"to hand off. Do the work yourself, taking a slot with lac_acquire_slot first.")
	}

	var report strings.Builder
	report.WriteString("Jobs a shared worker will run for you:\n")

	for _, command := range commands {
		fmt.Fprintf(&report, "\n- %s: runs `%s` on the %q resource",
			command.Key, strings.Join(command.Run, " "), command.Resource)
		if command.Description != "" {
			fmt.Fprintf(&report, " — %s", command.Description)
		}
	}

	report.WriteString("\n\nAsk for one with lac_ask_worker. It runs in your working directory.")

	return textResult(report.String())
}

type taskArguments struct {
	TaskID string `json:"task_id"`
	Wait   bool   `json:"wait"`
}

func handleTask(ctx context.Context, _ *Server, client *lacclient.Client, arguments json.RawMessage) callToolResult {
	var request taskArguments
	if err := decode(arguments, &request); err != nil {
		return errorResult("%v", err)
	}
	if request.TaskID == "" {
		return errorResult("Which job? Give task_id, the one lac_ask_worker gave you.")
	}

	task, err := client.TaskStatus(ctx, request.TaskID, request.Wait)
	if err != nil {
		return errorResult("Could not read %s: %v", request.TaskID, err)
	}

	return textResult(describeTask(task))
}

type pullRequestArguments struct {
	PullRequest string `json:"pull_request"`
	Refresh     bool   `json:"refresh"`
}

func handleWatchPullRequest(
	ctx context.Context, _ *Server, client *lacclient.Client, arguments json.RawMessage,
) callToolResult {
	var request pullRequestArguments
	if err := decode(arguments, &request); err != nil {
		return errorResult("%v", err)
	}
	if request.PullRequest == "" {
		return errorResult("Which pull request? Give pull_request as owner/repository#number.")
	}

	watch, err := client.WatchPullRequest(ctx, request.PullRequest)
	if err != nil {
		return errorResult("Could not watch %s: %v", request.PullRequest, err)
	}

	return textResult(fmt.Sprintf(
		"Watching %s — %s.\n%s\n\nChanges will arrive in lac_inbox: CI results, reviews, "+
			"comments, and whether it is merged.",
		watch.Reference, watch.Title, describeWatch(watch)))
}

func handlePullRequestStatus(
	ctx context.Context, _ *Server, client *lacclient.Client, arguments json.RawMessage,
) callToolResult {
	var request pullRequestArguments
	if err := decode(arguments, &request); err != nil {
		return errorResult("%v", err)
	}
	if request.PullRequest == "" {
		return errorResult("Which pull request? Give pull_request as owner/repository#number.")
	}

	detail, err := client.PullRequestStatus(ctx, request.PullRequest, request.Refresh, false)
	if err != nil {
		if lacclient.ErrorCode(err) == lacclient.CodeNotFound {
			return errorResult("%s is not being watched. Start with lac_watch_pr.", request.PullRequest)
		}

		return errorResult("Could not read %s: %v", request.PullRequest, err)
	}

	var report strings.Builder
	fmt.Fprintf(&report, "%s — %s\n%s", detail.Watch.Reference, detail.Watch.Title, describeWatch(detail.Watch))

	if failing := failingChecks(detail.Checks); len(failing) > 0 {
		fmt.Fprintf(&report, "\n\nFailing checks: %s", strings.Join(failing, ", "))
	}

	unresolved := detail.UnresolvedThreads()
	if len(unresolved) > 0 {
		fmt.Fprintf(&report, "\n\n%d review conversation(s) waiting on somebody:", len(unresolved))
		for _, thread := range unresolved {
			where := thread.Path
			if thread.Line > 0 {
				where = fmt.Sprintf("%s:%d", thread.Path, thread.Line)
			}
			fmt.Fprintf(&report, "\n- %s: %s", where, firstCommentOf(thread))
		}
		report.WriteString("\n\nDeal with these before asking for the pull request to be merged.")
	}

	if len(detail.Changes) > 0 {
		report.WriteString("\n\nRecently:")
		for index, change := range detail.Changes {
			if index >= 5 {
				break
			}
			fmt.Fprintf(&report, "\n- %s", change.Summary)
		}
	}

	return textResult(report.String())
}

// describeWatch is the line that matters: where it is, whether CI is happy, and whether any of it
// can still be trusted.
func describeWatch(watch lacclient.Watch) string {
	state := watch.State
	if watch.Draft {
		state += " (draft)"
	}

	description := fmt.Sprintf("State: %s. CI: %s. Unresolved review conversations: %d.",
		state, watch.Checks, watch.Unresolved)

	if watch.Stale {
		description += " This is the last thing that was true, not the current state: GitHub could " +
			"not be reached."
		if watch.LastError != "" {
			description += " (" + watch.LastError + ")"
		}
	}

	return description
}

func failingChecks(checks []lacclient.PullRequestCheck) []string {
	failing := make([]string, 0, len(checks))
	for _, check := range checks {
		switch check.State {
		case "failure", "cancelled", "timed_out", "action_required", "startup_failure", "stale":
			failing = append(failing, check.Name)
		}
	}

	return failing
}

func firstCommentOf(thread lacclient.ReviewThread) string {
	if len(thread.Comments) == 0 {
		return ""
	}

	body := strings.TrimSpace(thread.Comments[0].Body)
	if line, _, found := strings.Cut(body, "\n"); found {
		return line
	}

	return body
}

type reportArguments struct {
	RequestID string `json:"request_id"`
	Text      string `json:"text"`
}

func handleReport(
	ctx context.Context, _ *Server, client *lacclient.Client, arguments json.RawMessage,
) callToolResult {
	var request reportArguments
	if err := decode(arguments, &request); err != nil {
		return errorResult("%v", err)
	}
	if request.RequestID == "" {
		return errorResult("Which request? Give request_id, from the report-request message in " +
			"lac_inbox.")
	}
	if request.Text == "" {
		return errorResult("Nothing to report: give text saying what you are working on.")
	}

	if err := client.SubmitReport(ctx, request.RequestID, request.Text); err != nil {
		return errorResult("Could not send your report: %v", err)
	}

	return textResult("Reported. The operator can see it.")
}

type inboxArguments struct {
	KeepUnread bool `json:"keep_unread"`
}

func handleInbox(ctx context.Context, _ *Server, client *lacclient.Client, arguments json.RawMessage) callToolResult {
	var request inboxArguments
	if err := decode(arguments, &request); err != nil {
		return errorResult("%v", err)
	}

	messages, err := client.Inbox(ctx, 0)
	if err != nil {
		return errorResult("Could not read your inbox: %v", err)
	}
	if len(messages) == 0 {
		return textResult("Nothing waiting for you.")
	}

	var report strings.Builder
	fmt.Fprintf(&report, "%d message(s):\n", len(messages))

	identifiers := make([]string, 0, len(messages))
	for _, message := range messages {
		identifiers = append(identifiers, message.ID)

		sender := message.FromName
		if sender == "" {
			sender = message.From
		}

		if message.Kind == "report-request" {
			fmt.Fprintf(&report, "\n%s", reportRequestFrom(sender, message.Body))
			continue
		}

		fmt.Fprintf(&report, "\nFrom %s (%s): %s", sender, message.Kind, textOf(message.Body))
	}

	if !request.KeepUnread {
		if _, err := client.Acknowledge(ctx, identifiers...); err != nil {
			report.WriteString("\n\n(These could not be marked as read, so they will appear again.)")
		}
	} else {
		report.WriteString("\n\n(Left unread; they will appear again next time.)")
	}

	return textResult(report.String())
}

// reportRequestFrom renders a report request as an instruction rather than a puzzle: the model
// needs the question and the request id together, or it cannot answer.
func reportRequestFrom(sender string, body json.RawMessage) string {
	var shaped struct {
		RequestID string `json:"request_id"`
		Question  string `json:"question"`
	}

	if err := json.Unmarshal(body, &shaped); err != nil || shaped.RequestID == "" {
		return fmt.Sprintf("From %s (report-request): %s", sender, string(body))
	}

	return fmt.Sprintf("The operator asks: %s\n  Answer now with lac_report(request_id: %q, text: ...)",
		shaped.Question, shaped.RequestID)
}

// textOf pulls the readable part out of a message body, falling back to the raw JSON for a message
// that carries something more structured.
func textOf(body json.RawMessage) string {
	var shaped struct {
		Text string `json:"text"`
	}

	if err := json.Unmarshal(body, &shaped); err == nil && shaped.Text != "" {
		return shaped.Text
	}

	return string(body)
}

func decode(arguments json.RawMessage, target any) error {
	if len(arguments) == 0 {
		return nil
	}

	if err := json.Unmarshal(arguments, target); err != nil {
		return fmt.Errorf("those arguments could not be read: %v", err) //nolint:errorlint // shown to a model, not wrapped
	}

	return nil
}

func isTimeout(err error) bool {
	return strings.Contains(err.Error(), context.DeadlineExceeded.Error())
}

// object builds a JSON Schema object for a tool's arguments.
func object(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}

	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}

	return schema
}

func property(kind, description string) map[string]any {
	return map[string]any{"type": kind, "description": description}
}

// identity decides how this session appears to the other agents.
func (s *Server) identity() (name, workdir string, err error) {
	workdir = s.options.Workdir
	if workdir == "" {
		if workdir, err = currentDirectory(); err != nil {
			return "", "", err
		}
	}

	name = s.options.AgentName
	if name == "" {
		name = deriveName(workdir, s.options.AgentKind)
	}

	return name, workdir, nil
}

// resourceDefinitions are the read-only views a client can fetch without calling a tool.
func resourceDefinitions() []resource {
	return []resource{
		{
			URI:         "lac://agents",
			Name:        "agents",
			Title:       "Agents working on this machine",
			Description: "Who is registered right now, and where each of them is working.",
			MIMEType:    "application/json",
		},
		{
			URI:         "lac://resources",
			Name:        "resources",
			Title:       "Shared resources",
			Description: "The scarce things on this machine and how busy each one is.",
			MIMEType:    "application/json",
		},
	}
}

func readResource(ctx context.Context, client *lacclient.Client, uri string) (string, error) {
	var value any

	switch uri {
	case "lac://agents":
		agents, err := client.Agents(ctx)
		if err != nil {
			return "", fmt.Errorf("reading the agent roster: %w", err)
		}
		value = agents

	case "lac://resources":
		resources, err := client.Resources(ctx)
		if err != nil {
			return "", fmt.Errorf("reading the resources: %w", err)
		}
		value = resources

	default:
		return "", fmt.Errorf("unknown resource %q", uri)
	}

	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encoding %s: %w", uri, err)
	}

	return string(encoded), nil
}
