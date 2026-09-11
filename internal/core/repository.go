package core

import (
	"context"
	"time"
)

// Store is the whole persistence surface. Services depend on this interface, never on a database
// driver, so the business rules can be tested against any implementation.
//
// InTransaction is what makes the lease grant safe: the service composes several repository calls
// inside one write transaction, so capacity can never be exceeded by a race between two agents.
type Store interface {
	Agents() AgentRepository
	Credentials() CredentialRepository
	Messages() MessageRepository
	Resources() ResourceRepository
	Leases() LeaseRepository
	Reports() ReportRepository
	Tasks() TaskRepository
	Watches() WatchRepository
	Audit() AuditLog

	// InTransaction runs fn against a Store bound to a single write transaction. The transaction
	// commits when fn returns nil and rolls back on any error or panic. Implementations must take
	// the write lock immediately rather than on first write, so read-then-write logic is safe.
	InTransaction(ctx context.Context, fn func(tx Store) error) error

	// Close releases the underlying resources.
	Close() error
}

// AgentFilter narrows a listing of agents. The zero value matches every agent.
type AgentFilter struct {
	// States, when non-empty, keeps only agents in one of these states.
	States []AgentState
	// Kinds, when non-empty, keeps only agents of one of these kinds.
	Kinds []string
}

// AgentRepository stores the agent roster.
type AgentRepository interface {
	// Create stores a new agent, returning ErrAlreadyExists if the name is taken by a live agent.
	Create(ctx context.Context, agent Agent) error
	// Update replaces a stored agent, returning ErrNotFound if it is gone.
	Update(ctx context.Context, agent Agent) error
	// ByID returns one agent, or ErrNotFound.
	ByID(ctx context.Context, agentID string) (Agent, error)
	// ByName returns the active agent with this name, or ErrNotFound. It is the registration check:
	// two agents may not answer to one name at the same time.
	ByName(ctx context.Context, name string) (Agent, error)
	// FindForDelivery returns the agent a message addressed to this name should go to, preferring
	// an active one but accepting a stale one.
	//
	// A stale agent is quiet, not gone: its inbox should still accumulate, or "the message waits for
	// the recipient" stops being true for exactly the agent most likely to need it. A deregistered
	// agent said goodbye, and is not addressable.
	FindForDelivery(ctx context.Context, name string) (Agent, error)
	// List returns the agents matching the filter, oldest registration first.
	List(ctx context.Context, filter AgentFilter) ([]Agent, error)
	// Heartbeat records that the agent is still alive, returning ErrNotFound if it is not
	// registered and ErrUnauthorised if it has been deregistered.
	Heartbeat(ctx context.Context, agentID string, at time.Time) error
	// SetState moves an agent to a new presence state.
	SetState(ctx context.Context, agentID string, state AgentState, at time.Time) error
	// StaleBefore returns the active agents whose last heartbeat predates the cutoff.
	StaleBefore(ctx context.Context, cutoff time.Time) ([]Agent, error)
}

// CredentialRepository stores agent authentication secrets.
type CredentialRepository interface {
	// Store saves a credential, replacing any previous one for the same agent.
	Store(ctx context.Context, credential Credential) error
	// ByTokenHash returns the credential with this hash, or ErrNotFound. Implementations must look
	// the hash up directly rather than scanning and comparing in application code.
	ByTokenHash(ctx context.Context, tokenHash []byte) (Credential, error)
	// Revoke invalidates an agent's credential. Revoking an unknown agent is not an error, so that
	// deregistration is idempotent.
	Revoke(ctx context.Context, agentID string, at time.Time) error
}

// MessageRepository stores messages and their per-recipient deliveries.
type MessageRepository interface {
	// Append stores a message together with one pending delivery per recipient, atomically.
	Append(ctx context.Context, message Message, recipientIDs []string) error
	// Pending returns up to limit messages this agent has not acknowledged, oldest first.
	Pending(ctx context.Context, agentID string, limit int) ([]Message, error)
	// MarkDelivered records that the agent has seen these messages.
	MarkDelivered(ctx context.Context, agentID string, messageIDs []string, at time.Time) error
	// Acknowledge marks deliveries complete and returns how many it changed. Acknowledging a
	// message addressed to another agent must not change anything.
	Acknowledge(ctx context.Context, agentID string, messageIDs []string, at time.Time) (int, error)
	// PruneExpired removes fully acknowledged messages and any that passed their expiry.
	PruneExpired(ctx context.Context, at time.Time) (int, error)

	// Subscribe adds an agent to a topic. Subscribing twice is not an error.
	Subscribe(ctx context.Context, agentID, topic string) error
	// Unsubscribe removes an agent from a topic. Removing a subscription that is not there is not an error.
	Unsubscribe(ctx context.Context, agentID, topic string) error
	// Subscribers returns the agent ids subscribed to a topic.
	Subscribers(ctx context.Context, topic string) ([]string, error)
}

// ResourceRepository stores resource definitions.
type ResourceRepository interface {
	// Define creates or reconfigures a resource.
	Define(ctx context.Context, resource Resource) error
	// ByName returns one resource, or ErrNotFound.
	ByName(ctx context.Context, name string) (Resource, error)
	// List returns every resource, by name.
	List(ctx context.Context) ([]Resource, error)
	// Delete removes a resource. Removing one that does not exist returns ErrNotFound.
	Delete(ctx context.Context, name string) error
}

// LeaseRepository stores the queue and the leases granted from it. Its methods are deliberately
// small so the leasing service can compose them inside one transaction and keep the rules — who is
// next, and whether there is room — in the service layer where they can be read and tested.
type LeaseRepository interface {
	// Enqueue adds a waiting entry to a resource's queue.
	Enqueue(ctx context.Context, entry QueueEntry) error
	// QueueEntryByID returns one entry, or ErrNotFound.
	QueueEntryByID(ctx context.Context, entryID string) (QueueEntry, error)
	// Waiting returns the resource's waiting entries in service order: highest priority first,
	// then earliest request first.
	Waiting(ctx context.Context, resourceName string) ([]QueueEntry, error)
	// WaitingByAgent returns every waiting entry belonging to an agent, across all resources.
	WaitingByAgent(ctx context.Context, agentID string) ([]QueueEntry, error)
	// ResolveQueueEntry moves an entry to a terminal state.
	ResolveQueueEntry(ctx context.Context, entryID string, state QueueEntryState, at time.Time) error

	// CountActiveLeases returns how many slots on the resource are held at the given instant.
	CountActiveLeases(ctx context.Context, resourceName string, at time.Time) (int, error)
	// CreateLease stores a granted lease.
	CreateLease(ctx context.Context, lease Lease) error
	// LeaseByID returns one lease, or ErrNotFound.
	LeaseByID(ctx context.Context, leaseID string) (Lease, error)
	// ActiveLeases returns the leases holding a slot on the resource at the given instant.
	ActiveLeases(ctx context.Context, resourceName string, at time.Time) ([]Lease, error)
	// ActiveLeasesByAgent returns every slot an agent holds, across all resources.
	ActiveLeasesByAgent(ctx context.Context, agentID string, at time.Time) ([]Lease, error)
	// Renew extends a lease's expiry, returning ErrNotFound if it is unknown and ErrConflict if it
	// has already been released or has expired.
	Renew(ctx context.Context, leaseID string, expiresAt, at time.Time) error
	// Release ends a lease. Releasing an already-released lease is not an error, so a client that
	// retries after a dropped connection behaves sensibly.
	Release(ctx context.Context, leaseID string, at time.Time) error
	// ExpiredLeases returns leases that passed their expiry without being released or renewed.
	ExpiredLeases(ctx context.Context, at time.Time) ([]Lease, error)
}

// TaskRepository stores dispatched work.
type TaskRepository interface {
	// Create stores a queued task.
	Create(ctx context.Context, task Task) error
	// ByID returns one task, or ErrNotFound.
	ByID(ctx context.Context, taskID string) (Task, error)
	// NextQueued returns the oldest queued task for a resource, or ErrNotFound if there is none.
	NextQueued(ctx context.Context, resourceName string) (Task, error)
	// Claim marks a task as running for a worker holding a lease. It only succeeds while the task
	// is still queued, so two workers racing for the same task cannot both get it.
	Claim(ctx context.Context, taskID, workerID, leaseID string, at time.Time) error
	// AppendOutput adds to a running task's output, so a waiting requester sees progress rather
	// than silence.
	AppendOutput(ctx context.Context, taskID, chunk string) error
	// Finish records the outcome of a task.
	Finish(ctx context.Context, taskID string, state TaskState, exitCode int, failure string, at time.Time) error
	// ListByRequester returns a requester's recent tasks, newest first.
	ListByRequester(ctx context.Context, requesterID string, limit int) ([]Task, error)
	// ReleaseAbandoned moves a worker's running tasks back to the queue, for when that worker went
	// away without finishing them. It returns the tasks it requeued.
	ReleaseAbandoned(ctx context.Context, workerID string, at time.Time) ([]Task, error)
}

// WatchRepository stores watched pull requests, who is listening, and what changed.
type WatchRepository interface {
	// Create stores a new watch.
	Create(ctx context.Context, watch Watch) error
	// ByID returns one watch, or ErrNotFound.
	ByID(ctx context.Context, watchID string) (Watch, error)
	// ByReference returns the watch for a pull request, or ErrNotFound.
	ByReference(ctx context.Context, owner, repository string, number int) (Watch, error)
	// List returns every watch, oldest first.
	List(ctx context.Context) ([]Watch, error)
	// ListForAgent returns the watches an agent subscribed to.
	ListForAgent(ctx context.Context, agentID string) ([]Watch, error)
	// Due returns the watches whose next poll is at or before the given instant, soonest first.
	Due(ctx context.Context, at time.Time, limit int) ([]Watch, error)
	// RecordObservation stores a successful poll: the snapshot, what was copied out of it, and when
	// to look again. It clears the failure count, because GitHub answered.
	RecordObservation(ctx context.Context, watch Watch) error
	// RecordFailure records that GitHub could not be reached, increments the failure count and sets
	// the next attempt. The last good snapshot is left exactly as it was.
	RecordFailure(ctx context.Context, watchID, reason string, at, nextPollAt time.Time) error
	// Delete removes a watch and everything attached to it.
	Delete(ctx context.Context, watchID string) error

	// Subscribe adds an agent to a watch. Subscribing twice is not an error.
	Subscribe(ctx context.Context, watchID, agentID string, at time.Time) error
	// Unsubscribe removes an agent. Removing one that is not subscribed is not an error.
	Unsubscribe(ctx context.Context, watchID, agentID string) error
	// Subscribers returns the agents listening to a watch.
	Subscribers(ctx context.Context, watchID string) ([]string, error)

	// AppendEvent records a change.
	AppendEvent(ctx context.Context, event WatchEvent) error
	// Events returns the changes to a watch, newest first.
	Events(ctx context.Context, watchID string, limit int) ([]WatchEvent, error)
}

// ReportRepository stores report requests and the answers to them.
type ReportRepository interface {
	// CreateRequest stores a broadcast report request.
	CreateRequest(ctx context.Context, request ReportRequest) error
	// RequestByID returns one request, or ErrNotFound.
	RequestByID(ctx context.Context, requestID string) (ReportRequest, error)
	// AddReport stores one agent's answer, replacing an earlier answer from the same agent.
	AddReport(ctx context.Context, report Report) error
	// ReportsFor returns the answers to a request, in arrival order.
	ReportsFor(ctx context.Context, requestID string) ([]Report, error)
}

// AuditFilter narrows an audit listing. The zero value matches everything.
type AuditFilter struct {
	// Actor, when set, keeps only entries by this agent.
	Actor string
	// Actions, when non-empty, keeps only these actions.
	Actions []AuditAction
	// Since, when non-zero, keeps only entries at or after this instant.
	Since time.Time
	// Limit caps the number of entries returned. Zero means the implementation's default.
	Limit int
}

// AuditLog is the append-only record of state changes. It has no update or delete.
type AuditLog interface {
	// Append records one entry.
	Append(ctx context.Context, entry AuditEntry) error
	// List returns matching entries, newest first.
	List(ctx context.Context, filter AuditFilter) ([]AuditEntry, error)
}
