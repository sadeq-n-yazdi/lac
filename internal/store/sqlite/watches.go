package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"sadeq.uk/lac/internal/core"
)

type watchRepository struct{ queries querier }

var _ core.WatchRepository = watchRepository{}

const watchColumns = `id, owner, repository, number, snapshot, state, draft, title, checks_state, ` +
	`unresolved, observed_at, failures, last_error, last_error_at, next_poll_at, created_at`

// defaultEventLimit caps an event listing that does not ask for one.
const defaultEventLimit = 50

func (r watchRepository) Create(ctx context.Context, watch core.Watch) error {
	if err := watch.Validate(); err != nil {
		return err
	}

	_, err := r.queries.ExecContext(ctx, `
		INSERT INTO watches (`+watchColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		watch.ID, watch.Owner, watch.Repository, watch.Number, string(watch.Snapshot),
		watch.State, boolToInt(watch.Draft), watch.Title, watch.ChecksState, watch.Unresolved,
		toMicros(watch.ObservedAt), watch.Failures, watch.LastError, toMicros(watch.LastErrorAt),
		requireMicros(watch.NextPollAt), requireMicros(watch.CreatedAt),
	)

	return translateError("watching "+watch.Reference(), err)
}

func (r watchRepository) ByID(ctx context.Context, watchID string) (core.Watch, error) {
	row := r.queries.QueryRowContext(ctx, `SELECT `+watchColumns+` FROM watches WHERE id = ?`, watchID)

	watch, err := scanWatch(row)
	if err != nil {
		return core.Watch{}, translateError("reading watch "+watchID, err)
	}

	return watch, nil
}

func (r watchRepository) ByReference(
	ctx context.Context, owner, repository string, number int,
) (core.Watch, error) {
	row := r.queries.QueryRowContext(ctx,
		`SELECT `+watchColumns+` FROM watches WHERE owner = ? AND repository = ? AND number = ?`,
		owner, repository, number)

	watch, err := scanWatch(row)
	if err != nil {
		return core.Watch{}, translateError(
			fmt.Sprintf("reading the watch on %s/%s#%d", owner, repository, number), err)
	}

	return watch, nil
}

func (r watchRepository) List(ctx context.Context) ([]core.Watch, error) {
	rows, err := r.queries.QueryContext(ctx,
		`SELECT `+watchColumns+` FROM watches ORDER BY created_at, id`)
	if err != nil {
		return nil, translateError("listing watches", err)
	}

	return collectWatches(rows, "listing watches")
}

func (r watchRepository) ListForAgent(ctx context.Context, agentID string) ([]core.Watch, error) {
	rows, err := r.queries.QueryContext(ctx, `
		SELECT `+prefixed("w", watchColumns)+`
		  FROM watches w
		  JOIN watch_subscribers s ON s.watch_id = w.id
		 WHERE s.agent_id = ?
		 ORDER BY w.created_at, w.id`,
		agentID)
	if err != nil {
		return nil, translateError("listing watches for "+agentID, err)
	}

	return collectWatches(rows, "listing watches for "+agentID)
}

// Due returns what should be looked at now, soonest first, so a backlog is worked through in the
// order it built up.
func (r watchRepository) Due(ctx context.Context, at time.Time, limit int) ([]core.Watch, error) {
	if limit <= 0 {
		limit = 20
	}

	rows, err := r.queries.QueryContext(ctx, `
		SELECT `+watchColumns+`
		  FROM watches WHERE next_poll_at <= ?
		 ORDER BY next_poll_at
		 LIMIT ?`,
		requireMicros(at), limit)
	if err != nil {
		return nil, translateError("finding watches to poll", err)
	}

	return collectWatches(rows, "finding watches to poll")
}

// RecordObservation stores a successful poll. The failure count is cleared, because GitHub answered.
func (r watchRepository) RecordObservation(ctx context.Context, watch core.Watch) error {
	result, err := r.queries.ExecContext(ctx, `
		UPDATE watches
		   SET snapshot = ?, state = ?, draft = ?, title = ?, checks_state = ?, unresolved = ?,
		       observed_at = ?, failures = 0, last_error = '', next_poll_at = ?
		 WHERE id = ?`,
		string(watch.Snapshot), watch.State, boolToInt(watch.Draft), watch.Title,
		watch.ChecksState, watch.Unresolved, requireMicros(watch.ObservedAt),
		requireMicros(watch.NextPollAt), watch.ID,
	)

	return affectedOrNotFound("recording an observation of "+watch.Reference(), result, err)
}

// RecordFailure notes that GitHub could not be reached. The last good snapshot is left exactly as it
// was: an agent asking during an outage should get the last thing that was true, marked as old.
func (r watchRepository) RecordFailure(
	ctx context.Context, watchID, reason string, at, nextPollAt time.Time,
) error {
	result, err := r.queries.ExecContext(ctx, `
		UPDATE watches
		   SET failures = failures + 1, last_error = ?, last_error_at = ?, next_poll_at = ?
		 WHERE id = ?`,
		reason, requireMicros(at), requireMicros(nextPollAt), watchID,
	)

	return affectedOrNotFound("recording a failure for watch "+watchID, result, err)
}

func (r watchRepository) Delete(ctx context.Context, watchID string) error {
	result, err := r.queries.ExecContext(ctx, `DELETE FROM watches WHERE id = ?`, watchID)

	return affectedOrNotFound("removing watch "+watchID, result, err)
}

func (r watchRepository) Subscribe(ctx context.Context, watchID, agentID string, at time.Time) error {
	_, err := r.queries.ExecContext(ctx,
		`INSERT OR IGNORE INTO watch_subscribers (watch_id, agent_id, subscribed_at) VALUES (?, ?, ?)`,
		watchID, agentID, requireMicros(at))

	return translateError("subscribing "+agentID+" to watch "+watchID, err)
}

func (r watchRepository) Unsubscribe(ctx context.Context, watchID, agentID string) error {
	_, err := r.queries.ExecContext(ctx,
		`DELETE FROM watch_subscribers WHERE watch_id = ? AND agent_id = ?`, watchID, agentID)

	return translateError("unsubscribing "+agentID+" from watch "+watchID, err)
}

func (r watchRepository) Subscribers(ctx context.Context, watchID string) ([]string, error) {
	rows, err := r.queries.QueryContext(ctx,
		`SELECT agent_id FROM watch_subscribers WHERE watch_id = ? ORDER BY subscribed_at, agent_id`,
		watchID)
	if err != nil {
		return nil, translateError("listing the subscribers of watch "+watchID, err)
	}
	defer func() { _ = rows.Close() }()

	subscribers := make([]string, 0, 4)
	for rows.Next() {
		var agentID string
		if err := rows.Scan(&agentID); err != nil {
			return nil, translateError("listing the subscribers of watch "+watchID, err)
		}
		subscribers = append(subscribers, agentID)
	}
	if err := rows.Err(); err != nil {
		return nil, translateError("listing the subscribers of watch "+watchID, err)
	}

	return subscribers, nil
}

func (r watchRepository) AppendEvent(ctx context.Context, event core.WatchEvent) error {
	if event.WatchID == "" || event.Kind == "" {
		return fmt.Errorf("%w: an event needs a watch and a kind", core.ErrInvalidArgument)
	}

	_, err := r.queries.ExecContext(ctx,
		`INSERT INTO watch_events (watch_id, kind, summary, at) VALUES (?, ?, ?, ?)`,
		event.WatchID, string(event.Kind), event.Summary, requireMicros(event.At))

	return translateError("recording a change to watch "+event.WatchID, err)
}

func (r watchRepository) Events(ctx context.Context, watchID string, limit int) ([]core.WatchEvent, error) {
	if limit <= 0 {
		limit = defaultEventLimit
	}

	rows, err := r.queries.QueryContext(ctx, `
		SELECT id, watch_id, kind, summary, at
		  FROM watch_events WHERE watch_id = ?
		 ORDER BY id DESC
		 LIMIT ?`,
		watchID, limit)
	if err != nil {
		return nil, translateError("reading the changes to watch "+watchID, err)
	}
	defer func() { _ = rows.Close() }()

	events := make([]core.WatchEvent, 0, 8)
	for rows.Next() {
		var (
			event core.WatchEvent
			kind  string
			at    int64
		)
		if err := rows.Scan(&event.ID, &event.WatchID, &kind, &event.Summary, &at); err != nil {
			return nil, translateError("reading the changes to watch "+watchID, err)
		}
		event.Kind = core.WatchEventKind(kind)
		event.At = fromRequiredMicros(at)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, translateError("reading the changes to watch "+watchID, err)
	}

	return events, nil
}

func collectWatches(rows *sql.Rows, operation string) ([]core.Watch, error) {
	defer func() { _ = rows.Close() }()

	watches := make([]core.Watch, 0, 8)
	for rows.Next() {
		watch, err := scanWatch(rows)
		if err != nil {
			return nil, translateError(operation, err)
		}
		watches = append(watches, watch)
	}
	if err := rows.Err(); err != nil {
		return nil, translateError(operation, err)
	}

	return watches, nil
}

func scanWatch(source scanner) (core.Watch, error) {
	var (
		watch       core.Watch
		snapshot    string
		draft       int
		observedAt  sql.NullInt64
		lastErrorAt sql.NullInt64
		nextPollAt  int64
		createdAt   int64
	)

	if err := source.Scan(&watch.ID, &watch.Owner, &watch.Repository, &watch.Number, &snapshot,
		&watch.State, &draft, &watch.Title, &watch.ChecksState, &watch.Unresolved,
		&observedAt, &watch.Failures, &watch.LastError, &lastErrorAt, &nextPollAt, &createdAt); err != nil {
		return core.Watch{}, err
	}

	if snapshot != "" {
		watch.Snapshot = []byte(snapshot)
	}
	watch.Draft = draft != 0
	watch.ObservedAt = fromMicros(observedAt)
	watch.LastErrorAt = fromMicros(lastErrorAt)
	watch.NextPollAt = fromRequiredMicros(nextPollAt)
	watch.CreatedAt = fromRequiredMicros(createdAt)

	return watch, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}

	return 0
}
