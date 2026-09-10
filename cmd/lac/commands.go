package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"code.sadeq.uk/lac/internal/version"
	"code.sadeq.uk/lac/pkg/lacclient"
)

func commands() []command {
	return []command{
		{name: "version", summary: "print the version", run: runVersion},
		{name: "info", summary: "what the daemon is and what it can do", run: runInfo},
		{name: "register", summary: "register this shell as an agent and print its token", run: runRegister},
		{name: "agents", summary: "who is connected right now", run: runAgents},
		{name: "send", summary: "send a message to an agent or a topic", run: runSend},
		{name: "inbox", summary: "read the messages waiting for you", run: runInbox},
		{name: "resources", summary: "the shared resources on this machine", run: runResources},
		{name: "define", summary: "create or reconfigure a resource (operators only)", run: runDefine},
		{name: "queue", summary: "who is waiting for a resource", run: runQueue},
		{name: "acquire", summary: "take a slot and hold it until released", run: runAcquire},
		{name: "release", summary: "give a slot back", run: runRelease},
		{name: "held", summary: "the slots you are holding", run: runHeld},
		{name: "run", summary: "wait for a slot, then run a command", run: runRun},
		{name: "ask", summary: "ask a shared worker to run something for you", run: runAsk},
		{name: "commands", summary: "what a shared worker can be asked to do", run: runCommands},
		{name: "task", summary: "show one task, optionally waiting for it", run: runTaskStatus},
		{name: "tasks", summary: "the work you have asked for", run: runTasks},
		{name: "worker", summary: "become a shared worker for a resource", run: runWorker},
		{name: "mcp", summary: "serve LAC over MCP on stdin and stdout, for AI tools", run: runMCP},
		{name: "skill", summary: "install the LAC skill for AI tools that read skills", run: runSkill},
		{name: "report", summary: "ask every agent what it is doing", run: runReport},
		{name: "answer", summary: "answer a report request", run: runAnswer},
		{name: "deregister", summary: "retire this agent and give back its slots", run: runDeregister},
	}
}

func runVersion(_ context.Context, env *environment, _ []string) error {
	if env.asJSON {
		return writeJSON(env, map[string]string{
			"version": version.Version, "commit": version.Commit, "build_date": version.BuildDate,
		})
	}

	fmt.Fprintf(env.output, "lac %s\n", version.String())

	return nil
}

func runInfo(ctx context.Context, env *environment, _ []string) error {
	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	info, err := client.Info(ctx)
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, info)
	}

	fmt.Fprintf(env.output, "version\t%s\n", info.Version)
	fmt.Fprintf(env.output, "protocol\t%s\n", info.Protocol)
	fmt.Fprintf(env.output, "started\t%s\n", info.StartedAt)
	fmt.Fprintf(env.output, "uptime\t%s\n", (time.Duration(info.UptimeSeconds) * time.Second).String())
	fmt.Fprintf(env.output, "methods\t%s\n", strings.Join(info.Methods, " "))

	return nil
}

func runRegister(ctx context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("register", flag.ContinueOnError)
	save := flags.Bool("save", false, "save the token for later commands")
	if err := flags.Parse(arguments); err != nil {
		return err //nolint:wrapcheck // the flag package already printed the problem
	}

	name, err := env.identity.resolveName()
	if err != nil {
		return err
	}

	workdir := env.identity.workdir
	if workdir == "" {
		if workdir, err = os.Getwd(); err != nil {
			return fmt.Errorf("resolving the working directory: %w", err)
		}
	}

	client, err := lacclient.Dial(ctx, lacclient.Options{SocketPath: env.socketPath})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	registration, err := client.Register(ctx, name, env.identity.kind, workdir, os.Getpid())
	if err != nil {
		return err
	}

	var savedTo string
	if *save {
		if savedTo, err = saveToken(registration.Token); err != nil {
			return err
		}
	}

	if env.asJSON {
		return writeJSON(env, map[string]any{
			"agent": registration.Agent, "token": registration.Token, "saved_to": savedTo,
		})
	}

	fmt.Fprintf(env.output, "registered\t%s (%s)\n", registration.Agent.Name, registration.Agent.ID)
	fmt.Fprintf(env.output, "token\t%s\n", registration.Token)
	if savedTo != "" {
		fmt.Fprintf(env.output, "saved to\t%s\n", savedTo)
	} else {
		fmt.Fprintf(env.output, "\t(this token is shown once; export LAC_TOKEN to reuse it)\n")
	}

	return nil
}

func runAgents(ctx context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("agents", flag.ContinueOnError)
	all := flags.Bool("all", false, "include stale and deregistered agents")
	if err := flags.Parse(arguments); err != nil {
		return err //nolint:wrapcheck // the flag package already printed the problem
	}

	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	var states []string
	if *all {
		states = []string{"active", "stale", "deregistered"}
	}

	agents, err := client.Agents(ctx, states...)
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, agents)
	}

	if len(agents) == 0 {
		fmt.Fprintln(env.output, "no agents are connected")
		return nil
	}

	fmt.Fprintln(env.output, "NAME\tKIND\tSTATE\tWORKDIR\tLAST SEEN")
	for _, agent := range agents {
		fmt.Fprintf(env.output, "%s\t%s\t%s\t%s\t%s\n",
			agent.Name, agent.Kind, agent.State, agent.Workdir, shortTime(agent.LastHeartbeatAt))
	}

	return nil
}

func runSend(ctx context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("send", flag.ContinueOnError)
	topic := flags.String("topic", "", "publish to this topic instead of a single agent")
	if err := flags.Parse(arguments); err != nil {
		return err //nolint:wrapcheck // the flag package already printed the problem
	}

	positional := flags.Args()
	if *topic == "" && len(positional) < 3 {
		return errors.New("usage: lac send <agent> <kind> <body>, or lac send --topic <topic> <kind> <body>")
	}
	if *topic != "" && len(positional) < 2 {
		return errors.New("usage: lac send --topic <topic> <kind> <body>")
	}

	var recipient, kind, body string
	if *topic != "" {
		kind, body = positional[0], strings.Join(positional[1:], " ")
	} else {
		recipient, kind, body = positional[0], positional[1], strings.Join(positional[2:], " ")
	}

	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	payload := bodyOf(body)

	var sent lacclient.Sent
	if *topic != "" {
		sent, err = client.SendToTopic(ctx, *topic, kind, payload)
	} else {
		sent, err = client.SendTo(ctx, recipient, kind, payload)
	}
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, sent)
	}

	fmt.Fprintf(env.output, "sent\t%s to %d agent(s), %d connected\n",
		sent.MessageID, sent.Recipients, sent.Notified)

	return nil
}

// bodyOf lets a caller pass either JSON or plain text. Plain text becomes {"text": "..."} so the
// wire format stays JSON without forcing everyone to quote braces in a shell.
func bodyOf(body string) any {
	trimmed := strings.TrimSpace(body)
	if json.Valid([]byte(trimmed)) && strings.HasPrefix(trimmed, "{") {
		return json.RawMessage(trimmed)
	}

	return map[string]string{"text": body}
}

func runInbox(ctx context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("inbox", flag.ContinueOnError)
	acknowledge := flags.Bool("ack", false, "acknowledge the messages that are shown")
	limit := flags.Int("limit", 0, "how many messages to read")
	if err := flags.Parse(arguments); err != nil {
		return err //nolint:wrapcheck // the flag package already printed the problem
	}

	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	messages, err := client.Inbox(ctx, *limit)
	if err != nil {
		return err
	}

	if *acknowledge && len(messages) > 0 {
		ids := make([]string, 0, len(messages))
		for _, message := range messages {
			ids = append(ids, message.ID)
		}
		if _, err := client.Acknowledge(ctx, ids...); err != nil {
			return err
		}
	}

	if env.asJSON {
		return writeJSON(env, messages)
	}

	if len(messages) == 0 {
		fmt.Fprintln(env.output, "nothing waiting")
		return nil
	}

	fmt.Fprintln(env.output, "FROM\tKIND\tWHEN\tBODY")
	for _, message := range messages {
		from := message.FromName
		if message.Topic != "" {
			from += " → " + message.Topic
		}
		fmt.Fprintf(env.output, "%s\t%s\t%s\t%s\n",
			from, message.Kind, shortTime(message.CreatedAt), string(message.Body))
	}

	return nil
}

func runResources(ctx context.Context, env *environment, _ []string) error {
	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	resources, err := client.Resources(ctx)
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, resources)
	}

	if len(resources) == 0 {
		fmt.Fprintln(env.output, "no resources are defined yet\n\n"+
			"  define one now:   lac --name operator define --capacity 4 test\n"+
			"  or permanently:   add it to ~/.config/lac/config.yaml")
		return nil
	}

	fmt.Fprintln(env.output, "NAME\tHELD\tFREE\tWAITING\tTTL\tDESCRIPTION")
	for _, resource := range resources {
		fmt.Fprintf(env.output, "%s\t%d/%d\t%d\t%d\t%s\t%s\n",
			resource.Name, resource.Held, resource.Capacity, resource.Free,
			resource.Waiting, resource.LeaseTimeToLive, resource.Description)
	}

	return nil
}

// runDefine creates or reconfigures a resource. It needs the operator capability, because the
// machine's limits are the operator's decision rather than an agent's.
func runDefine(ctx context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("define", flag.ContinueOnError)
	var (
		capacity    = flags.Int("capacity", 0, "how many agents may hold a slot at once (required)")
		timeToLive  = flags.Duration("ttl", 0, "how long a slot survives without renewal")
		description = flags.String("description", "", "what this resource is")
	)
	if err := flags.Parse(arguments); err != nil {
		return err //nolint:wrapcheck // the flag package already printed the problem
	}
	if flags.NArg() < 1 || *capacity < 1 {
		return errors.New("usage: lac define --capacity <n> [flags] <resource>")
	}

	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	resource, err := client.DefineResource(ctx, flags.Arg(0), *capacity, *timeToLive, *description)
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, resource)
	}

	fmt.Fprintf(env.output, "defined\t%s with %d slot(s), %s lease\n",
		resource.Name, resource.Capacity, resource.LeaseTimeToLive)

	return nil
}

func runQueue(ctx context.Context, env *environment, arguments []string) error {
	if len(arguments) < 1 {
		return errors.New("usage: lac queue <resource>")
	}

	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	status, err := client.Queue(ctx, arguments[0])
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, status)
	}

	fmt.Fprintf(env.output, "%s\t%d of %d slots held, %d waiting\n",
		status.Resource.Name, status.Resource.Held, status.Resource.Capacity, status.Resource.Waiting)

	if len(status.Waiting) > 0 {
		fmt.Fprintln(env.output, "\nPOSITION\tAGENT\tPRIORITY\tSINCE\tREASON")
		for _, entry := range status.Waiting {
			fmt.Fprintf(env.output, "%d\t%s\t%d\t%s\t%s\n",
				entry.Position, entry.AgentName, entry.Priority, shortTime(entry.RequestedAt), entry.Reason)
		}
	}

	return nil
}

func runAcquire(ctx context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("acquire", flag.ContinueOnError)
	var (
		reason   = flags.String("reason", "", "what the slot is for, shown in the queue")
		priority = flags.Int("priority", 0, "higher goes first")
		noWait   = flags.Bool("no-wait", false, "fail immediately if the resource is full")
		timeout  = flags.Duration("timeout", 0, "give up after this long")
	)
	if err := flags.Parse(arguments); err != nil {
		return err //nolint:wrapcheck // the flag package already printed the problem
	}
	if flags.NArg() < 1 {
		return errors.New("usage: lac acquire [flags] <resource>")
	}

	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	lease, err := client.Acquire(ctx, lacclient.AcquireRequest{
		Resource: flags.Arg(0), Reason: *reason, Priority: *priority,
		NoWait: *noWait, Timeout: *timeout,
	})
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, lease)
	}

	fmt.Fprintf(env.output, "granted\t%s on %s until %s\n", lease.ID, lease.Resource, shortTime(lease.ExpiresAt))
	fmt.Fprintf(env.output, "\trelease it with: lac release %s\n", lease.ID)

	return nil
}

func runRelease(ctx context.Context, env *environment, arguments []string) error {
	if len(arguments) < 1 {
		return errors.New("usage: lac release <lease-id>")
	}

	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	if err := client.Release(ctx, arguments[0]); err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, map[string]any{"released": true, "lease_id": arguments[0]})
	}

	fmt.Fprintf(env.output, "released\t%s\n", arguments[0])

	return nil
}

func runHeld(ctx context.Context, env *environment, _ []string) error {
	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	leases, err := client.Held(ctx)
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, leases)
	}

	if len(leases) == 0 {
		fmt.Fprintln(env.output, "you are not holding any slots")
		return nil
	}

	fmt.Fprintln(env.output, "LEASE\tRESOURCE\tEXPIRES")
	for _, lease := range leases {
		fmt.Fprintf(env.output, "%s\t%s\t%s\n", lease.ID, lease.Resource, shortTime(lease.ExpiresAt))
	}

	return nil
}

func runDeregister(ctx context.Context, env *environment, _ []string) error {
	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	released, err := client.Deregister(ctx)
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, map[string]int{"released_leases": released})
	}

	fmt.Fprintf(env.output, "deregistered\t%d slot(s) given back\n", released)

	return nil
}

func writeJSON(env *environment, value any) error {
	if err := env.output.Flush(); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")

	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}

	return nil
}

// shortTime renders a daemon timestamp as something readable in a table.
func shortTime(value string) string {
	if value == "" {
		return "-"
	}

	instant, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return value
	}

	return instant.Local().Format("15:04:05")
}

// runReport asks every agent what it is doing and waits for the answers. It is the operator's
// question — "what is everybody up to?" — with one command and one screen of output.
func runReport(ctx context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("report", flag.ContinueOnError)
	deadline := flags.Duration("deadline", 30*time.Second, "how long to wait for answers")
	if err := flags.Parse(arguments); err != nil {
		return err //nolint:wrapcheck // the flag package already printed the problem
	}

	question := strings.Join(flags.Args(), " ")
	if question == "" {
		question = "What are you working on right now?"
	}

	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	collection, err := client.RequestReports(ctx, question, *deadline, true)
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, collection)
	}

	fmt.Fprintf(env.output, "asked\t%d agent(s): %s\n", collection.Asked, collection.Question)

	if collection.Asked == 0 {
		fmt.Fprintln(env.output, "\nnobody else is connected")
		return nil
	}

	for _, report := range collection.Reports {
		fmt.Fprintf(env.output, "\n%s\t%s\n", report.AgentName, report.Body)
	}

	if len(collection.Silent) > 0 {
		fmt.Fprintf(env.output, "\nno answer\t%s\n", strings.Join(collection.Silent, ", "))
	}

	return nil
}

// runAnswer answers a report request, for an agent driving LAC from a shell rather than MCP.
func runAnswer(ctx context.Context, env *environment, arguments []string) error {
	if len(arguments) < 2 {
		return errors.New("usage: lac answer <request-id> <what you are doing>")
	}

	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	if err := client.SubmitReport(ctx, arguments[0], strings.Join(arguments[1:], " ")); err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, map[string]any{"recorded": true, "request_id": arguments[0]})
	}

	fmt.Fprintf(env.output, "answered\t%s\n", arguments[0])

	return nil
}

// runAsk hands a piece of work to a shared worker. It is the other half of `lac run`: instead of
// waiting for a slot and doing the work yourself, you ask the machine's worker to do it.
func runAsk(ctx context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("ask", flag.ContinueOnError)
	var (
		workdir    = flags.String("workdir", "", "where to do the work (default: your own directory)")
		background = flags.Bool("background", false, "return as soon as it is queued")
		timeout    = flags.Duration("timeout", 0, "give up waiting after this long; the work carries on")
	)
	if err := flags.Parse(arguments); err != nil {
		return err //nolint:wrapcheck // the flag package already printed the problem
	}
	if flags.NArg() < 1 {
		return errors.New("usage: lac ask [flags] <command>   (lac commands lists what you can ask for)")
	}

	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	task, err := client.SubmitTask(ctx, flags.Arg(0), *workdir, !*background, *timeout)
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, task)
	}

	if !task.Finished() {
		fmt.Fprintf(env.output, "queued\t%s (%s)\n", task.ID, task.Command)
		fmt.Fprintf(env.output, "\tfollow it with: lac task %s --wait\n", task.ID)

		return nil
	}

	return reportTask(env, task)
}

// reportTask prints a finished task and returns its exit status as this command's own, so `lac ask`
// can stand in for running the thing yourself.
func reportTask(env *environment, task lacclient.Task) error {
	if task.Output != "" {
		if err := env.output.Flush(); err != nil {
			return fmt.Errorf("writing output: %w", err)
		}
		fmt.Fprint(os.Stdout, task.Output)
		if !strings.HasSuffix(task.Output, "\n") {
			fmt.Fprintln(os.Stdout)
		}
	}

	worker := task.Worker
	if worker == "" {
		worker = "nobody"
	}

	fmt.Fprintf(env.output, "%s\t%s, run by %s\n", task.State, task.Command, worker)
	if task.Failure != "" {
		fmt.Fprintf(env.output, "\t%s\n", task.Failure)
	}

	if task.ExitCode != 0 {
		if err := env.output.Flush(); err != nil {
			return fmt.Errorf("writing output: %w", err)
		}

		return exitCode{code: task.ExitCode}
	}

	return nil
}

// runTasks lists the work this agent has asked for.
func runTasks(ctx context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("tasks", flag.ContinueOnError)
	limit := flags.Int("limit", 0, "how many to show")
	if err := flags.Parse(arguments); err != nil {
		return err //nolint:wrapcheck // the flag package already printed the problem
	}

	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	tasks, err := client.Tasks(ctx, *limit)
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, tasks)
	}

	if len(tasks) == 0 {
		fmt.Fprintln(env.output, "you have not asked for any work")
		return nil
	}

	fmt.Fprintln(env.output, "TASK\tCOMMAND\tSTATE\tWORKER\tSUBMITTED")
	for _, task := range tasks {
		fmt.Fprintf(env.output, "%s\t%s\t%s\t%s\t%s\n",
			task.ID, task.Command, task.State, task.Worker, shortTime(task.SubmittedAt))
	}

	return nil
}

// runTaskStatus shows one task, optionally waiting for it to finish.
func runTaskStatus(ctx context.Context, env *environment, arguments []string) error {
	flags := flag.NewFlagSet("task", flag.ContinueOnError)
	wait := flags.Bool("wait", false, "wait until it finishes")
	if err := flags.Parse(arguments); err != nil {
		return err //nolint:wrapcheck // the flag package already printed the problem
	}
	if flags.NArg() < 1 {
		return errors.New("usage: lac task [--wait] <task-id>")
	}

	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	task, err := client.TaskStatus(ctx, flags.Arg(0), *wait)
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, task)
	}
	if !task.Finished() {
		fmt.Fprintf(env.output, "%s\t%s (%s)\n", task.State, task.Command, task.ID)
		if task.Output != "" {
			fmt.Fprintf(env.output, "\nso far:\n%s\n", task.Output)
		}

		return nil
	}

	return reportTask(env, task)
}

// runCommands lists what a shared worker on this machine can be asked to do.
func runCommands(ctx context.Context, env *environment, _ []string) error {
	client, release, err := env.identity.connect(ctx, env.socketPath)
	if err != nil {
		return err
	}
	defer release()

	commands, err := client.Commands(ctx)
	if err != nil {
		return err
	}

	if env.asJSON {
		return writeJSON(env, commands)
	}

	if len(commands) == 0 {
		fmt.Fprintln(env.output, "no commands are configured\n\n"+
			"  an operator adds them under `commands:` in ~/.config/lac/config.yaml")

		return nil
	}

	fmt.Fprintln(env.output, "COMMAND\tRESOURCE\tRUNS\tDESCRIPTION")
	for _, command := range commands {
		fmt.Fprintf(env.output, "%s\t%s\t%s\t%s\n",
			command.Key, command.Resource, strings.Join(command.Run, " "), command.Description)
	}

	return nil
}
