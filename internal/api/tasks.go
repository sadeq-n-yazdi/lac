package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"code.sadeq.uk/lac/internal/auth"
	"code.sadeq.uk/lac/internal/core"
	"code.sadeq.uk/lac/internal/service/dispatch"
	"code.sadeq.uk/lac/internal/transport/jsonrpc"
)

// TaskView is a dispatched piece of work as clients see it.
type TaskView struct {
	ID          string `json:"id"`
	Command     string `json:"command"`
	Resource    string `json:"resource"`
	Workdir     string `json:"workdir"`
	Requester   string `json:"requester,omitempty"`
	Worker      string `json:"worker,omitempty"`
	State       string `json:"state"`
	ExitCode    int    `json:"exit_code"`
	Output      string `json:"output,omitempty"`
	Failure     string `json:"failure,omitempty"`
	SubmittedAt string `json:"submitted_at"`
	StartedAt   string `json:"started_at,omitempty"`
	FinishedAt  string `json:"finished_at,omitempty"`
}

func (a *API) viewOfTask(ctx context.Context, task core.Task) TaskView {
	return TaskView{
		ID:          task.ID,
		Command:     task.CommandKey,
		Resource:    task.ResourceName,
		Workdir:     task.Workdir,
		Requester:   a.nameOf(ctx, task.RequesterID),
		Worker:      a.nameOf(ctx, task.WorkerID),
		State:       string(task.State),
		ExitCode:    task.ExitCode,
		Output:      task.Output,
		Failure:     task.Failure,
		SubmittedAt: formatTime(task.SubmittedAt),
		StartedAt:   formatTime(task.StartedAt),
		FinishedAt:  formatTime(task.FinishedAt),
	}
}

// CommandView is something this machine can be asked to do.
type CommandView struct {
	Key         string   `json:"key"`
	Resource    string   `json:"resource"`
	Description string   `json:"description,omitempty"`
	Run         []string `json:"run"`
	TimeLimit   string   `json:"time_limit,omitempty"`
}

// TaskCommandsResult lists what a worker can be asked for.
type TaskCommandsResult struct {
	Commands []CommandView `json:"commands"`
}

func (a *API) handleTaskCommands(
	_ context.Context, _ core.Agent, _ *jsonrpc.Session, _ json.RawMessage,
) (any, error) {
	commands := a.dispatch.Commands()

	views := make([]CommandView, 0, len(commands))
	for _, command := range commands {
		view := CommandView{
			Key:         command.Key,
			Resource:    command.Resource,
			Description: command.Description,
			Run:         command.Run,
		}
		if command.TimeLimit > 0 {
			view.TimeLimit = command.TimeLimit.String()
		}
		views = append(views, view)
	}

	return TaskCommandsResult{Commands: views}, nil
}

// TaskSubmitParams asks a shared worker to do something.
//
// There is no way to say what to run beyond naming a configured command: that is what makes
// dispatch safe to have at all.
type TaskSubmitParams struct {
	// Command is a key from task.commands.
	Command string `json:"command"`
	// Workdir is where to do it. Empty means the requester's own working directory.
	Workdir string `json:"workdir,omitempty"`
	// Wait blocks until the work has finished and returns the result.
	Wait bool `json:"wait,omitempty"`
	// Timeout gives up waiting after a duration such as "10m". The work carries on regardless.
	Timeout string `json:"timeout,omitempty"`
}

// TaskResult is one task.
type TaskResult struct {
	Task TaskView `json:"task"`
}

func (a *API) handleTaskSubmit(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments TaskSubmitParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	task, err := a.dispatch.Submit(ctx, dispatch.SubmitRequest{
		RequesterID: caller.ID,
		CommandKey:  arguments.Command,
		Workdir:     arguments.Workdir,
	})
	if err != nil {
		return nil, err
	}

	if !arguments.Wait {
		return TaskResult{Task: a.viewOfTask(ctx, task)}, nil
	}

	timeout, err := parseDuration(arguments.Timeout)
	if err != nil {
		return nil, err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	finished, err := a.dispatch.Await(ctx, task.ID)
	if err != nil {
		return nil, err
	}

	return TaskResult{Task: a.viewOfTask(ctx, finished)}, nil
}

// TaskClaimParams is a worker asking for something to do.
type TaskClaimParams struct {
	// Resource is what this worker serves.
	Resource string `json:"resource"`
	// LeaseID is the slot the worker already holds on that resource. Dispatched work runs inside
	// the same capacity limit as everything else, and this is what proves it.
	LeaseID string `json:"lease_id"`
	// NoWait returns at once when there is nothing queued, so a worker that lost the race to
	// another can give its slot back rather than sit on it.
	NoWait bool `json:"no_wait,omitempty"`
}

// TaskWaitParams waits for work to exist, without claiming it and without needing a slot.
type TaskWaitParams struct {
	Resource string `json:"resource"`
}

// TaskWaitResult says there is something to do.
type TaskWaitResult struct {
	Available bool `json:"available"`
}

// handleTaskWait lets a worker wait for work *before* taking a slot.
//
// It grants nothing, which is the point: a worker that took a slot first and then waited would
// occupy capacity it is not using, and two idle workers on a two-slot resource would leave nobody
// else able to run anything.
func (a *API) handleTaskWait(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments TaskWaitParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	if err := a.authenticator.Authorise(ctx, caller, auth.PermissionLease, arguments.Resource); err != nil {
		return nil, err
	}

	if err := a.dispatch.WaitForWork(ctx, arguments.Resource); err != nil {
		return nil, err
	}

	return TaskWaitResult{Available: true}, nil
}

// TaskClaimResult is the work, and the command to run for it.
type TaskClaimResult struct {
	Task TaskView `json:"task"`
	// Run is the command the worker should execute, from the daemon's configuration. The requester
	// had no say in it.
	Run []string `json:"run"`
	// TimeLimit stops a command that will not finish. Empty means no limit beyond the lease.
	TimeLimit string `json:"time_limit,omitempty"`
}

func (a *API) handleTaskClaim(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments TaskClaimParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	// A worker is doing work for other agents, so it needs the resource in its own right.
	if err := a.authenticator.Authorise(ctx, caller, auth.PermissionLease, arguments.Resource); err != nil {
		return nil, err
	}

	claimed, err := a.dispatch.Claim(ctx, arguments.Resource, caller.ID, arguments.LeaseID, arguments.NoWait)
	if err != nil {
		return nil, err
	}

	result := TaskClaimResult{Task: a.viewOfTask(ctx, claimed.Task), Run: claimed.Command.Run}
	if claimed.Command.TimeLimit > 0 {
		result.TimeLimit = claimed.Command.TimeLimit.String()
	}

	return result, nil
}

// TaskOutputParams reports what a running task has printed.
type TaskOutputParams struct {
	TaskID string `json:"task_id"`
	Chunk  string `json:"chunk"`
}

// TaskOutputResult confirms the output was recorded.
type TaskOutputResult struct {
	Recorded bool `json:"recorded"`
}

// OutputNotificationMethod is pushed to a requester waiting on a task, so it sees progress rather
// than silence.
const OutputNotificationMethod = "task.output"

func (a *API) handleTaskOutput(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments TaskOutputParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}
	if arguments.TaskID == "" {
		return nil, fmt.Errorf("%w: which task? give task_id", core.ErrInvalidArgument)
	}

	if err := a.dispatch.AppendOutput(ctx, arguments.TaskID, caller.ID, arguments.Chunk); err != nil {
		return nil, err
	}

	// Push it to whoever asked for the work, if they are connected.
	if task, err := a.dispatch.Task(ctx, arguments.TaskID); err == nil && a.notifier != nil {
		a.notifier.Broadcast(task.RequesterID, OutputNotificationMethod, map[string]any{
			"task_id": task.ID,
			"chunk":   arguments.Chunk,
			"at":      time.Now().UTC().Format(time.RFC3339Nano),
		})
	}

	return TaskOutputResult{Recorded: true}, nil
}

// TaskCompleteParams is a worker reporting an outcome.
type TaskCompleteParams struct {
	TaskID   string `json:"task_id"`
	ExitCode int    `json:"exit_code"`
	// Failure explains a task that failed for a reason other than the command's exit status.
	Failure string `json:"failure,omitempty"`
}

func (a *API) handleTaskComplete(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments TaskCompleteParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}
	if arguments.TaskID == "" {
		return nil, fmt.Errorf("%w: which task? give task_id", core.ErrInvalidArgument)
	}

	task, err := a.dispatch.Complete(ctx, arguments.TaskID, caller.ID, arguments.ExitCode, arguments.Failure)
	if err != nil {
		return nil, err
	}

	return TaskResult{Task: a.viewOfTask(ctx, task)}, nil
}

// TaskIDParams names a task.
type TaskIDParams struct {
	TaskID string `json:"task_id"`
	// Wait blocks until the task finishes.
	Wait bool `json:"wait,omitempty"`
}

func (a *API) handleTaskStatus(
	ctx context.Context, _ core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments TaskIDParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}
	if arguments.TaskID == "" {
		return nil, fmt.Errorf("%w: which task? give task_id", core.ErrInvalidArgument)
	}

	fetch := a.dispatch.Task
	if arguments.Wait {
		fetch = a.dispatch.Await
	}

	task, err := fetch(ctx, arguments.TaskID)
	if err != nil {
		return nil, err
	}

	return TaskResult{Task: a.viewOfTask(ctx, task)}, nil
}

func (a *API) handleTaskCancel(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments TaskIDParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	task, err := a.dispatch.Cancel(ctx, arguments.TaskID, caller.ID)
	if err != nil {
		return nil, err
	}

	return TaskResult{Task: a.viewOfTask(ctx, task)}, nil
}

// TaskListParams narrows a listing of the caller's own tasks.
type TaskListParams struct {
	Limit int `json:"limit,omitempty"`
}

// TaskListResult is the caller's recent tasks.
type TaskListResult struct {
	Tasks []TaskView `json:"tasks"`
}

func (a *API) handleTaskList(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments TaskListParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	tasks, err := a.dispatch.Mine(ctx, caller.ID, arguments.Limit)
	if err != nil {
		return nil, err
	}

	views := make([]TaskView, 0, len(tasks))
	for _, task := range tasks {
		views = append(views, a.viewOfTask(ctx, task))
	}

	return TaskListResult{Tasks: views}, nil
}
