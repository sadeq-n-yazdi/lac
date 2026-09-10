package core

import (
	"fmt"
	"time"
)

// TaskState is how far a dispatched piece of work has got.
type TaskState string

const (
	// TaskQueued means nobody has picked it up yet.
	TaskQueued TaskState = "queued"
	// TaskRunning means a worker is doing it, holding a slot on the resource.
	TaskRunning TaskState = "running"
	// TaskSucceeded means the command finished with exit status zero.
	TaskSucceeded TaskState = "succeeded"
	// TaskFailed means the command failed, or the worker could not run it.
	TaskFailed TaskState = "failed"
	// TaskCancelled means the requester withdrew it, or the worker went away.
	TaskCancelled TaskState = "cancelled"
)

// Valid reports whether the state is one this domain defines.
func (s TaskState) Valid() bool {
	switch s {
	case TaskQueued, TaskRunning, TaskSucceeded, TaskFailed, TaskCancelled:
		return true
	default:
		return false
	}
}

// Finished reports whether the task will not change again.
func (s TaskState) Finished() bool {
	return s == TaskSucceeded || s == TaskFailed || s == TaskCancelled
}

// MaxTaskOutput is how much of a command's output is kept. Beyond it the beginning is dropped and
// the end is kept, because the end is where the failure is.
const MaxTaskOutput = 256 << 10

// Task is work one agent asked a shared worker to do.
//
// It names a command *key*, never a command line: what that key runs is the operator's decision,
// recorded in the daemon's configuration. A requester can ask for "the tests" and cannot say what
// "the tests" means.
type Task struct {
	// ID is assigned by the daemon.
	ID string
	// ResourceName is the resource whose workers do this kind of work, and whose capacity limits
	// how many run at once.
	ResourceName string
	// CommandKey names a command in the daemon's configuration.
	CommandKey string
	// Workdir is where the work happens: the requester's own directory, checked against the
	// allowed roots when the task was submitted.
	Workdir string
	// RequesterID is the agent that asked.
	RequesterID string
	// WorkerID is the agent doing it, once claimed.
	WorkerID string
	// LeaseID is the slot the worker holds while running it.
	LeaseID string
	// State is how far it has got.
	State TaskState
	// ExitCode is the command's exit status, once it has finished.
	ExitCode int
	// Output is what the command printed, truncated from the front at MaxTaskOutput.
	Output string
	// Failure explains a task that failed for a reason other than the command's exit status.
	Failure string
	// SubmittedAt, StartedAt and FinishedAt trace its progress. The last two are zero until they
	// happen.
	SubmittedAt time.Time
	StartedAt   time.Time
	FinishedAt  time.Time
}

// Validate reports whether the task is well formed enough to store.
func (t Task) Validate() error {
	if t.ID == "" {
		return fmt.Errorf("%w: task id is required", ErrInvalidArgument)
	}
	if err := ValidateName("resource name", t.ResourceName); err != nil {
		return err
	}
	if err := ValidateName("command", t.CommandKey); err != nil {
		return err
	}
	if t.Workdir == "" {
		return fmt.Errorf("%w: a task needs a working directory", ErrInvalidArgument)
	}
	if t.RequesterID == "" {
		return fmt.Errorf("%w: a task needs a requester", ErrInvalidArgument)
	}
	if !t.State.Valid() {
		return fmt.Errorf("%w: unknown task state %q", ErrInvalidArgument, t.State)
	}

	return nil
}

// TruncateOutput keeps the end of the output, which is where a failure explains itself.
func TruncateOutput(output string) string {
	if len(output) <= MaxTaskOutput {
		return output
	}

	const notice = "… output truncated; the beginning was dropped …\n"

	return notice + output[len(output)-MaxTaskOutput:]
}
