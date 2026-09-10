package leasing_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"code.sadeq.uk/lac/internal/core"
	"code.sadeq.uk/lac/internal/id"
	"code.sadeq.uk/lac/internal/service/leasing"
	"code.sadeq.uk/lac/internal/store/sqlite"
)

var baseTime = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

// clock is a time source the test drives, so expiry can be tested without waiting for it.
type clock struct {
	mutex sync.Mutex
	now   time.Time
}

func (c *clock) Now() time.Time {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	return c.now
}

func (c *clock) advance(by time.Duration) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.now = c.now.Add(by)
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type harness struct {
	service *leasing.Service
	store   *sqlite.Store
	clock   *clock
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "lac.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	tick := &clock{now: baseTime}
	service := leasing.New(store, leasing.Options{
		DefaultTimeToLive: 15 * time.Minute,
		Clock:             tick.Now,
		Logger:            quietLogger(),
	})

	return &harness{service: service, store: store, clock: tick}
}

func (h *harness) defineResource(t *testing.T, name string, capacity int, timeToLive time.Duration) {
	t.Helper()

	err := h.service.Define(t.Context(), "operator", core.Resource{
		Name: name, Capacity: capacity, LeaseTimeToLive: timeToLive,
	})
	if err != nil {
		t.Fatalf("Define(%q) = %v, want nil", name, err)
	}
}

func (h *harness) newAgent(t *testing.T, name string) core.Agent {
	t.Helper()

	agent := core.Agent{
		ID: id.New("agent"), Name: name, Kind: "claude", Workdir: "/tmp/" + name,
		Capabilities: core.Capabilities{Resources: []string{core.WildcardResource}},
		State:        core.AgentActive, RegisteredAt: baseTime, LastHeartbeatAt: baseTime,
	}
	if err := h.store.Agents().Create(t.Context(), agent); err != nil {
		t.Fatalf("creating the agent: %v", err)
	}

	return agent
}

func (h *harness) acquire(t *testing.T, agent core.Agent, resource string) core.Lease {
	t.Helper()

	lease, err := h.service.Acquire(t.Context(), leasing.AcquireRequest{
		AgentID: agent.ID, ResourceName: resource, Reason: "running tests",
	})
	if err != nil {
		t.Fatalf("Acquire(%q) = %v, want nil", resource, err)
	}

	return lease
}

// The promise the whole project makes: however many agents ask at once, only as many as the
// capacity allows are ever running.
func TestCapacityIsNeverExceeded(t *testing.T) {
	const (
		capacity   = 4
		contenders = 12
	)

	subject := newHarness(t)
	subject.defineResource(t, "test", capacity, 15*time.Minute)

	var (
		concurrent atomic.Int64
		observed   atomic.Int64
		wait       sync.WaitGroup
	)

	start := make(chan struct{})

	for index := range contenders {
		agent := subject.newAgent(t, "claude-"+string(rune('a'+index)))

		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start

			lease, err := subject.service.Acquire(t.Context(), leasing.AcquireRequest{
				AgentID: agent.ID, ResourceName: "test", Reason: "running tests",
			})
			if err != nil {
				t.Errorf("Acquire() = %v, want nil", err)
				return
			}

			// Hold the slot briefly, recording the high-water mark of simultaneous holders.
			held := concurrent.Add(1)
			for {
				seen := observed.Load()
				if held <= seen || observed.CompareAndSwap(seen, held) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			concurrent.Add(-1)

			if err := subject.service.Release(t.Context(), agent.ID, lease.ID); err != nil {
				t.Errorf("Release() = %v, want nil", err)
			}
		}()
	}

	close(start)
	wait.Wait()

	if peak := observed.Load(); peak > capacity {
		t.Errorf("%d agents held the resource at once, want at most %d", peak, capacity)
	}
	if peak := observed.Load(); peak == 0 {
		t.Error("no agent ever held the resource")
	}
}

// Fairness: with equal priority, the agent that asked first is served first.
func TestQueueIsServedInOrder(t *testing.T) {
	subject := newHarness(t)
	subject.defineResource(t, "test", 1, 15*time.Minute)

	holder := subject.newAgent(t, "holder")
	first := subject.newAgent(t, "first")
	second := subject.newAgent(t, "second")

	held := subject.acquire(t, holder, "test")

	granted := make(chan string, 2)
	waiterReady := make(chan struct{})

	go func() {
		close(waiterReady)
		lease, err := subject.service.Acquire(t.Context(), leasing.AcquireRequest{
			AgentID: first.ID, ResourceName: "test",
		})
		if err != nil {
			t.Errorf("the first waiter failed: %v", err)
			return
		}
		granted <- "first"
		_ = subject.service.Release(t.Context(), first.ID, lease.ID)
	}()

	<-waiterReady
	waitUntilQueued(t, subject, "test", 1)

	go func() {
		lease, err := subject.service.Acquire(t.Context(), leasing.AcquireRequest{
			AgentID: second.ID, ResourceName: "test",
		})
		if err != nil {
			t.Errorf("the second waiter failed: %v", err)
			return
		}
		granted <- "second"
		_ = subject.service.Release(t.Context(), second.ID, lease.ID)
	}()

	waitUntilQueued(t, subject, "test", 2)

	if err := subject.service.Release(t.Context(), holder.ID, held.ID); err != nil {
		t.Fatalf("Release() = %v, want nil", err)
	}

	if got := receive(t, granted); got != "first" {
		t.Errorf("%q was served first, want the agent that asked first", got)
	}
	if got := receive(t, granted); got != "second" {
		t.Errorf("%q was served second", got)
	}
}

// An interactive request — the operator sitting in front of the screen — goes ahead of background
// work that is already waiting.
func TestHigherPriorityIsServedFirst(t *testing.T) {
	subject := newHarness(t)
	subject.defineResource(t, "test", 1, 15*time.Minute)

	holder := subject.newAgent(t, "holder")
	background := subject.newAgent(t, "background")
	interactive := subject.newAgent(t, "interactive")

	held := subject.acquire(t, holder, "test")
	granted := make(chan string, 2)

	go func() {
		lease, err := subject.service.Acquire(t.Context(), leasing.AcquireRequest{
			AgentID: background.ID, ResourceName: "test", Priority: core.PriorityBackground,
		})
		if err != nil {
			t.Errorf("the background waiter failed: %v", err)
			return
		}
		granted <- "background"
		_ = subject.service.Release(t.Context(), background.ID, lease.ID)
	}()

	waitUntilQueued(t, subject, "test", 1)

	go func() {
		lease, err := subject.service.Acquire(t.Context(), leasing.AcquireRequest{
			AgentID: interactive.ID, ResourceName: "test", Priority: core.PriorityInteractive,
		})
		if err != nil {
			t.Errorf("the interactive waiter failed: %v", err)
			return
		}
		granted <- "interactive"
		_ = subject.service.Release(t.Context(), interactive.ID, lease.ID)
	}()

	waitUntilQueued(t, subject, "test", 2)

	if err := subject.service.Release(t.Context(), holder.ID, held.ID); err != nil {
		t.Fatalf("Release() = %v, want nil", err)
	}

	if got := receive(t, granted); got != "interactive" {
		t.Errorf("%q was served first, want the interactive request to overtake the background one", got)
	}
	// Wait for the background request too, so it finishes inside the test rather than reporting
	// against a test that has already returned.
	if got := receive(t, granted); got != "background" {
		t.Errorf("%q was served second, want the background request", got)
	}
}

// NoWait is for a caller that would rather do something else than queue.
func TestNoWaitFailsImmediatelyWhenFull(t *testing.T) {
	subject := newHarness(t)
	subject.defineResource(t, "test", 1, 15*time.Minute)

	holder := subject.newAgent(t, "holder")
	hopeful := subject.newAgent(t, "hopeful")
	subject.acquire(t, holder, "test")

	_, err := subject.service.Acquire(t.Context(), leasing.AcquireRequest{
		AgentID: hopeful.ID, ResourceName: "test", NoWait: true,
	})
	if !errors.Is(err, core.ErrCapacityReached) {
		t.Fatalf("Acquire(NoWait) = %v, want ErrCapacityReached", err)
	}

	// And it must not leave a ghost in the queue.
	queue, err := subject.service.Queue(t.Context(), "test")
	if err != nil {
		t.Fatalf("Queue() = %v, want nil", err)
	}
	if len(queue) != 0 {
		t.Errorf("the queue holds %d entries after a NoWait failure, want 0", len(queue))
	}
}

// An agent that gives up — a cancelled call, a crashed session — must not leave the agents behind
// it waiting for somebody who is no longer there.
func TestGivingUpLeavesTheQueue(t *testing.T) {
	subject := newHarness(t)
	subject.defineResource(t, "test", 1, 15*time.Minute)

	holder := subject.newAgent(t, "holder")
	quitter := subject.newAgent(t, "quitter")
	subject.acquire(t, holder, "test")

	ctx, cancel := context.WithCancel(t.Context())
	failed := make(chan error, 1)

	go func() {
		_, err := subject.service.Acquire(ctx, leasing.AcquireRequest{
			AgentID: quitter.ID, ResourceName: "test",
		})
		failed <- err
	}()

	waitUntilQueued(t, subject, "test", 1)
	cancel()

	select {
	case err := <-failed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Acquire() = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire() did not return when its caller gave up")
	}

	waitUntilQueued(t, subject, "test", 0)
}

// A crashed holder must lose its slot, or the resource is stuck until somebody restarts the daemon.
func TestExpiredLeasesAreReclaimed(t *testing.T) {
	subject := newHarness(t)
	subject.defineResource(t, "test", 1, time.Minute)

	crashed := subject.newAgent(t, "crashed")
	waiting := subject.newAgent(t, "waiting")
	subject.acquire(t, crashed, "test")

	granted := make(chan core.Lease, 1)
	go func() {
		lease, err := subject.service.Acquire(t.Context(), leasing.AcquireRequest{
			AgentID: waiting.ID, ResourceName: "test",
		})
		if err != nil {
			t.Errorf("Acquire() = %v, want nil", err)
			return
		}
		granted <- lease
	}()

	waitUntilQueued(t, subject, "test", 1)

	// The holder never comes back; its lease runs out.
	subject.clock.advance(2 * time.Minute)

	reclaimed, err := subject.service.ReapExpired(t.Context())
	if err != nil {
		t.Fatalf("ReapExpired() = %v, want nil", err)
	}
	if reclaimed != 1 {
		t.Fatalf("ReapExpired() reclaimed %d leases, want 1", reclaimed)
	}

	select {
	case lease := <-granted:
		if lease.AgentID != waiting.ID {
			t.Errorf("the slot went to %q, want the agent that was waiting", lease.AgentID)
		}
	case <-time.After(5 * time.Second):
		t.Error("the waiting agent never got the reclaimed slot")
	}
}

// A holder that keeps working must be able to keep its slot.
func TestRenewExtendsALease(t *testing.T) {
	subject := newHarness(t)
	subject.defineResource(t, "test", 1, time.Minute)
	agent := subject.newAgent(t, "claude-a")

	lease := subject.acquire(t, agent, "test")
	subject.clock.advance(30 * time.Second)

	renewed, err := subject.service.Renew(t.Context(), agent.ID, lease.ID)
	if err != nil {
		t.Fatalf("Renew() = %v, want nil", err)
	}
	if !renewed.ExpiresAt.After(lease.ExpiresAt) {
		t.Errorf("ExpiresAt = %s, want later than %s", renewed.ExpiresAt, lease.ExpiresAt)
	}

	// Past the original expiry, the renewed lease still holds its slot.
	subject.clock.advance(45 * time.Second)
	reclaimed, err := subject.service.ReapExpired(t.Context())
	if err != nil {
		t.Fatalf("ReapExpired() = %v, want nil", err)
	}
	if reclaimed != 0 {
		t.Errorf("ReapExpired() reclaimed %d renewed leases, want 0", reclaimed)
	}
}

// Renewing a slot you have already lost must fail loudly: somebody else may be using it.
func TestRenewFailsAfterExpiry(t *testing.T) {
	subject := newHarness(t)
	subject.defineResource(t, "test", 1, time.Minute)
	agent := subject.newAgent(t, "claude-a")

	lease := subject.acquire(t, agent, "test")
	subject.clock.advance(2 * time.Minute)

	if _, err := subject.service.Renew(t.Context(), agent.ID, lease.ID); !errors.Is(err, core.ErrConflict) {
		t.Errorf("Renew() = %v, want ErrConflict", err)
	}
}

// One agent must not be able to release or renew another's slot.
func TestLeasesBelongToTheirHolder(t *testing.T) {
	subject := newHarness(t)
	subject.defineResource(t, "test", 1, 15*time.Minute)

	holder := subject.newAgent(t, "holder")
	stranger := subject.newAgent(t, "stranger")
	lease := subject.acquire(t, holder, "test")

	if err := subject.service.Release(t.Context(), stranger.ID, lease.ID); !errors.Is(err, core.ErrUnauthorised) {
		t.Errorf("Release() by a stranger = %v, want ErrUnauthorised", err)
	}
	if _, err := subject.service.Renew(t.Context(), stranger.ID, lease.ID); !errors.Is(err, core.ErrUnauthorised) {
		t.Errorf("Renew() by a stranger = %v, want ErrUnauthorised", err)
	}

	status, err := subject.service.Status(t.Context(), "test")
	if err != nil {
		t.Fatalf("Status() = %v, want nil", err)
	}
	if status.ActiveLeases != 1 {
		t.Errorf("the lease was disturbed: %d active, want 1", status.ActiveLeases)
	}
}

// When an agent goes stale, everything it was holding must come back, or the machine slowly
// strangles itself.
func TestReleaseEverythingHeldBy(t *testing.T) {
	subject := newHarness(t)
	subject.defineResource(t, "test", 2, 15*time.Minute)
	subject.defineResource(t, "reviewer", 1, 15*time.Minute)

	crashed := subject.newAgent(t, "crashed")
	subject.acquire(t, crashed, "test")
	subject.acquire(t, crashed, "reviewer")

	released, err := subject.service.ReleaseEverythingHeldBy(t.Context(), crashed.ID, "went stale")
	if err != nil {
		t.Fatalf("ReleaseEverythingHeldBy() = %v, want nil", err)
	}
	if released != 2 {
		t.Errorf("released %d leases, want 2", released)
	}

	for _, name := range []string{"test", "reviewer"} {
		status, err := subject.service.Status(t.Context(), name)
		if err != nil {
			t.Fatalf("Status(%q) = %v, want nil", name, err)
		}
		if status.ActiveLeases != 0 {
			t.Errorf("%q still has %d active leases", name, status.ActiveLeases)
		}
	}
}

// Releasing twice happens whenever a client retries after a dropped connection.
func TestReleaseIsIdempotent(t *testing.T) {
	subject := newHarness(t)
	subject.defineResource(t, "test", 1, 15*time.Minute)
	agent := subject.newAgent(t, "claude-a")

	lease := subject.acquire(t, agent, "test")

	for attempt := range 2 {
		if err := subject.service.Release(t.Context(), agent.ID, lease.ID); err != nil {
			t.Fatalf("Release() attempt %d = %v, want nil", attempt+1, err)
		}
	}
}

func TestAcquireOnAnUnknownResource(t *testing.T) {
	subject := newHarness(t)
	agent := subject.newAgent(t, "claude-a")

	_, err := subject.service.Acquire(t.Context(), leasing.AcquireRequest{
		AgentID: agent.ID, ResourceName: "nothing-like-this",
	})
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("Acquire() = %v, want ErrNotFound", err)
	}
}

// The status is what the operator reads to understand the machine, so it must count both halves.
func TestStatusReportsHeldAndWaiting(t *testing.T) {
	subject := newHarness(t)
	subject.defineResource(t, "test", 1, 15*time.Minute)

	holder := subject.newAgent(t, "holder")
	waiter := subject.newAgent(t, "waiter")
	subject.acquire(t, holder, "test")

	go func() {
		_, _ = subject.service.Acquire(t.Context(), leasing.AcquireRequest{
			AgentID: waiter.ID, ResourceName: "test",
		})
	}()
	waitUntilQueued(t, subject, "test", 1)

	status, err := subject.service.Status(t.Context(), "test")
	if err != nil {
		t.Fatalf("Status() = %v, want nil", err)
	}
	if status.ActiveLeases != 1 || status.Waiting != 1 || status.Free() != 0 {
		t.Errorf("Status() = %+v, want one held, one waiting and no free slot", status)
	}
}

// waitUntilQueued blocks until the resource's queue has the expected length, so tests synchronise
// on the state they care about rather than on a sleep.
func waitUntilQueued(t *testing.T, subject *harness, resource string, want int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		queue, err := subject.service.Queue(t.Context(), resource)
		if err != nil {
			t.Fatalf("Queue() = %v, want nil", err)
		}
		if len(queue) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}

	t.Fatalf("the queue for %q never reached %d entries", resource, want)
}

func receive(t *testing.T, channel chan string) string {
	t.Helper()

	select {
	case value := <-channel:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was granted in time")
		return ""
	}
}
