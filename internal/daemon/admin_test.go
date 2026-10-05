package daemon_test

import (
	"testing"
	"time"

	"sadeq.uk/lac/internal/config"
	"sadeq.uk/lac/pkg/lacclient"
)

// acquireResult is what a waiting Acquire eventually returned.
type acquireResult struct {
	lease lacclient.Lease
	err   error
}

// queueInBackground starts an Acquire that will have to wait, and returns where its result lands.
func queueInBackground(t *testing.T, client *lacclient.Client, resource string) <-chan acquireResult {
	t.Helper()

	result := make(chan acquireResult, 1)
	go func() {
		lease, err := client.Acquire(t.Context(), lacclient.AcquireRequest{Resource: resource, Reason: "waiting"})
		result <- acquireResult{lease: lease, err: err}
	}()

	return result
}

// settledQueue waits for the queue to reach this many waiting and returns what it then looks like,
// so a test acts on a known queue.
func settledQueue(t *testing.T, client *lacclient.Client, resource string, waiting int) lacclient.QueueStatus {
	t.Helper()

	waitForQueue(t, client, resource, waiting)
	status, err := client.Queue(t.Context(), resource)
	if err != nil {
		t.Fatalf("Queue() = %v, want nil", err)
	}

	return status
}

func awaitAcquire(t *testing.T, result <-chan acquireResult) acquireResult {
	t.Helper()

	select {
	case got := <-result:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting Acquire never returned")
		return acquireResult{}
	}
}

// Taking slots and queue places from other agents is the operator's remedy, not something one
// agent may do to another: otherwise any agent could jump the queue by evicting whoever is ahead.
func TestOnlyOperatorsMayManageOtherAgents(t *testing.T) {
	subject := startDaemon(t, []config.ResourceConfig{{Name: "test", Capacity: 1}})

	holder := subject.connect(t, "claude-a")
	lease, err := holder.Acquire(t.Context(), lacclient.AcquireRequest{Resource: "test"})
	if err != nil {
		t.Fatalf("Acquire() = %v, want nil", err)
	}
	waiter := subject.connect(t, "claude-c")
	waiting := queueInBackground(t, waiter, "test")
	entry := settledQueue(t, holder, "test", 1).Waiting[0]

	intruder := subject.connect(t, "claude-b")
	attempts := map[string]error{
		"ForceRelease":          intruder.ForceRelease(t.Context(), lease.ID),
		"ForceCancelQueueEntry": intruder.ForceCancelQueueEntry(t.Context(), entry.ID),
	}
	_, attempts["ReleaseAgent"] = intruder.ReleaseAgent(t.Context(), "claude-a")
	_, attempts["FreeResource"] = intruder.FreeResource(t.Context(), "test")
	_, attempts["ClearQueue"] = intruder.ClearQueue(t.Context(), "test")
	_, attempts["Evict"] = intruder.Evict(t.Context(), "claude-a")

	for method, err := range attempts {
		if lacclient.ErrorCode(err) != lacclient.CodeUnauthorised {
			t.Errorf("%s by an ordinary agent = %v, want CodeUnauthorised", method, err)
		}
	}

	status := settledQueue(t, holder, "test", 1)
	if len(status.Holders) != 1 || status.Holders[0].ID != lease.ID {
		t.Errorf("the holder lost its slot to an ordinary agent: holders = %+v", status.Holders)
	}
	if _, err := holder.Held(t.Context()); err != nil {
		t.Errorf("the holder was evicted by an ordinary agent: %v", err)
	}

	if err := holder.Release(t.Context(), lease.ID); err != nil {
		t.Fatalf("Release() = %v, want nil", err)
	}
	awaitAcquire(t, waiting)
}

// Unblocking a resource: freeing its holders hands the slot to whoever is next, and clearing its
// queue makes every waiter's Acquire fail rather than hang.
func TestOperatorUnblocksAResource(t *testing.T) {
	subject := startDaemon(t, []config.ResourceConfig{{Name: "test", Capacity: 1}})
	operator := subject.connect(t, "operator")

	holder := subject.connect(t, "claude-a")
	if _, err := holder.Acquire(t.Context(), lacclient.AcquireRequest{Resource: "test"}); err != nil {
		t.Fatalf("Acquire() = %v, want nil", err)
	}
	next := queueInBackground(t, subject.connect(t, "claude-b"), "test")
	waitForQueue(t, operator, "test", 1)

	released, err := operator.FreeResource(t.Context(), "test")
	if err != nil || released != 1 {
		t.Fatalf("FreeResource() = %d, %v, want 1, nil", released, err)
	}
	if got := awaitAcquire(t, next); got.err != nil {
		t.Fatalf("the next in line did not get the freed slot: %v", got.err)
	}

	stuckFirst := queueInBackground(t, subject.connect(t, "claude-c"), "test")
	stuckSecond := queueInBackground(t, subject.connect(t, "claude-d"), "test")
	waitForQueue(t, operator, "test", 2)

	cancelled, err := operator.ClearQueue(t.Context(), "test")
	if err != nil || cancelled != 2 {
		t.Fatalf("ClearQueue() = %d, %v, want 2, nil", cancelled, err)
	}
	for _, waiting := range []<-chan acquireResult{stuckFirst, stuckSecond} {
		if got := awaitAcquire(t, waiting); got.err == nil {
			t.Errorf("a cleared waiter was granted %s, want its Acquire to fail", got.lease.ID)
		}
	}
}

// One waiter can be withdrawn by entry id without disturbing the rest of the line.
func TestOperatorCancelsOneWaiter(t *testing.T) {
	subject := startDaemon(t, []config.ResourceConfig{{Name: "test", Capacity: 1}})
	operator := subject.connect(t, "operator")

	holder := subject.connect(t, "claude-a")
	if _, err := holder.Acquire(t.Context(), lacclient.AcquireRequest{Resource: "test"}); err != nil {
		t.Fatalf("Acquire() = %v, want nil", err)
	}
	cancelledWaiter := queueInBackground(t, subject.connect(t, "claude-b"), "test")
	waitForQueue(t, operator, "test", 1)
	queueInBackground(t, subject.connect(t, "claude-c"), "test")
	status := settledQueue(t, operator, "test", 2)

	if err := operator.ForceCancelQueueEntry(t.Context(), status.Waiting[0].ID); err != nil {
		t.Fatalf("ForceCancelQueueEntry() = %v, want nil", err)
	}
	if got := awaitAcquire(t, cancelledWaiter); got.err == nil {
		t.Error("the cancelled waiter was granted a slot, want its Acquire to fail")
	}

	remaining := settledQueue(t, operator, "test", 1)
	if remaining.Waiting[0].AgentName != "claude-c" {
		t.Errorf("the wrong waiter was withdrawn: %s is still waiting", remaining.Waiting[0].AgentName)
	}
}

// Releasing an agent gives back its slots and queue places but leaves it connected; evicting it
// also revokes its token, so a stuck session cannot simply take the slot again.
func TestOperatorReleasesAndEvictsAnAgent(t *testing.T) {
	subject := startDaemon(t, []config.ResourceConfig{
		{Name: "test", Capacity: 1}, {Name: "reviewer", Capacity: 1},
	})
	operator := subject.connect(t, "operator")

	stuck := subject.connect(t, "claude-a")
	for _, resource := range []string{"test", "reviewer"} {
		if _, err := stuck.Acquire(t.Context(), lacclient.AcquireRequest{Resource: resource}); err != nil {
			t.Fatalf("Acquire(%s) = %v, want nil", resource, err)
		}
	}

	released, err := operator.ReleaseAgent(t.Context(), "claude-a")
	if err != nil || released != 2 {
		t.Fatalf("ReleaseAgent() = %d, %v, want 2, nil", released, err)
	}
	held, err := stuck.Held(t.Context())
	if err != nil || len(held) != 0 {
		t.Fatalf("after ReleaseAgent the agent holds %d slots (%v), want 0 and still connected", len(held), err)
	}

	if _, err := stuck.Acquire(t.Context(), lacclient.AcquireRequest{Resource: "test"}); err != nil {
		t.Fatalf("Acquire() = %v, want nil", err)
	}

	released, err = operator.Evict(t.Context(), "claude-a")
	if err != nil || released != 1 {
		t.Fatalf("Evict() = %d, %v, want 1, nil", released, err)
	}
	if _, err := stuck.Held(t.Context()); lacclient.ErrorCode(err) != lacclient.CodeUnauthorised {
		t.Errorf("the evicted agent's next call = %v, want CodeUnauthorised", err)
	}

	agents, err := operator.Agents(t.Context())
	if err != nil {
		t.Fatalf("Agents() = %v, want nil", err)
	}
	for _, agent := range agents {
		if agent.Name == "claude-a" {
			t.Error("the evicted agent is still on the roster")
		}
	}

	status, err := operator.Queue(t.Context(), "test")
	if err != nil || len(status.Holders) != 0 {
		t.Errorf("after eviction test has holders %+v (%v), want none", status.Holders, err)
	}
}
