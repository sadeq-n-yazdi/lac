package dispatch_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code.sadeq.uk/lac/internal/core"
	"code.sadeq.uk/lac/internal/id"
	"code.sadeq.uk/lac/internal/service/dispatch"
	"code.sadeq.uk/lac/internal/service/leasing"
	"code.sadeq.uk/lac/internal/store/sqlite"
)

var baseTime = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// catalogue is the operator's configured commands, which is the only place a command line ever
// comes from.
type catalogue struct {
	commands map[string]dispatch.Command
}

func (c catalogue) Command(key string) (dispatch.Command, error) {
	command, found := c.commands[key]
	if !found {
		return dispatch.Command{}, fmt.Errorf("%w: no command called %q", core.ErrNotFound, key)
	}

	return command, nil
}

func (c catalogue) Commands() []dispatch.Command {
	all := make([]dispatch.Command, 0, len(c.commands))
	for _, command := range c.commands {
		all = append(all, command)
	}

	return all
}

type harness struct {
	service   *dispatch.Service
	leasing   *leasing.Service
	store     *sqlite.Store
	catalogue catalogue
}

func newHarness(t *testing.T, roots ...string) *harness {
	t.Helper()

	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "lac.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	commands := catalogue{commands: map[string]dispatch.Command{
		"test": {Key: "test", Resource: "test", Run: []string{"make", "test"}, Description: "the tests"},
	}}

	leasingService := leasing.New(store, leasing.Options{Logger: quietLogger()})

	subject := &harness{
		service: dispatch.New(store, commands, dispatch.Options{
			WorkdirRoots: roots,
			Logger:       quietLogger(),
		}),
		leasing:   leasingService,
		store:     store,
		catalogue: commands,
	}

	if err := leasingService.Define(t.Context(), "operator", core.Resource{
		Name: "test", Capacity: 1, LeaseTimeToLive: time.Hour,
	}); err != nil {
		t.Fatalf("Define() = %v, want nil", err)
	}

	return subject
}

func (h *harness) newAgent(t *testing.T, name, workdir string) core.Agent {
	t.Helper()

	agent := core.Agent{
		ID: id.New("agent"), Name: name, Kind: "claude", Workdir: workdir,
		Capabilities: core.Capabilities{Resources: []string{core.WildcardResource}},
		State:        core.AgentActive, RegisteredAt: baseTime, LastHeartbeatAt: baseTime,
	}
	if err := h.store.Agents().Create(t.Context(), agent); err != nil {
		t.Fatalf("creating the agent: %v", err)
	}

	return agent
}

// takeSlot gives a worker a real lease, which is what it must hold to claim work.
func (h *harness) takeSlot(t *testing.T, worker core.Agent) core.Lease {
	t.Helper()

	lease, err := h.leasing.Acquire(t.Context(), leasing.AcquireRequest{
		AgentID: worker.ID, ResourceName: "test",
	})
	if err != nil {
		t.Fatalf("Acquire() = %v, want nil", err)
	}

	return lease
}

// The whole loop: one agent asks, a worker claims it, runs it, reports it, and the requester gets
// the result.
func TestSubmitClaimAndComplete(t *testing.T) {
	subject := newHarness(t)
	requester := subject.newAgent(t, "claude-a", "/tmp/project")
	worker := subject.newAgent(t, "test-runner", "/tmp/runner")

	task, err := subject.service.Submit(t.Context(), dispatch.SubmitRequest{
		RequesterID: requester.ID, CommandKey: "test",
	})
	if err != nil {
		t.Fatalf("Submit() = %v, want nil", err)
	}
	if task.State != core.TaskQueued {
		t.Errorf("state = %q, want queued", task.State)
	}
	// An empty working directory means the requester's own, which is what "run my tests" means.
	if task.Workdir != "/tmp/project" {
		t.Errorf("workdir = %q, want the requester's own", task.Workdir)
	}

	lease := subject.takeSlot(t, worker)

	claimed, err := subject.service.Claim(t.Context(), "test", worker.ID, lease.ID, false)
	if err != nil {
		t.Fatalf("Claim() = %v, want nil", err)
	}
	if claimed.Task.ID != task.ID {
		t.Errorf("claimed %q, want %q", claimed.Task.ID, task.ID)
	}
	// The command comes from the operator's configuration, never from the request.
	if strings.Join(claimed.Command.Run, " ") != "make test" {
		t.Errorf("the worker was told to run %v, want the configured command", claimed.Command.Run)
	}

	if err := subject.service.AppendOutput(t.Context(), task.ID, worker.ID, "ok  parser\n"); err != nil {
		t.Fatalf("AppendOutput() = %v, want nil", err)
	}

	finished, err := subject.service.Complete(t.Context(), task.ID, worker.ID, 0, "")
	if err != nil {
		t.Fatalf("Complete() = %v, want nil", err)
	}
	if finished.State != core.TaskSucceeded {
		t.Errorf("state = %q, want succeeded", finished.State)
	}
	if finished.Output != "ok  parser\n" {
		t.Errorf("output = %q, want what the command printed", finished.Output)
	}
}

// A requester names a key. If it could name a command line, dispatch would be a way to run anything
// as this user.
func TestOnlyConfiguredCommandsCanBeAsked(t *testing.T) {
	subject := newHarness(t)
	requester := subject.newAgent(t, "claude-a", "/tmp/project")

	for _, key := range []string{"rm", "make test", "/bin/sh", ""} {
		t.Run(key, func(t *testing.T) {
			_, err := subject.service.Submit(t.Context(), dispatch.SubmitRequest{
				RequesterID: requester.ID, CommandKey: key,
			})
			if err == nil {
				t.Fatalf("Submit(%q) succeeded; only configured commands may run", key)
			}
			if !errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrInvalidArgument) {
				t.Errorf("Submit(%q) = %v, want a refusal", key, err)
			}
		})
	}
}

// A task is the one place LAC really executes something, so where it runs is checked.
func TestWorkdirIsConfinedToTheAllowedRoots(t *testing.T) {
	subject := newHarness(t, "/tmp/project")
	requester := subject.newAgent(t, "claude-a", "/tmp/project")

	if _, err := subject.service.Submit(t.Context(), dispatch.SubmitRequest{
		RequesterID: requester.ID, CommandKey: "test", Workdir: "/tmp/project/sub",
	}); err != nil {
		t.Errorf("a directory inside a root was refused: %v", err)
	}

	for _, workdir := range []string{"/etc", "/tmp/project-evil", "relative/path"} {
		t.Run(workdir, func(t *testing.T) {
			_, err := subject.service.Submit(t.Context(), dispatch.SubmitRequest{
				RequesterID: requester.ID, CommandKey: "test", Workdir: workdir,
			})
			if err == nil {
				t.Fatalf("Submit() accepted work in %q", workdir)
			}
			if !errors.Is(err, core.ErrUnauthorised) && !errors.Is(err, core.ErrInvalidArgument) {
				t.Errorf("Submit() = %v, want a refusal", err)
			}
		})
	}
}

// Dispatch must not be a way around the capacity limit, so a worker without a slot gets nothing.
func TestAWorkerMustHoldASlot(t *testing.T) {
	subject := newHarness(t)
	requester := subject.newAgent(t, "claude-a", "/tmp/project")
	worker := subject.newAgent(t, "test-runner", "/tmp/runner")
	stranger := subject.newAgent(t, "impostor", "/tmp/impostor")

	if _, err := subject.service.Submit(t.Context(), dispatch.SubmitRequest{
		RequesterID: requester.ID, CommandKey: "test",
	}); err != nil {
		t.Fatalf("Submit() = %v, want nil", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	if _, err := subject.service.Claim(ctx, "test", worker.ID, "", false); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("Claim() with no lease = %v, want ErrInvalidArgument", err)
	}

	// A lease that belongs to somebody else is no better than none.
	lease := subject.takeSlot(t, worker)
	if _, err := subject.service.Claim(ctx, "test", stranger.ID, lease.ID, false); !errors.Is(err, core.ErrUnauthorised) {
		t.Errorf("Claim() with another agent's lease = %v, want ErrUnauthorised", err)
	}
}

// Two workers reaching for the same task must not both get it, or the work runs twice.
func TestATaskIsClaimedOnce(t *testing.T) {
	subject := newHarness(t)
	requester := subject.newAgent(t, "claude-a", "/tmp/project")

	if err := subject.leasing.Define(t.Context(), "operator", core.Resource{
		Name: "test", Capacity: 2, LeaseTimeToLive: time.Hour,
	}); err != nil {
		t.Fatalf("Define() = %v, want nil", err)
	}

	task, err := subject.service.Submit(t.Context(), dispatch.SubmitRequest{
		RequesterID: requester.ID, CommandKey: "test",
	})
	if err != nil {
		t.Fatalf("Submit() = %v, want nil", err)
	}

	first := subject.newAgent(t, "runner-one", "/tmp/one")
	second := subject.newAgent(t, "runner-two", "/tmp/two")
	firstLease := subject.takeSlot(t, first)
	secondLease := subject.takeSlot(t, second)

	claimed, err := subject.service.Claim(t.Context(), "test", first.ID, firstLease.ID, false)
	if err != nil {
		t.Fatalf("the first Claim() = %v, want nil", err)
	}
	if claimed.Task.ID != task.ID {
		t.Fatalf("claimed %q, want %q", claimed.Task.ID, task.ID)
	}

	// There is no more work, so the second worker waits rather than getting the same task.
	ctx, cancel := context.WithTimeout(t.Context(), 1*time.Second)
	defer cancel()

	if _, err := subject.service.Claim(ctx, "test", second.ID, secondLease.ID, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the second Claim() = %v, want it to wait for work that never came", err)
	}
}

// A worker waiting for work must be given it the moment it is submitted, without polling.
func TestClaimWaitsForWork(t *testing.T) {
	subject := newHarness(t)
	requester := subject.newAgent(t, "claude-a", "/tmp/project")
	worker := subject.newAgent(t, "test-runner", "/tmp/runner")
	lease := subject.takeSlot(t, worker)

	claimed := make(chan dispatch.Claimed, 1)
	go func() {
		result, err := subject.service.Claim(t.Context(), "test", worker.ID, lease.ID, false)
		if err != nil {
			t.Errorf("Claim() = %v, want nil", err)
			return
		}
		claimed <- result
	}()

	// Let the worker start waiting, then give it something to do.
	time.Sleep(50 * time.Millisecond)

	task, err := subject.service.Submit(t.Context(), dispatch.SubmitRequest{
		RequesterID: requester.ID, CommandKey: "test",
	})
	if err != nil {
		t.Fatalf("Submit() = %v, want nil", err)
	}

	select {
	case result := <-claimed:
		if result.Task.ID != task.ID {
			t.Errorf("claimed %q, want %q", result.Task.ID, task.ID)
		}
	case <-time.After(5 * time.Second):
		t.Error("the waiting worker was never given the work")
	}
}

// A requester waiting on a task must be released the moment it finishes.
func TestAwaitReturnsWhenTheWorkIsDone(t *testing.T) {
	subject := newHarness(t)
	requester := subject.newAgent(t, "claude-a", "/tmp/project")
	worker := subject.newAgent(t, "test-runner", "/tmp/runner")

	task, err := subject.service.Submit(t.Context(), dispatch.SubmitRequest{
		RequesterID: requester.ID, CommandKey: "test",
	})
	if err != nil {
		t.Fatalf("Submit() = %v, want nil", err)
	}

	lease := subject.takeSlot(t, worker)
	if _, err := subject.service.Claim(t.Context(), "test", worker.ID, lease.ID, false); err != nil {
		t.Fatalf("Claim() = %v, want nil", err)
	}

	awaited := make(chan core.Task, 1)
	go func() {
		finished, err := subject.service.Await(t.Context(), task.ID)
		if err != nil {
			t.Errorf("Await() = %v, want nil", err)
			return
		}
		awaited <- finished
	}()

	time.Sleep(50 * time.Millisecond)
	if _, err := subject.service.Complete(t.Context(), task.ID, worker.ID, 1, ""); err != nil {
		t.Fatalf("Complete() = %v, want nil", err)
	}

	select {
	case finished := <-awaited:
		if finished.State != core.TaskFailed || finished.ExitCode != 1 {
			t.Errorf("finished = %+v, want a failure with exit status 1", finished)
		}
	case <-time.After(5 * time.Second):
		t.Error("Await() did not return when the work finished")
	}
}

// One worker must not be able to report on another's work.
func TestOnlyTheRunningWorkerMayReport(t *testing.T) {
	subject := newHarness(t)
	requester := subject.newAgent(t, "claude-a", "/tmp/project")
	worker := subject.newAgent(t, "test-runner", "/tmp/runner")
	stranger := subject.newAgent(t, "impostor", "/tmp/impostor")

	task, err := subject.service.Submit(t.Context(), dispatch.SubmitRequest{
		RequesterID: requester.ID, CommandKey: "test",
	})
	if err != nil {
		t.Fatalf("Submit() = %v, want nil", err)
	}

	lease := subject.takeSlot(t, worker)
	if _, err := subject.service.Claim(t.Context(), "test", worker.ID, lease.ID, false); err != nil {
		t.Fatalf("Claim() = %v, want nil", err)
	}

	if _, err := subject.service.Complete(t.Context(), task.ID, stranger.ID, 0, ""); !errors.Is(err, core.ErrUnauthorised) {
		t.Errorf("Complete() by a stranger = %v, want ErrUnauthorised", err)
	}
	if err := subject.service.AppendOutput(t.Context(), task.ID, stranger.ID, "lies"); !errors.Is(err, core.ErrUnauthorised) {
		t.Errorf("AppendOutput() by a stranger = %v, want ErrUnauthorised", err)
	}
}

// A worker that dies mid-task leaves work that still needs doing, so it goes back in the queue.
func TestAnAbandonedTaskIsRequeued(t *testing.T) {
	subject := newHarness(t)
	requester := subject.newAgent(t, "claude-a", "/tmp/project")
	worker := subject.newAgent(t, "test-runner", "/tmp/runner")

	task, err := subject.service.Submit(t.Context(), dispatch.SubmitRequest{
		RequesterID: requester.ID, CommandKey: "test",
	})
	if err != nil {
		t.Fatalf("Submit() = %v, want nil", err)
	}

	lease := subject.takeSlot(t, worker)
	if _, err := subject.service.Claim(t.Context(), "test", worker.ID, lease.ID, false); err != nil {
		t.Fatalf("Claim() = %v, want nil", err)
	}
	if err := subject.service.AppendOutput(t.Context(), task.ID, worker.ID, "half a test run\n"); err != nil {
		t.Fatalf("AppendOutput() = %v, want nil", err)
	}

	requeued, err := subject.service.ReleaseAbandoned(t.Context(), worker.ID)
	if err != nil {
		t.Fatalf("ReleaseAbandoned() = %v, want nil", err)
	}
	if requeued != 1 {
		t.Fatalf("requeued %d tasks, want 1", requeued)
	}

	again, err := subject.service.Task(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("Task() = %v, want nil", err)
	}
	if again.State != core.TaskQueued {
		t.Errorf("state = %q, want it back in the queue", again.State)
	}
	// Half an output attached to the next attempt would be misleading.
	if again.Output != "" {
		t.Errorf("output = %q, want it cleared for the next attempt", again.Output)
	}
	if !strings.Contains(again.Failure, "went away") {
		t.Errorf("failure = %q, want it to say what happened", again.Failure)
	}
}

// A requester may withdraw work that has not started; work already running is left to finish,
// because stopping somebody's process mid-write is worse than letting it end.
func TestCancel(t *testing.T) {
	subject := newHarness(t)
	requester := subject.newAgent(t, "claude-a", "/tmp/project")
	stranger := subject.newAgent(t, "claude-b", "/tmp/other")
	worker := subject.newAgent(t, "test-runner", "/tmp/runner")

	task, err := subject.service.Submit(t.Context(), dispatch.SubmitRequest{
		RequesterID: requester.ID, CommandKey: "test",
	})
	if err != nil {
		t.Fatalf("Submit() = %v, want nil", err)
	}

	if _, err := subject.service.Cancel(t.Context(), task.ID, stranger.ID); !errors.Is(err, core.ErrUnauthorised) {
		t.Errorf("Cancel() by another agent = %v, want ErrUnauthorised", err)
	}

	cancelled, err := subject.service.Cancel(t.Context(), task.ID, requester.ID)
	if err != nil {
		t.Fatalf("Cancel() = %v, want nil", err)
	}
	if cancelled.State != core.TaskCancelled {
		t.Errorf("state = %q, want cancelled", cancelled.State)
	}

	// A running task cannot be withdrawn.
	running, err := subject.service.Submit(t.Context(), dispatch.SubmitRequest{
		RequesterID: requester.ID, CommandKey: "test",
	})
	if err != nil {
		t.Fatalf("Submit() = %v, want nil", err)
	}

	lease := subject.takeSlot(t, worker)
	if _, err := subject.service.Claim(t.Context(), "test", worker.ID, lease.ID, false); err != nil {
		t.Fatalf("Claim() = %v, want nil", err)
	}

	if _, err := subject.service.Cancel(t.Context(), running.ID, requester.ID); !errors.Is(err, core.ErrConflict) {
		t.Errorf("Cancel() on running work = %v, want ErrConflict", err)
	}
}

// Output is capped, keeping the end, because the end is where a failure explains itself.
func TestOutputIsCappedKeepingTheEnd(t *testing.T) {
	subject := newHarness(t)
	requester := subject.newAgent(t, "claude-a", "/tmp/project")
	worker := subject.newAgent(t, "test-runner", "/tmp/runner")

	task, err := subject.service.Submit(t.Context(), dispatch.SubmitRequest{
		RequesterID: requester.ID, CommandKey: "test",
	})
	if err != nil {
		t.Fatalf("Submit() = %v, want nil", err)
	}

	lease := subject.takeSlot(t, worker)
	if _, err := subject.service.Claim(t.Context(), "test", worker.ID, lease.ID, false); err != nil {
		t.Fatalf("Claim() = %v, want nil", err)
	}

	noise := strings.Repeat("x", core.MaxTaskOutput)
	if err := subject.service.AppendOutput(t.Context(), task.ID, worker.ID, noise); err != nil {
		t.Fatalf("AppendOutput() = %v, want nil", err)
	}
	if err := subject.service.AppendOutput(t.Context(), task.ID, worker.ID, "FAIL: the parser\n"); err != nil {
		t.Fatalf("AppendOutput() = %v, want nil", err)
	}

	stored, err := subject.service.Task(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("Task() = %v, want nil", err)
	}
	if len(stored.Output) > core.MaxTaskOutput {
		t.Errorf("output is %d bytes, want at most %d", len(stored.Output), core.MaxTaskOutput)
	}
	if !strings.HasSuffix(stored.Output, "FAIL: the parser\n") {
		t.Error("the end of the output was dropped; that is where the failure is")
	}
}

// An idle worker must not sit on a slot. Waiting for work is what happens first; the slot is taken
// only when there is something to do. Otherwise two idle workers on a two-slot resource would leave
// nobody else able to run anything.
func TestWaitingForWorkNeedsNoSlot(t *testing.T) {
	subject := newHarness(t)
	requester := subject.newAgent(t, "claude-a", "/tmp/project")

	waiting := make(chan error, 1)
	go func() { waiting <- subject.service.WaitForWork(t.Context(), "test") }()

	// Nothing queued yet, so the worker is still waiting — and the resource is still free.
	time.Sleep(50 * time.Millisecond)

	status, err := subject.leasing.Status(t.Context(), "test")
	if err != nil {
		t.Fatalf("Status() = %v, want nil", err)
	}
	if status.ActiveLeases != 0 {
		t.Errorf("%d slots are held while the worker is merely waiting, want 0", status.ActiveLeases)
	}

	if _, err := subject.service.Submit(t.Context(), dispatch.SubmitRequest{
		RequesterID: requester.ID, CommandKey: "test",
	}); err != nil {
		t.Fatalf("Submit() = %v, want nil", err)
	}

	select {
	case err := <-waiting:
		if err != nil {
			t.Errorf("WaitForWork() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("WaitForWork() did not return when work was submitted")
	}
}

// A worker that queued for a slot only to find another took the work must be told at once, so it
// can give the slot back rather than sit on it.
func TestClaimNoWaitReturnsWhenThereIsNothingLeft(t *testing.T) {
	subject := newHarness(t)
	worker := subject.newAgent(t, "test-runner", "/tmp/runner")
	lease := subject.takeSlot(t, worker)

	_, err := subject.service.Claim(t.Context(), "test", worker.ID, lease.ID, true)
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("Claim(noWait) with nothing queued = %v, want ErrNotFound", err)
	}
}
