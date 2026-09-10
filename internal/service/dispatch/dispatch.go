// Package dispatch lets one agent ask a shared worker to do a piece of work on its behalf.
//
// It is built on leases rather than beside them: a worker claims a task, takes a slot on the
// resource like anybody else, and gives the slot back when it is done. The capacity guarantee is
// therefore the same one, whether work arrives through `lac run` or through a worker.
//
// The security property that makes this safe to have at all: a requester names a *command key*,
// never a command line. What that key runs comes from the daemon's configuration, so an agent can
// ask for "the tests" and can never say what "the tests" means.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"code.sadeq.uk/lac/internal/auth"
	"code.sadeq.uk/lac/internal/core"
	"code.sadeq.uk/lac/internal/id"
)

// recheckInterval is a safety net for waiters, not the mechanism: a submitted task wakes a worker
// at once, and a finished one wakes its requester. This catches what happened in another process.
const recheckInterval = 500 * time.Millisecond

// Command is what a worker is told to run. It comes from the daemon's configuration, never from
// the requester.
type Command struct {
	// Key is the name an agent asks for.
	Key string
	// Resource is the resource a worker must hold a slot on to run it.
	Resource string
	// Run is the program and its arguments, already split. No shell is involved.
	Run []string
	// Description is shown to agents choosing what to ask for.
	Description string
	// TimeLimit stops a command that will not finish. Zero leaves it to the lease.
	TimeLimit time.Duration
}

// Catalogue is the set of commands this machine offers.
type Catalogue interface {
	// Command returns one command, or an error wrapping core.ErrNotFound.
	Command(key string) (Command, error)
	// Commands returns all of them.
	Commands() []Command
}

// Options configure a Service.
type Options struct {
	// WorkdirRoots limits where work may be done. Empty allows anywhere, which is only sensible in
	// tests.
	WorkdirRoots []string
	// Clock defaults to the wall clock in UTC.
	Clock auth.Clock
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Service dispatches work to shared workers.
type Service struct {
	store     core.Store
	catalogue Catalogue
	options   Options
	now       auth.Clock
	logger    *slog.Logger

	arrivals  *signals
	outcomes  *signals
	waitMutex sync.Mutex
}

// New returns a dispatch service.
func New(store core.Store, catalogue Catalogue, options Options) *Service {
	if options.Clock == nil {
		options.Clock = func() time.Time { return time.Now().UTC() }
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}

	return &Service{
		store:     store,
		catalogue: catalogue,
		options:   options,
		now:       options.Clock,
		logger:    options.Logger,
		arrivals:  newSignals(),
		outcomes:  newSignals(),
	}
}

// Commands returns what this machine can be asked to do.
func (s *Service) Commands() []Command { return s.catalogue.Commands() }

// SubmitRequest asks for a piece of work.
type SubmitRequest struct {
	// RequesterID is the agent asking, taken from its authenticated connection.
	RequesterID string
	// CommandKey names a command in the daemon's configuration.
	CommandKey string
	// Workdir is where the work should happen — normally the requester's own directory.
	Workdir string
}

// Submit queues a piece of work for a shared worker.
func (s *Service) Submit(ctx context.Context, request SubmitRequest) (core.Task, error) {
	command, err := s.catalogue.Command(request.CommandKey)
	if err != nil {
		return core.Task{}, err
	}

	workdir, err := s.resolveWorkdir(ctx, request)
	if err != nil {
		return core.Task{}, err
	}

	task := core.Task{
		ID:           id.New("task"),
		ResourceName: command.Resource,
		CommandKey:   command.Key,
		Workdir:      workdir,
		RequesterID:  request.RequesterID,
		State:        core.TaskQueued,
		SubmittedAt:  s.now(),
	}

	if err := s.store.Tasks().Create(ctx, task); err != nil {
		return core.Task{}, err
	}

	s.logger.Debug("task submitted",
		"task", task.ID, "command", task.CommandKey, "requester", task.RequesterID)
	s.arrivals.signal(command.Resource)

	return task, nil
}

// resolveWorkdir decides where the work happens, and refuses anywhere the operator did not allow.
//
// An empty working directory means the requester's own, which is what an agent asking "run my
// tests" means. Anything else is checked, because a task is the one place in LAC where a command
// really is executed.
func (s *Service) resolveWorkdir(ctx context.Context, request SubmitRequest) (string, error) {
	workdir := request.Workdir

	if workdir == "" {
		requester, err := s.store.Agents().ByID(ctx, request.RequesterID)
		if err != nil {
			return "", err
		}
		workdir = requester.Workdir
	}

	if !filepath.IsAbs(workdir) {
		return "", fmt.Errorf("%w: the working directory %q must be absolute", core.ErrInvalidArgument, workdir)
	}

	cleaned := filepath.Clean(workdir)

	if len(s.options.WorkdirRoots) == 0 {
		return cleaned, nil
	}

	for _, root := range s.options.WorkdirRoots {
		if within(cleaned, filepath.Clean(root)) {
			return cleaned, nil
		}
	}

	return "", fmt.Errorf("%w: work may not be run in %q; it is outside the allowed roots %s",
		core.ErrUnauthorised, cleaned, strings.Join(s.options.WorkdirRoots, ", "))
}

// Claimed is a task a worker has taken on, with the command it should run.
type Claimed struct {
	Task    core.Task
	Command Command
}

// Claim waits for work on a resource and hands it to a worker holding a slot.
//
// The worker must already hold the lease it passes: that is what keeps dispatched work inside the
// same capacity limit as everything else.
func (s *Service) Claim(
	ctx context.Context, resourceName, workerID, leaseID string,
) (Claimed, error) {
	if leaseID == "" {
		return Claimed{}, fmt.Errorf("%w: a worker must hold a slot before claiming work",
			core.ErrInvalidArgument)
	}
	if err := s.verifyLease(ctx, resourceName, workerID, leaseID); err != nil {
		return Claimed{}, err
	}

	for {
		// Watch before looking, so work submitted while we are looking still wakes us.
		arrived := s.arrivals.watch(resourceName)

		claimed, found, err := s.tryClaim(ctx, resourceName, workerID, leaseID)
		if err != nil {
			return Claimed{}, err
		}
		if found {
			return claimed, nil
		}

		timer := time.NewTimer(recheckInterval)

		select {
		case <-arrived:
			timer.Stop()
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return Claimed{}, ctx.Err() //nolint:wrapcheck // the caller compares with context errors
		}
	}
}

// tryClaim takes the oldest queued task, if there is one.
func (s *Service) tryClaim(
	ctx context.Context, resourceName, workerID, leaseID string,
) (Claimed, bool, error) {
	// One worker at a time reads the head of the queue and claims it. The database enforces this
	// too — Claim only succeeds while the task is queued — but taking it here as well means a
	// losing worker retries immediately instead of returning an error to a caller who cannot act
	// on it.
	s.waitMutex.Lock()
	defer s.waitMutex.Unlock()

	var claimed Claimed

	err := s.store.InTransaction(ctx, func(tx core.Store) error {
		task, err := tx.Tasks().NextQueued(ctx, resourceName)
		if err != nil {
			return err
		}

		command, err := s.catalogue.Command(task.CommandKey)
		if err != nil {
			// The operator removed the command since the task was submitted. Fail it rather than
			// leaving it in the queue forever.
			return tx.Tasks().Finish(ctx, task.ID, core.TaskFailed, -1,
				fmt.Sprintf("the command %q is no longer configured on this machine", task.CommandKey),
				s.now())
		}

		now := s.now()
		if err := tx.Tasks().Claim(ctx, task.ID, workerID, leaseID, now); err != nil {
			return err
		}

		task.State = core.TaskRunning
		task.WorkerID = workerID
		task.LeaseID = leaseID
		task.StartedAt = now

		claimed = Claimed{Task: task, Command: command}

		return nil
	})

	switch {
	case errors.Is(err, core.ErrNotFound):
		return Claimed{}, false, nil
	case errors.Is(err, core.ErrConflict):
		// Another worker got there first, or the command vanished. Look again.
		return Claimed{}, false, nil
	case err != nil:
		return Claimed{}, false, err
	}

	if claimed.Task.ID == "" {
		return Claimed{}, false, nil
	}

	s.logger.Debug("task claimed",
		"task", claimed.Task.ID, "worker", workerID, "command", claimed.Task.CommandKey)

	return claimed, true, nil
}

// verifyLease refuses a worker that is not actually holding a slot on the resource. Without this
// check, dispatch would be a way around the capacity limit.
func (s *Service) verifyLease(ctx context.Context, resourceName, workerID, leaseID string) error {
	lease, err := s.store.Leases().LeaseByID(ctx, leaseID)
	if err != nil {
		return err
	}

	switch {
	case lease.AgentID != workerID:
		return fmt.Errorf("%w: lease %s belongs to another agent", core.ErrUnauthorised, leaseID)
	case lease.ResourceName != resourceName:
		return fmt.Errorf("%w: lease %s is for %q, not %q",
			core.ErrInvalidArgument, leaseID, lease.ResourceName, resourceName)
	case !lease.Active(s.now()):
		return fmt.Errorf("%w: lease %s is no longer held", core.ErrConflict, leaseID)
	}

	return nil
}

// AppendOutput records what a running task has printed so far, so a waiting requester sees progress
// rather than silence.
func (s *Service) AppendOutput(ctx context.Context, taskID, workerID, chunk string) error {
	task, err := s.store.Tasks().ByID(ctx, taskID)
	if err != nil {
		return err
	}
	if task.WorkerID != workerID {
		return fmt.Errorf("%w: task %s is being run by another worker", core.ErrUnauthorised, taskID)
	}

	if err := s.store.Tasks().AppendOutput(ctx, taskID, chunk); err != nil {
		return err
	}

	s.outcomes.signal(taskID)

	return nil
}

// Complete records the outcome of a task.
func (s *Service) Complete(
	ctx context.Context, taskID, workerID string, exitCode int, failure string,
) (core.Task, error) {
	task, err := s.store.Tasks().ByID(ctx, taskID)
	if err != nil {
		return core.Task{}, err
	}
	if task.WorkerID != workerID {
		return core.Task{}, fmt.Errorf("%w: task %s is being run by another worker",
			core.ErrUnauthorised, taskID)
	}

	state := core.TaskSucceeded
	if exitCode != 0 || failure != "" {
		state = core.TaskFailed
	}

	if err := s.store.Tasks().Finish(ctx, taskID, state, exitCode, failure, s.now()); err != nil {
		return core.Task{}, err
	}

	s.logger.Debug("task finished", "task", taskID, "state", state, "exit_code", exitCode)
	s.outcomes.signal(taskID)

	return s.store.Tasks().ByID(ctx, taskID)
}

// Cancel withdraws a task the requester no longer wants. A task already running is left to finish:
// stopping somebody else's process mid-write is worse than letting it end.
func (s *Service) Cancel(ctx context.Context, taskID, requesterID string) (core.Task, error) {
	task, err := s.store.Tasks().ByID(ctx, taskID)
	if err != nil {
		return core.Task{}, err
	}
	if task.RequesterID != requesterID {
		return core.Task{}, fmt.Errorf("%w: task %s was submitted by another agent",
			core.ErrUnauthorised, taskID)
	}
	if task.State != core.TaskQueued {
		return core.Task{}, fmt.Errorf("%w: task %s is %s and cannot be cancelled",
			core.ErrConflict, taskID, task.State)
	}

	if err := s.store.Tasks().Finish(ctx, taskID, core.TaskCancelled, -1,
		"withdrawn by the requester", s.now()); err != nil {
		return core.Task{}, err
	}

	s.outcomes.signal(taskID)

	return s.store.Tasks().ByID(ctx, taskID)
}

// Task returns one task.
func (s *Service) Task(ctx context.Context, taskID string) (core.Task, error) {
	task, err := s.store.Tasks().ByID(ctx, taskID)
	if err != nil {
		return core.Task{}, err
	}

	return task, nil
}

// Await waits for a task to finish and returns it.
func (s *Service) Await(ctx context.Context, taskID string) (core.Task, error) {
	for {
		changed := s.outcomes.watch(taskID)

		task, err := s.store.Tasks().ByID(ctx, taskID)
		if err != nil {
			return core.Task{}, err
		}
		if task.State.Finished() {
			return task, nil
		}

		timer := time.NewTimer(recheckInterval)

		select {
		case <-changed:
			timer.Stop()
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return task, ctx.Err() //nolint:wrapcheck // the caller compares with context errors
		}
	}
}

// Mine returns a requester's recent tasks, newest first.
func (s *Service) Mine(ctx context.Context, requesterID string, limit int) ([]core.Task, error) {
	tasks, err := s.store.Tasks().ListByRequester(ctx, requesterID, limit)
	if err != nil {
		return nil, err
	}

	return tasks, nil
}

// ReleaseAbandoned puts a departed worker's tasks back in the queue. The daemon calls it when a
// worker goes stale: the work still needs doing, and another worker can pick it up.
func (s *Service) ReleaseAbandoned(ctx context.Context, workerID string) (int, error) {
	requeued, err := s.store.Tasks().ReleaseAbandoned(ctx, workerID, s.now())
	if err != nil {
		return 0, err
	}

	for _, task := range requeued {
		s.logger.Warn("requeued a task whose worker went away",
			"task", task.ID, "worker", workerID, "command", task.CommandKey)
		s.arrivals.signal(task.ResourceName)
	}

	return len(requeued), nil
}

// within reports whether path is root or sits underneath it.
func within(path, root string) bool {
	if path == root {
		return true
	}

	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}

	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// signals wakes everyone waiting on a key, the same close-a-channel broadcast the lease queue uses.
type signals struct {
	mutex    sync.Mutex
	channels map[string]chan struct{}
}

func newSignals() *signals {
	return &signals{channels: make(map[string]chan struct{})}
}

func (s *signals) watch(key string) <-chan struct{} {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	channel, found := s.channels[key]
	if !found {
		channel = make(chan struct{})
		s.channels[key] = channel
	}

	return channel
}

func (s *signals) signal(key string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if channel, found := s.channels[key]; found {
		close(channel)
		delete(s.channels, key)
	}
}
