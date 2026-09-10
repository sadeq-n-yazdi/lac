// Package leasing arbitrates the scarce things on this machine.
//
// A resource has a capacity — four concurrent test runs, one shared reviewer — and a queue. An
// agent asks for a slot, waits its turn, does its work, and gives the slot back. Two rules make
// that worth relying on:
//
//   - Capacity is never exceeded. The check for room and the grant of a slot happen inside one
//     write transaction, so two agents cannot both see the last free slot.
//   - The queue is fair. Entries are served by priority, then by the order they arrived, and only
//     the agent at the head of the queue may take a free slot.
package leasing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"code.sadeq.uk/lac/internal/auth"
	"code.sadeq.uk/lac/internal/core"
	"code.sadeq.uk/lac/internal/id"
)

// Options configure a Service.
type Options struct {
	// DefaultTimeToLive is used for resources that do not set their own.
	DefaultTimeToLive time.Duration
	// Clock defaults to the wall clock in UTC.
	Clock auth.Clock
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Service hands out and reclaims resource slots.
type Service struct {
	store   core.Store
	waiters *waiters
	now     auth.Clock
	logger  *slog.Logger
	options Options
}

// New returns a leasing service.
func New(store core.Store, options Options) *Service {
	if options.Clock == nil {
		options.Clock = func() time.Time { return time.Now().UTC() }
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.DefaultTimeToLive <= 0 {
		options.DefaultTimeToLive = 15 * time.Minute
	}

	return &Service{
		store:   store,
		waiters: newWaiters(),
		now:     options.Clock,
		logger:  options.Logger,
		options: options,
	}
}

// Define creates or reconfigures a resource.
func (s *Service) Define(ctx context.Context, actorID string, resource core.Resource) error {
	if resource.LeaseTimeToLive <= 0 {
		resource.LeaseTimeToLive = s.options.DefaultTimeToLive
	}
	if resource.CreatedAt.IsZero() {
		resource.CreatedAt = s.now()
	}

	if err := s.store.Resources().Define(ctx, resource); err != nil {
		return err
	}

	s.audit(ctx, actorID, core.AuditResourceDefined, resource.Name,
		fmt.Sprintf("capacity=%d ttl=%s", resource.Capacity, resource.LeaseTimeToLive))

	// A larger capacity means somebody waiting can start right now.
	s.waiters.signal(resource.Name)

	return nil
}

// Resource returns one resource definition.
func (s *Service) Resource(ctx context.Context, name string) (core.Resource, error) {
	resource, err := s.store.Resources().ByName(ctx, name)
	if err != nil {
		return core.Resource{}, err
	}

	return resource, nil
}

// Status describes one resource: how many slots are held and how many agents are waiting.
func (s *Service) Status(ctx context.Context, name string) (core.ResourceStatus, error) {
	resource, err := s.store.Resources().ByName(ctx, name)
	if err != nil {
		return core.ResourceStatus{}, err
	}

	return s.statusOf(ctx, resource)
}

// List describes every resource.
func (s *Service) List(ctx context.Context) ([]core.ResourceStatus, error) {
	resources, err := s.store.Resources().List(ctx)
	if err != nil {
		return nil, err
	}

	statuses := make([]core.ResourceStatus, 0, len(resources))
	for _, resource := range resources {
		status, err := s.statusOf(ctx, resource)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
	}

	return statuses, nil
}

func (s *Service) statusOf(ctx context.Context, resource core.Resource) (core.ResourceStatus, error) {
	now := s.now()

	held, err := s.store.Leases().CountActiveLeases(ctx, resource.Name, now)
	if err != nil {
		return core.ResourceStatus{}, err
	}

	waiting, err := s.store.Leases().Waiting(ctx, resource.Name)
	if err != nil {
		return core.ResourceStatus{}, err
	}

	return core.ResourceStatus{Resource: resource, ActiveLeases: held, Waiting: len(waiting)}, nil
}

// Queue returns who is waiting for a resource, in the order they will be served.
func (s *Service) Queue(ctx context.Context, resourceName string) ([]core.QueueEntry, error) {
	entries, err := s.store.Leases().Waiting(ctx, resourceName)
	if err != nil {
		return nil, err
	}

	return entries, nil
}

// Held returns the slots an agent currently holds.
func (s *Service) Held(ctx context.Context, agentID string) ([]core.Lease, error) {
	leases, err := s.store.Leases().ActiveLeasesByAgent(ctx, agentID, s.now())
	if err != nil {
		return nil, err
	}

	return leases, nil
}

// AcquireRequest asks for a slot.
type AcquireRequest struct {
	// AgentID is who is asking. It comes from the authenticated connection, never from the body.
	AgentID string
	// ResourceName is what they want a slot on.
	ResourceName string
	// Priority orders this request against the others waiting.
	Priority core.Priority
	// Reason is free-form text shown in the queue, so the operator can see who is waiting for what.
	Reason string
	// Metadata is free-form JSON attached to the lease, such as the command about to be run.
	Metadata []byte
	// NoWait asks for an immediate answer: if there is no room, the request fails with
	// ErrCapacityReached rather than joining the queue.
	NoWait bool
}

// Acquire waits for a slot on a resource and returns the lease that holds it.
//
// The caller blocks until it reaches the head of the queue and a slot frees, its context is
// cancelled, or — with NoWait — it learns immediately that the resource is full. There is no
// polling: a waiter is woken when something actually changes.
func (s *Service) Acquire(ctx context.Context, request AcquireRequest) (core.Lease, error) {
	resource, err := s.store.Resources().ByName(ctx, request.ResourceName)
	if err != nil {
		return core.Lease{}, err
	}

	entry := core.QueueEntry{
		ID:           id.New("queue"),
		ResourceName: resource.Name,
		AgentID:      request.AgentID,
		Priority:     request.Priority,
		Reason:       request.Reason,
		State:        core.QueueWaiting,
		RequestedAt:  s.now(),
	}

	if err := s.store.Leases().Enqueue(ctx, entry); err != nil {
		return core.Lease{}, err
	}
	s.audit(ctx, request.AgentID, core.AuditQueueJoined, resource.Name, request.Reason)

	lease, err := s.waitForSlot(ctx, resource, entry, request)
	if err != nil {
		// The caller's context is finished by now, so the cleanup deliberately uses its own.
		s.abandon(entry, err) //nolint:contextcheck // ctx is already cancelled; abandon makes a fresh one
		return core.Lease{}, err
	}

	return lease, nil
}

// waitForSlot is the wait loop: try to take a slot, and if there is none, sleep until something
// changes and try again.
func (s *Service) waitForSlot(
	ctx context.Context, resource core.Resource, entry core.QueueEntry, request AcquireRequest,
) (core.Lease, error) {
	for {
		// Watch before looking, so a slot that frees while we are looking still wakes us.
		changed := s.waiters.watch(resource.Name)

		lease, granted, err := s.tryGrant(ctx, entry, request.Metadata)
		if err != nil {
			return core.Lease{}, err
		}
		if granted {
			s.logger.Debug("lease granted",
				"resource", resource.Name, "agent", request.AgentID, "lease", lease.ID)

			return lease, nil
		}

		if request.NoWait {
			return core.Lease{}, fmt.Errorf("%w: %q has no free slot",
				core.ErrCapacityReached, resource.Name)
		}

		select {
		case <-changed:
		case <-ctx.Done():
			return core.Lease{}, ctx.Err() //nolint:wrapcheck // the caller compares with context errors
		case <-time.After(s.reapInterval(resource)):
			// A safety net, not the mechanism: if the holder ahead of us expires and nothing else
			// happens on this machine, we still look again shortly after.
		}
	}
}

// tryGrant takes a slot if this entry is next in line and there is room. Everything it reads and
// writes happens in one transaction, which is what makes the capacity limit real.
func (s *Service) tryGrant(ctx context.Context, entry core.QueueEntry, metadata []byte) (core.Lease, bool, error) {
	var (
		lease   core.Lease
		granted bool
	)

	err := s.store.InTransaction(ctx, func(tx core.Store) error {
		current, err := tx.Leases().QueueEntryByID(ctx, entry.ID)
		if err != nil {
			return err
		}
		if current.State != core.QueueWaiting {
			return fmt.Errorf("%w: the request is no longer waiting (%s)", core.ErrConflict, current.State)
		}

		resource, err := tx.Resources().ByName(ctx, entry.ResourceName)
		if err != nil {
			return err
		}

		now := s.now()

		held, err := tx.Leases().CountActiveLeases(ctx, resource.Name, now)
		if err != nil {
			return err
		}
		if held >= resource.Capacity {
			return nil
		}

		// Only the head of the queue may take a free slot. Without this, a request that arrived a
		// moment ago could overtake one that has been waiting for minutes.
		waiting, err := tx.Leases().Waiting(ctx, resource.Name)
		if err != nil {
			return err
		}
		if len(waiting) == 0 || waiting[0].ID != entry.ID {
			return nil
		}

		lease = core.Lease{
			ID:           id.New("lease"),
			ResourceName: resource.Name,
			AgentID:      entry.AgentID,
			QueueEntryID: entry.ID,
			AcquiredAt:   now,
			ExpiresAt:    now.Add(resource.LeaseTimeToLive),
			Metadata:     metadata,
		}

		if err := tx.Leases().CreateLease(ctx, lease); err != nil {
			return err
		}
		if err := tx.Leases().ResolveQueueEntry(ctx, entry.ID, core.QueueGranted, now); err != nil {
			return err
		}

		granted = true

		return tx.Audit().Append(ctx, core.AuditEntry{
			Actor: entry.AgentID, Action: core.AuditLeaseGranted, Target: resource.Name,
			Detail: "lease " + lease.ID, At: now,
		})
	})
	if err != nil {
		return core.Lease{}, false, err
	}

	if granted {
		// Another agent may now be at the head of the queue with room left over.
		s.waiters.signal(entry.ResourceName)
	}

	return lease, granted, nil
}

// Renew extends a slot the agent still holds.
func (s *Service) Renew(ctx context.Context, agentID, leaseID string) (core.Lease, error) {
	var renewed core.Lease

	err := s.store.InTransaction(ctx, func(tx core.Store) error {
		lease, err := tx.Leases().LeaseByID(ctx, leaseID)
		if err != nil {
			return err
		}
		if lease.AgentID != agentID {
			return notYours(leaseID)
		}

		resource, err := tx.Resources().ByName(ctx, lease.ResourceName)
		if err != nil {
			return err
		}

		now := s.now()
		expiresAt := now.Add(resource.LeaseTimeToLive)

		if err := tx.Leases().Renew(ctx, leaseID, expiresAt, now); err != nil {
			return err
		}

		lease.ExpiresAt = expiresAt
		renewed = lease

		return tx.Audit().Append(ctx, core.AuditEntry{
			Actor: agentID, Action: core.AuditLeaseRenewed, Target: lease.ResourceName,
			Detail: "lease " + leaseID, At: now,
		})
	})
	if err != nil {
		return core.Lease{}, err
	}

	return renewed, nil
}

// Release gives a slot back. Releasing a slot that was already released is not an error: a client
// that retries after a dropped connection should not have to reason about it.
func (s *Service) Release(ctx context.Context, agentID, leaseID string) error {
	var resourceName string

	err := s.store.InTransaction(ctx, func(tx core.Store) error {
		lease, err := tx.Leases().LeaseByID(ctx, leaseID)
		if err != nil {
			return err
		}
		if lease.AgentID != agentID {
			return notYours(leaseID)
		}

		resourceName = lease.ResourceName
		now := s.now()

		if err := tx.Leases().Release(ctx, leaseID, now); err != nil {
			return err
		}

		return tx.Audit().Append(ctx, core.AuditEntry{
			Actor: agentID, Action: core.AuditLeaseReleased, Target: lease.ResourceName,
			Detail: "lease " + leaseID, At: now,
		})
	})
	if err != nil {
		return err
	}

	s.logger.Debug("lease released", "resource", resourceName, "agent", agentID, "lease", leaseID)
	s.waiters.signal(resourceName)

	return nil
}

// Cancel takes an agent out of a queue it is waiting in.
func (s *Service) Cancel(ctx context.Context, agentID, entryID string) error {
	var resourceName string

	err := s.store.InTransaction(ctx, func(tx core.Store) error {
		entry, err := tx.Leases().QueueEntryByID(ctx, entryID)
		if err != nil {
			return err
		}
		if entry.AgentID != agentID {
			return fmt.Errorf("%w: queue entry %s belongs to another agent", core.ErrUnauthorised, entryID)
		}

		resourceName = entry.ResourceName
		now := s.now()

		if err := tx.Leases().ResolveQueueEntry(ctx, entryID, core.QueueCancelled, now); err != nil {
			return err
		}

		return tx.Audit().Append(ctx, core.AuditEntry{
			Actor: agentID, Action: core.AuditQueueCancelled, Target: entry.ResourceName,
			Detail: "entry " + entryID, At: now,
		})
	})
	if err != nil {
		return err
	}

	// Whoever was behind them in the queue may now be at the head.
	s.waiters.signal(resourceName)

	return nil
}

// ReleaseEverythingHeldBy gives back every slot an agent holds and takes it out of every queue.
// The registry calls this when an agent goes stale: a crashed agent must not keep the machine's
// scarce things to itself.
func (s *Service) ReleaseEverythingHeldBy(ctx context.Context, agentID, reason string) (int, error) {
	now := s.now()
	released := 0

	err := s.store.InTransaction(ctx, func(tx core.Store) error {
		leases, err := tx.Leases().ActiveLeasesByAgent(ctx, agentID, now)
		if err != nil {
			return err
		}

		for _, lease := range leases {
			if err := tx.Leases().Release(ctx, lease.ID, now); err != nil {
				return err
			}
			if err := tx.Audit().Append(ctx, core.AuditEntry{
				Actor: core.SystemActor, Action: core.AuditLeaseReleased, Target: lease.ResourceName,
				Detail: fmt.Sprintf("lease %s reclaimed: %s", lease.ID, reason), At: now,
			}); err != nil {
				return err
			}
			released++
		}

		entries, err := tx.Leases().WaitingByAgent(ctx, agentID)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := tx.Leases().ResolveQueueEntry(ctx, entry.ID, core.QueueCancelled, now); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return 0, err
	}

	if released > 0 {
		s.logger.Info("reclaimed leases", "agent", agentID, "count", released, "reason", reason)
	}
	s.waiters.signalAll()

	return released, nil
}

// ReapExpired reclaims slots whose holders went away without releasing them, and reports how many.
func (s *Service) ReapExpired(ctx context.Context) (int, error) {
	now := s.now()
	reclaimed := 0

	err := s.store.InTransaction(ctx, func(tx core.Store) error {
		expired, err := tx.Leases().ExpiredLeases(ctx, now)
		if err != nil {
			return err
		}

		for _, lease := range expired {
			if err := tx.Leases().Release(ctx, lease.ID, now); err != nil {
				return err
			}
			if err := tx.Audit().Append(ctx, core.AuditEntry{
				Actor: core.SystemActor, Action: core.AuditLeaseExpired, Target: lease.ResourceName,
				Detail: fmt.Sprintf("lease %s held by %s expired at %s",
					lease.ID, lease.AgentID, lease.ExpiresAt.Format(time.RFC3339)),
				At: now,
			}); err != nil {
				return err
			}
			reclaimed++
		}

		return nil
	})
	if err != nil {
		return 0, err
	}

	if reclaimed > 0 {
		s.logger.Warn("reclaimed expired leases", "count", reclaimed)
		s.waiters.signalAll()
	}

	return reclaimed, nil
}

// abandon takes a request out of the queue after the caller gave up, so the agents behind it are
// not left waiting for somebody who is no longer there.
func (s *Service) abandon(entry core.QueueEntry, cause error) {
	state := core.QueueCancelled
	if errors.Is(cause, core.ErrCapacityReached) {
		state = core.QueueExpired
	}

	// The caller's context is already finished, so this cleanup gets its own short one.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.store.Leases().ResolveQueueEntry(ctx, entry.ID, state, s.now()); err != nil {
		if !errors.Is(err, core.ErrConflict) && !errors.Is(err, core.ErrNotFound) {
			s.logger.Error("could not remove an abandoned queue entry",
				"entry", entry.ID, "resource", entry.ResourceName, "error", err)
		}

		return
	}

	s.waiters.signal(entry.ResourceName)
}

// reapInterval is how long a waiter sleeps before looking again even if nothing signalled it. It
// is short enough that an expiring lease is noticed promptly, and long enough not to be a poll.
func (s *Service) reapInterval(resource core.Resource) time.Duration {
	const (
		shortest = time.Second
		longest  = 30 * time.Second
	)

	interval := resource.LeaseTimeToLive / 10
	interval = min(max(interval, shortest), longest)

	return interval
}

func notYours(leaseID string) error {
	return fmt.Errorf("%w: lease %s is held by another agent", core.ErrUnauthorised, leaseID)
}

func (s *Service) audit(ctx context.Context, actor string, action core.AuditAction, target, detail string) {
	entry := core.AuditEntry{Actor: actor, Action: action, Target: target, Detail: detail, At: s.now()}

	if err := s.store.Audit().Append(ctx, entry); err != nil {
		s.logger.Error("could not write an audit entry", "action", action, "target", target, "error", err)
	}
}
