package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"sadeq.uk/lac/internal/core"
)

type taskRepository struct{ queries querier }

var _ core.TaskRepository = taskRepository{}

const taskColumns = `id, resource_name, command_key, workdir, requester_id, worker_id, lease_id, ` +
	`state, exit_code, output, failure, submitted_at, started_at, finished_at`

// defaultTaskLimit caps a listing that does not ask for one.
const defaultTaskLimit = 50

func (r taskRepository) Create(ctx context.Context, task core.Task) error {
	if err := task.Validate(); err != nil {
		return err
	}

	_, err := r.queries.ExecContext(ctx, `
		INSERT INTO tasks (`+taskColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.ID, task.ResourceName, task.CommandKey, task.Workdir, task.RequesterID,
		nullString(task.WorkerID), nullString(task.LeaseID), string(task.State),
		nullExitCode(task), task.Output, task.Failure,
		requireMicros(task.SubmittedAt), toMicros(task.StartedAt), toMicros(task.FinishedAt),
	)

	return translateError("submitting a task", err)
}

func (r taskRepository) ByID(ctx context.Context, taskID string) (core.Task, error) {
	row := r.queries.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, taskID)

	task, err := scanTask(row)
	if err != nil {
		return core.Task{}, translateError("reading task "+taskID, err)
	}

	return task, nil
}

// NextQueued returns the task that has been waiting longest, which is what makes dispatch fair in
// the same way the lease queue is.
func (r taskRepository) NextQueued(ctx context.Context, resourceName string) (core.Task, error) {
	row := r.queries.QueryRowContext(ctx, `
		SELECT `+taskColumns+`
		  FROM tasks
		 WHERE resource_name = ? AND state = ?
		 ORDER BY submitted_at, id
		 LIMIT 1`,
		resourceName, string(core.TaskQueued),
	)

	task, err := scanTask(row)
	if err != nil {
		return core.Task{}, translateError("finding work for "+resourceName, err)
	}

	return task, nil
}

// Claim only succeeds while the task is still queued, so two workers reaching for the same task
// cannot both get it.
func (r taskRepository) Claim(ctx context.Context, taskID, workerID, leaseID string, at time.Time) error {
	result, err := r.queries.ExecContext(ctx, `
		UPDATE tasks
		   SET state = ?, worker_id = ?, lease_id = ?, started_at = ?
		 WHERE id = ? AND state = ?`,
		string(core.TaskRunning), workerID, nullString(leaseID), requireMicros(at),
		taskID, string(core.TaskQueued),
	)
	if err != nil {
		return translateError("claiming task "+taskID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("claiming task %s: reading the affected row count: %w", taskID, err)
	}
	if affected == 0 {
		return fmt.Errorf("claiming task %s: %w: somebody else took it", taskID, core.ErrConflict)
	}

	return nil
}

// AppendOutput adds to a running task's output, keeping the end when it grows too large.
func (r taskRepository) AppendOutput(ctx context.Context, taskID, chunk string) error {
	if chunk == "" {
		return nil
	}

	// substr with a negative start keeps the last N characters, so the cap is applied by the
	// database rather than by reading the whole output back to append to it.
	result, err := r.queries.ExecContext(ctx, `
		UPDATE tasks
		   SET output = substr(output || ?, -?)
		 WHERE id = ? AND state = ?`,
		chunk, core.MaxTaskOutput, taskID, string(core.TaskRunning),
	)
	if err != nil {
		return translateError("recording output for task "+taskID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("recording output for task %s: reading the affected row count: %w", taskID, err)
	}
	if affected == 0 {
		return fmt.Errorf("recording output for task %s: %w: it is not running", taskID, core.ErrConflict)
	}

	return nil
}

func (r taskRepository) Finish(
	ctx context.Context, taskID string, state core.TaskState, exitCode int, failure string, at time.Time,
) error {
	if !state.Finished() {
		return fmt.Errorf("%w: %q is not a finished task state", core.ErrInvalidArgument, state)
	}

	result, err := r.queries.ExecContext(ctx, `
		UPDATE tasks
		   SET state = ?, exit_code = ?, failure = ?, finished_at = ?
		 WHERE id = ? AND state IN (?, ?)`,
		string(state), exitCode, failure, requireMicros(at),
		taskID, string(core.TaskRunning), string(core.TaskQueued),
	)
	if err != nil {
		return translateError("finishing task "+taskID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("finishing task %s: reading the affected row count: %w", taskID, err)
	}
	if affected == 0 {
		return fmt.Errorf("finishing task %s: %w: it has already finished", taskID, core.ErrConflict)
	}

	return nil
}

func (r taskRepository) ListByRequester(ctx context.Context, requesterID string, limit int) ([]core.Task, error) {
	if limit <= 0 {
		limit = defaultTaskLimit
	}

	rows, err := r.queries.QueryContext(ctx, `
		SELECT `+taskColumns+`
		  FROM tasks WHERE requester_id = ?
		 ORDER BY submitted_at DESC, id DESC
		 LIMIT ?`,
		requesterID, limit,
	)
	if err != nil {
		return nil, translateError("listing tasks", err)
	}
	defer func() { _ = rows.Close() }()

	tasks := make([]core.Task, 0, 8)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, translateError("listing tasks", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, translateError("listing tasks", err)
	}

	return tasks, nil
}

// ReleaseAbandoned puts a departed worker's tasks back in the queue rather than failing them: the
// work still needs doing, and another worker can pick it up.
func (r taskRepository) ReleaseAbandoned(
	ctx context.Context, workerID string, at time.Time,
) ([]core.Task, error) {
	abandoned, err := r.runningTasksOf(ctx, workerID)
	if err != nil {
		return nil, err
	}

	// The output of a half-finished run would be misleading attached to the next attempt, so it is
	// cleared and the reason recorded in its place.
	reason := fmt.Sprintf("requeued at %s: the worker went away", at.UTC().Format(time.RFC3339))

	for _, task := range abandoned {
		if _, err := r.queries.ExecContext(ctx, `
			UPDATE tasks
			   SET state = ?, worker_id = NULL, lease_id = NULL, started_at = NULL,
			       output = '', failure = ?
			 WHERE id = ? AND state = ?`,
			string(core.TaskQueued), reason, task.ID, string(core.TaskRunning),
		); err != nil {
			return nil, translateError("requeueing task "+task.ID, err)
		}
	}

	return abandoned, nil
}

// runningTasksOf returns what a worker had in hand.
func (r taskRepository) runningTasksOf(ctx context.Context, workerID string) ([]core.Task, error) {
	rows, err := r.queries.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks WHERE worker_id = ? AND state = ?`,
		workerID, string(core.TaskRunning),
	)
	if err != nil {
		return nil, translateError("finding abandoned tasks", err)
	}
	defer func() { _ = rows.Close() }()

	tasks := make([]core.Task, 0, 4)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, translateError("finding abandoned tasks", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, translateError("finding abandoned tasks", err)
	}

	return tasks, nil
}

// nullExitCode keeps the exit status NULL until the command has actually run, so "0" never means
// "not finished yet".
func nullExitCode(task core.Task) sql.NullInt64 {
	if !task.State.Finished() {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: int64(task.ExitCode), Valid: true}
}

func scanTask(source scanner) (core.Task, error) {
	var (
		task        core.Task
		workerID    sql.NullString
		leaseID     sql.NullString
		state       string
		exitCode    sql.NullInt64
		submittedAt int64
		startedAt   sql.NullInt64
		finishedAt  sql.NullInt64
	)

	if err := source.Scan(&task.ID, &task.ResourceName, &task.CommandKey, &task.Workdir,
		&task.RequesterID, &workerID, &leaseID, &state, &exitCode, &task.Output, &task.Failure,
		&submittedAt, &startedAt, &finishedAt); err != nil {
		return core.Task{}, err
	}

	task.WorkerID = fromNullString(workerID)
	task.LeaseID = fromNullString(leaseID)
	task.State = core.TaskState(state)
	if exitCode.Valid {
		task.ExitCode = int(exitCode.Int64)
	}
	task.SubmittedAt = fromRequiredMicros(submittedAt)
	task.StartedAt = fromMicros(startedAt)
	task.FinishedAt = fromMicros(finishedAt)

	return task, nil
}
