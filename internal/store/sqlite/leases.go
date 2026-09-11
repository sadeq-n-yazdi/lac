package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"sadeq.uk/lac/internal/core"
)

type leaseRepository struct{ queries querier }

var _ core.LeaseRepository = leaseRepository{}

const (
	queueEntryColumns = `id, resource_name, agent_id, priority, reason, state, requested_at, resolved_at`
	leaseColumns      = `id, resource_name, agent_id, queue_entry_id, acquired_at, expires_at, released_at, metadata`
)

func (r leaseRepository) Enqueue(ctx context.Context, entry core.QueueEntry) error {
	if err := entry.Validate(); err != nil {
		return err
	}

	_, err := r.queries.ExecContext(ctx, `
		INSERT INTO queue_entries (`+queueEntryColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.ID, entry.ResourceName, entry.AgentID, int(entry.Priority), entry.Reason,
		string(entry.State), requireMicros(entry.RequestedAt), toMicros(entry.ResolvedAt),
	)

	return translateError("queueing "+entry.AgentID+" for "+entry.ResourceName, err)
}

func (r leaseRepository) QueueEntryByID(ctx context.Context, entryID string) (core.QueueEntry, error) {
	row := r.queries.QueryRowContext(ctx, `SELECT `+queueEntryColumns+` FROM queue_entries WHERE id = ?`, entryID)

	entry, err := scanQueueEntry(row)
	if err != nil {
		return core.QueueEntry{}, translateError("reading queue entry "+entryID, err)
	}

	return entry, nil
}

// Waiting returns the queue in service order: highest priority first, then earliest request. This
// ordering is the fairness guarantee, so it lives in one query rather than in each caller.
func (r leaseRepository) Waiting(ctx context.Context, resourceName string) ([]core.QueueEntry, error) {
	rows, err := r.queries.QueryContext(ctx, `
		SELECT `+queueEntryColumns+`
		  FROM queue_entries
		 WHERE resource_name = ? AND state = ?
		 ORDER BY priority DESC, requested_at ASC, id ASC`,
		resourceName, string(core.QueueWaiting),
	)
	if err != nil {
		return nil, translateError("reading the queue for "+resourceName, err)
	}

	return collectQueueEntries(rows, "reading the queue for "+resourceName)
}

func (r leaseRepository) WaitingByAgent(ctx context.Context, agentID string) ([]core.QueueEntry, error) {
	rows, err := r.queries.QueryContext(ctx, `
		SELECT `+queueEntryColumns+`
		  FROM queue_entries
		 WHERE agent_id = ? AND state = ?
		 ORDER BY requested_at`,
		agentID, string(core.QueueWaiting),
	)
	if err != nil {
		return nil, translateError("reading the queue entries of "+agentID, err)
	}

	return collectQueueEntries(rows, "reading the queue entries of "+agentID)
}

// ResolveQueueEntry moves a waiting entry to a terminal state. An entry that already reached one
// is left alone, so a cancellation racing a grant cannot undo the grant.
func (r leaseRepository) ResolveQueueEntry(
	ctx context.Context, entryID string, state core.QueueEntryState, at time.Time,
) error {
	if !state.Terminal() {
		return fmt.Errorf("%w: %q is not a terminal queue state", core.ErrInvalidArgument, state)
	}

	result, err := r.queries.ExecContext(ctx, `
		UPDATE queue_entries SET state = ?, resolved_at = ? WHERE id = ? AND state = ?`,
		string(state), requireMicros(at), entryID, string(core.QueueWaiting),
	)
	if err != nil {
		return translateError("resolving queue entry "+entryID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("resolving queue entry %s: reading the affected row count: %w", entryID, err)
	}
	if affected == 0 {
		return fmt.Errorf("resolving queue entry %s: %w: it is no longer waiting", entryID, core.ErrConflict)
	}

	return nil
}

// CountActiveLeases is the hottest query in LAC: it runs inside the transaction that decides
// whether another agent may start work.
func (r leaseRepository) CountActiveLeases(ctx context.Context, resourceName string, at time.Time) (int, error) {
	var count int

	err := r.queries.QueryRowContext(ctx, `
		SELECT count(*) FROM leases
		 WHERE resource_name = ? AND released_at IS NULL AND expires_at > ?`,
		resourceName, requireMicros(at),
	).Scan(&count)
	if err != nil {
		return 0, translateError("counting the active leases on "+resourceName, err)
	}

	return count, nil
}

func (r leaseRepository) CreateLease(ctx context.Context, lease core.Lease) error {
	if lease.ID == "" || lease.AgentID == "" {
		return fmt.Errorf("%w: a lease needs an id and a holder", core.ErrInvalidArgument)
	}
	if !lease.ExpiresAt.After(lease.AcquiredAt) {
		return fmt.Errorf("%w: a lease must expire after it is acquired", core.ErrInvalidArgument)
	}

	_, err := r.queries.ExecContext(ctx, `
		INSERT INTO leases (`+leaseColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		lease.ID, lease.ResourceName, lease.AgentID, nullString(lease.QueueEntryID),
		requireMicros(lease.AcquiredAt), requireMicros(lease.ExpiresAt),
		toMicros(lease.ReleasedAt), lease.Metadata,
	)

	return translateError("granting a lease on "+lease.ResourceName, err)
}

func (r leaseRepository) LeaseByID(ctx context.Context, leaseID string) (core.Lease, error) {
	row := r.queries.QueryRowContext(ctx, `SELECT `+leaseColumns+` FROM leases WHERE id = ?`, leaseID)

	lease, err := scanLease(row)
	if err != nil {
		return core.Lease{}, translateError("reading lease "+leaseID, err)
	}

	return lease, nil
}

func (r leaseRepository) ActiveLeases(ctx context.Context, resourceName string, at time.Time) ([]core.Lease, error) {
	rows, err := r.queries.QueryContext(ctx, `
		SELECT `+leaseColumns+`
		  FROM leases
		 WHERE resource_name = ? AND released_at IS NULL AND expires_at > ?
		 ORDER BY acquired_at`,
		resourceName, requireMicros(at),
	)
	if err != nil {
		return nil, translateError("reading the leases on "+resourceName, err)
	}

	return collectLeases(rows, "reading the leases on "+resourceName)
}

func (r leaseRepository) ActiveLeasesByAgent(ctx context.Context, agentID string, at time.Time) ([]core.Lease, error) {
	rows, err := r.queries.QueryContext(ctx, `
		SELECT `+leaseColumns+`
		  FROM leases
		 WHERE agent_id = ? AND released_at IS NULL AND expires_at > ?
		 ORDER BY acquired_at`,
		agentID, requireMicros(at),
	)
	if err != nil {
		return nil, translateError("reading the leases held by "+agentID, err)
	}

	return collectLeases(rows, "reading the leases held by "+agentID)
}

// Renew extends a lease that is still held. A lease that was released or has already expired is
// not revived: its slot may already belong to somebody else.
func (r leaseRepository) Renew(ctx context.Context, leaseID string, expiresAt, at time.Time) error {
	result, err := r.queries.ExecContext(ctx, `
		UPDATE leases SET expires_at = ?
		 WHERE id = ? AND released_at IS NULL AND expires_at > ?`,
		requireMicros(expiresAt), leaseID, requireMicros(at),
	)
	if err != nil {
		return translateError("renewing lease "+leaseID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("renewing lease %s: reading the affected row count: %w", leaseID, err)
	}
	if affected == 0 {
		return r.explainFailedRenewal(ctx, leaseID)
	}

	return nil
}

// explainFailedRenewal tells the holder why its renewal did nothing, which is the difference
// between "your lease id is wrong" and "you lost your slot; stop working".
func (r leaseRepository) explainFailedRenewal(ctx context.Context, leaseID string) error {
	if _, err := r.LeaseByID(ctx, leaseID); err != nil {
		return err
	}

	return fmt.Errorf("renewing lease %s: %w: it has been released or has expired", leaseID, core.ErrConflict)
}

// Release ends a lease. Releasing one that is already released is not an error, so a client that
// retries after a dropped connection behaves sensibly.
func (r leaseRepository) Release(ctx context.Context, leaseID string, at time.Time) error {
	_, err := r.queries.ExecContext(ctx,
		`UPDATE leases SET released_at = ? WHERE id = ? AND released_at IS NULL`,
		requireMicros(at), leaseID,
	)

	return translateError("releasing lease "+leaseID, err)
}

// ExpiredLeases returns the leases whose holders went away without releasing them. The reaper uses
// this to give the slot to whoever is next in line.
func (r leaseRepository) ExpiredLeases(ctx context.Context, at time.Time) ([]core.Lease, error) {
	rows, err := r.queries.QueryContext(ctx, `
		SELECT `+leaseColumns+`
		  FROM leases
		 WHERE released_at IS NULL AND expires_at <= ?
		 ORDER BY expires_at`,
		requireMicros(at),
	)
	if err != nil {
		return nil, translateError("finding expired leases", err)
	}

	return collectLeases(rows, "finding expired leases")
}

func collectQueueEntries(rows *sql.Rows, operation string) ([]core.QueueEntry, error) {
	defer func() { _ = rows.Close() }()

	entries := make([]core.QueueEntry, 0, 8)
	for rows.Next() {
		entry, err := scanQueueEntry(rows)
		if err != nil {
			return nil, translateError(operation, err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, translateError(operation, err)
	}

	return entries, nil
}

func collectLeases(rows *sql.Rows, operation string) ([]core.Lease, error) {
	defer func() { _ = rows.Close() }()

	leases := make([]core.Lease, 0, 8)
	for rows.Next() {
		lease, err := scanLease(rows)
		if err != nil {
			return nil, translateError(operation, err)
		}
		leases = append(leases, lease)
	}
	if err := rows.Err(); err != nil {
		return nil, translateError(operation, err)
	}

	return leases, nil
}

func scanQueueEntry(source scanner) (core.QueueEntry, error) {
	var (
		entry       core.QueueEntry
		priority    int
		state       string
		requestedAt int64
		resolvedAt  sql.NullInt64
	)

	if err := source.Scan(&entry.ID, &entry.ResourceName, &entry.AgentID, &priority,
		&entry.Reason, &state, &requestedAt, &resolvedAt); err != nil {
		return core.QueueEntry{}, err
	}

	entry.Priority = core.Priority(priority)
	entry.State = core.QueueEntryState(state)
	entry.RequestedAt = fromRequiredMicros(requestedAt)
	entry.ResolvedAt = fromMicros(resolvedAt)

	return entry, nil
}

func scanLease(source scanner) (core.Lease, error) {
	var (
		lease        core.Lease
		queueEntryID sql.NullString
		acquiredAt   int64
		expiresAt    int64
		releasedAt   sql.NullInt64
	)

	if err := source.Scan(&lease.ID, &lease.ResourceName, &lease.AgentID, &queueEntryID,
		&acquiredAt, &expiresAt, &releasedAt, &lease.Metadata); err != nil {
		return core.Lease{}, err
	}

	lease.QueueEntryID = fromNullString(queueEntryID)
	lease.AcquiredAt = fromRequiredMicros(acquiredAt)
	lease.ExpiresAt = fromRequiredMicros(expiresAt)
	lease.ReleasedAt = fromMicros(releasedAt)

	return lease, nil
}
