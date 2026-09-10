package core

import (
	"fmt"
	"time"
)

// MaxCapacity bounds a resource's capacity. It is a guard against a typo turning a shared slot
// into no limit at all, which is exactly the failure LAC exists to prevent.
const MaxCapacity = 1024

// Resource is a scarce local thing that only so many agents may use at once, such as the four
// concurrent test runs a laptop tolerates.
type Resource struct {
	// Name identifies the resource and is what agents queue for.
	Name string
	// Capacity is how many leases may be active at the same time.
	Capacity int
	// LeaseTimeToLive is how long a lease survives without a renewal. It bounds the damage of an
	// agent that crashes while holding a slot.
	LeaseTimeToLive time.Duration
	// Description is free-form text shown to operators and agents.
	Description string
	// CreatedAt is when the resource was defined.
	CreatedAt time.Time
}

// Validate reports whether the resource is well formed enough to store.
func (r Resource) Validate() error {
	if err := ValidateName("resource name", r.Name); err != nil {
		return err
	}
	if r.Capacity < 1 || r.Capacity > MaxCapacity {
		return fmt.Errorf("%w: resource %q capacity must be between 1 and %d, got %d",
			ErrInvalidArgument, r.Name, MaxCapacity, r.Capacity)
	}
	if r.LeaseTimeToLive <= 0 {
		return fmt.Errorf("%w: resource %q needs a positive lease time to live", ErrInvalidArgument, r.Name)
	}
	return nil
}

// ResourceStatus is a point-in-time view of a resource, for the CLI and the operator.
type ResourceStatus struct {
	// Resource is the definition being reported on.
	Resource Resource
	// ActiveLeases is how many slots are held right now.
	ActiveLeases int
	// Waiting is how many agents are queued for a slot.
	Waiting int
}

// Free reports how many slots are available right now.
func (s ResourceStatus) Free() int {
	free := s.Resource.Capacity - s.ActiveLeases
	if free < 0 {
		return 0
	}
	return free
}

// QueueEntryState is where a request for a slot has got to.
type QueueEntryState string

const (
	// QueueWaiting means the agent is still in line.
	QueueWaiting QueueEntryState = "waiting"
	// QueueGranted means the agent was given a slot; a lease exists.
	QueueGranted QueueEntryState = "granted"
	// QueueCancelled means the agent withdrew, or its connection went away.
	QueueCancelled QueueEntryState = "cancelled"
	// QueueExpired means the agent waited longer than it was willing to.
	QueueExpired QueueEntryState = "expired"
)

// Valid reports whether the state is one this domain defines.
func (s QueueEntryState) Valid() bool {
	return s == QueueWaiting || s == QueueGranted || s == QueueCancelled || s == QueueExpired
}

// Terminal reports whether the entry has reached a state it will not leave.
func (s QueueEntryState) Terminal() bool { return s != QueueWaiting }

// Priority orders the queue. Higher values are served first; equal values are served in request
// order, which is what makes the queue fair by default.
type Priority int

const (
	// PriorityBackground is for work nobody is waiting on, such as a speculative build.
	PriorityBackground Priority = -10
	// PriorityNormal is the default for agent work.
	PriorityNormal Priority = 0
	// PriorityInteractive is for a request the operator is sitting in front of.
	PriorityInteractive Priority = 10
)

// QueueEntry is one agent's place in line for a resource.
type QueueEntry struct {
	// ID is assigned by the daemon.
	ID string
	// ResourceName is the resource being queued for.
	ResourceName string
	// AgentID is the agent waiting.
	AgentID string
	// Priority orders this entry against the others.
	Priority Priority
	// Reason is free-form text the agent supplies, shown in the queue so the operator can see who
	// is waiting for what.
	Reason string
	// State is where the request has got to.
	State QueueEntryState
	// RequestedAt is when the agent joined the queue, and the tie-breaker for equal priorities.
	RequestedAt time.Time
	// ResolvedAt is when the entry reached a terminal state. Zero while waiting.
	ResolvedAt time.Time
}

// Validate reports whether the queue entry is well formed enough to store.
func (q QueueEntry) Validate() error {
	if q.ID == "" {
		return fmt.Errorf("%w: queue entry id is required", ErrInvalidArgument)
	}
	if err := ValidateName("resource name", q.ResourceName); err != nil {
		return err
	}
	if q.AgentID == "" {
		return fmt.Errorf("%w: queue entry agent is required", ErrInvalidArgument)
	}
	if !q.State.Valid() {
		return fmt.Errorf("%w: unknown queue entry state %q", ErrInvalidArgument, q.State)
	}
	return nil
}

// Lease is a granted slot on a resource. Holding one is what entitles an agent to run its work.
type Lease struct {
	// ID is assigned by the daemon and is what the holder presents to renew or release.
	ID string
	// ResourceName is the resource this lease is a slot on.
	ResourceName string
	// AgentID is the holder.
	AgentID string
	// QueueEntryID links back to the request that produced this lease.
	QueueEntryID string
	// AcquiredAt is when the slot was granted.
	AcquiredAt time.Time
	// ExpiresAt is when the slot is reclaimed unless the holder renews.
	ExpiresAt time.Time
	// ReleasedAt is when the holder gave the slot back. Zero while the lease is active.
	ReleasedAt time.Time
	// Metadata is free-form JSON the holder attaches, such as the command it is running.
	Metadata []byte
}

// Active reports whether the lease still occupies a slot at the given instant.
func (l Lease) Active(at time.Time) bool {
	return l.ReleasedAt.IsZero() && at.Before(l.ExpiresAt)
}

// Expired reports whether the lease ran out without being released or renewed.
func (l Lease) Expired(at time.Time) bool {
	return l.ReleasedAt.IsZero() && !at.Before(l.ExpiresAt)
}
