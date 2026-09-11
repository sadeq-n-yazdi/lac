package sqlite_test

import (
	"errors"
	"testing"
	"time"

	"sadeq.uk/lac/internal/core"
	"sadeq.uk/lac/internal/id"
	"sadeq.uk/lac/internal/store/sqlite"
)

// Two live agents sharing a name would make every "tell claude-a to stop" ambiguous.
func TestAgentNamesAreUniqueWhileActive(t *testing.T) {
	store := openStore(t)
	first := newAgent(t, store, "claude-a")

	duplicate := first
	duplicate.ID = id.New("agent")

	err := store.Agents().Create(t.Context(), duplicate)
	if !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("creating a second active agent named %q = %v, want ErrAlreadyExists", first.Name, err)
	}

	// Once the first agent is gone the name is free again, so a restarted session can take it back.
	if err := store.Agents().SetState(t.Context(), first.ID, core.AgentDeregistered, baseTime); err != nil {
		t.Fatalf("SetState() = %v, want nil", err)
	}
	if err := store.Agents().Create(t.Context(), duplicate); err != nil {
		t.Errorf("reusing the name of a deregistered agent = %v, want nil", err)
	}
}

// Whether an agent is one of the daemon's own components decides whether it is asked for reports,
// so it must survive being written and read back.
func TestWhetherAnAgentIsInternalRoundTrips(t *testing.T) {
	store := openStore(t)
	agent := newAgent(t, store, "pr-watcher")

	if stored, err := store.Agents().ByID(t.Context(), agent.ID); err != nil {
		t.Fatalf("ByID() = %v, want nil", err)
	} else if stored.Internal {
		t.Error("an ordinary agent came back marked as internal")
	}

	agent.Internal = true
	if err := store.Agents().Update(t.Context(), agent); err != nil {
		t.Fatalf("Update() = %v, want nil", err)
	}

	stored, err := store.Agents().ByID(t.Context(), agent.ID)
	if err != nil {
		t.Fatalf("ByID() = %v, want nil", err)
	}
	if !stored.Internal {
		t.Error("the internal mark did not round trip")
	}
}

// Capabilities decide what an agent may do, so they must survive a round trip exactly.
func TestAgentCapabilitiesRoundTrip(t *testing.T) {
	store := openStore(t)
	agent := newAgent(t, store, "claude-a")
	agent.Capabilities = core.Capabilities{
		Resources:          []string{"test", "reviewer"},
		CanBroadcast:       true,
		CanDefineResources: false,
		CanRequestReports:  true,
	}

	if err := store.Agents().Update(t.Context(), agent); err != nil {
		t.Fatalf("Update() = %v, want nil", err)
	}

	stored, err := store.Agents().ByID(t.Context(), agent.ID)
	if err != nil {
		t.Fatalf("ByID() = %v, want nil", err)
	}

	if !stored.Capabilities.MayLease("reviewer") || stored.Capabilities.MayLease("database") {
		t.Errorf("capabilities did not round trip: %+v", stored.Capabilities)
	}
	if !stored.Capabilities.CanRequestReports || stored.Capabilities.CanDefineResources {
		t.Errorf("capability flags did not round trip: %+v", stored.Capabilities)
	}
}

// A heartbeat is proof of life, so it must bring a stale agent back rather than leave it excluded.
func TestHeartbeatRevivesAStaleAgent(t *testing.T) {
	store := openStore(t)
	agent := newAgent(t, store, "claude-a")

	if err := store.Agents().SetState(t.Context(), agent.ID, core.AgentStale, baseTime); err != nil {
		t.Fatalf("SetState() = %v, want nil", err)
	}
	if err := store.Agents().Heartbeat(t.Context(), agent.ID, baseTime.Add(time.Minute)); err != nil {
		t.Fatalf("Heartbeat() = %v, want nil", err)
	}

	stored, err := store.Agents().ByID(t.Context(), agent.ID)
	if err != nil {
		t.Fatalf("ByID() = %v, want nil", err)
	}
	if stored.State != core.AgentActive {
		t.Errorf("state after a heartbeat = %q, want %q", stored.State, core.AgentActive)
	}
}

// A deregistered agent said goodbye. A late heartbeat must not resurrect it, because its token has
// already been revoked.
func TestHeartbeatDoesNotReviveADeregisteredAgent(t *testing.T) {
	store := openStore(t)
	agent := newAgent(t, store, "claude-a")

	if err := store.Agents().SetState(t.Context(), agent.ID, core.AgentDeregistered, baseTime); err != nil {
		t.Fatalf("SetState() = %v, want nil", err)
	}

	err := store.Agents().Heartbeat(t.Context(), agent.ID, baseTime.Add(time.Minute))
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("Heartbeat() on a deregistered agent = %v, want ErrNotFound", err)
	}
}

func TestStaleBefore(t *testing.T) {
	store := openStore(t)
	quiet := newAgent(t, store, "claude-quiet")
	chatty := newAgent(t, store, "claude-chatty")

	if err := store.Agents().Heartbeat(t.Context(), chatty.ID, baseTime.Add(time.Hour)); err != nil {
		t.Fatalf("Heartbeat() = %v, want nil", err)
	}

	stale, err := store.Agents().StaleBefore(t.Context(), baseTime.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("StaleBefore() = %v, want nil", err)
	}
	if len(stale) != 1 || stale[0].ID != quiet.ID {
		t.Errorf("StaleBefore() returned %d agents, want only %q", len(stale), quiet.Name)
	}
}

// Tokens are looked up by hash through a unique index. A revoked credential must still be found,
// so the auth layer can say "revoked" rather than "unknown".
func TestCredentialLookupAndRevocation(t *testing.T) {
	store := openStore(t)
	agent := newAgent(t, store, "claude-a")
	hash := []byte("a-keyed-hash-of-a-token")

	credential := core.Credential{AgentID: agent.ID, TokenHash: hash, IssuedAt: baseTime}
	if err := store.Credentials().Store(t.Context(), credential); err != nil {
		t.Fatalf("Store() = %v, want nil", err)
	}

	found, err := store.Credentials().ByTokenHash(t.Context(), hash)
	if err != nil {
		t.Fatalf("ByTokenHash() = %v, want nil", err)
	}
	if found.AgentID != agent.ID || found.Revoked() {
		t.Errorf("ByTokenHash() = %+v, want an active credential for %q", found, agent.ID)
	}

	if err := store.Credentials().Revoke(t.Context(), agent.ID, baseTime.Add(time.Minute)); err != nil {
		t.Fatalf("Revoke() = %v, want nil", err)
	}

	found, err = store.Credentials().ByTokenHash(t.Context(), hash)
	if err != nil {
		t.Fatalf("ByTokenHash() after revocation = %v, want nil", err)
	}
	if !found.Revoked() {
		t.Error("the credential is not marked revoked")
	}

	// Revoking twice must stay quiet, so deregistration can be retried.
	if err := store.Credentials().Revoke(t.Context(), agent.ID, baseTime.Add(time.Hour)); err != nil {
		t.Errorf("a second Revoke() = %v, want nil", err)
	}
}

func TestUnknownTokenHashIsNotFound(t *testing.T) {
	store := openStore(t)

	_, err := store.Credentials().ByTokenHash(t.Context(), []byte("nobody-has-this"))
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("ByTokenHash() = %v, want ErrNotFound", err)
	}
}

// The point of the outbox is that a message survives the recipient being away: it stays pending
// until that agent acknowledges it, however long it takes.
func TestMessagesStayPendingUntilAcknowledged(t *testing.T) {
	store := openStore(t)
	sender := newAgent(t, store, "claude-a")
	recipient := newAgent(t, store, "claude-b")

	message := core.Message{
		ID: id.New("msg"), FromAgentID: sender.ID, ToAgentID: recipient.ID,
		Kind: "status", Body: []byte(`{"working":"on the parser"}`), CreatedAt: baseTime,
	}
	if err := store.Messages().Append(t.Context(), message, []string{recipient.ID}); err != nil {
		t.Fatalf("Append() = %v, want nil", err)
	}

	pending, err := store.Messages().Pending(t.Context(), recipient.ID, 0)
	if err != nil {
		t.Fatalf("Pending() = %v, want nil", err)
	}
	if len(pending) != 1 || pending[0].ID != message.ID {
		t.Fatalf("Pending() returned %d messages, want the one that was sent", len(pending))
	}

	// Reading is not acknowledging: an agent that crashes after reading must see it again.
	if err := store.Messages().MarkDelivered(
		t.Context(), recipient.ID, []string{message.ID}, baseTime,
	); err != nil {
		t.Fatalf("MarkDelivered() = %v, want nil", err)
	}
	if pending, err = store.Messages().Pending(t.Context(), recipient.ID, 0); err != nil || len(pending) != 1 {
		t.Fatalf("Pending() after delivery returned %d messages (%v), want 1", len(pending), err)
	}

	acknowledged, err := store.Messages().Acknowledge(t.Context(), recipient.ID, []string{message.ID}, baseTime)
	if err != nil || acknowledged != 1 {
		t.Fatalf("Acknowledge() = %d, %v, want 1, nil", acknowledged, err)
	}

	if pending, err = store.Messages().Pending(t.Context(), recipient.ID, 0); err != nil || len(pending) != 0 {
		t.Fatalf("Pending() after acknowledgement returned %d messages (%v), want 0", len(pending), err)
	}
}

// One agent must not be able to clear another agent's inbox.
func TestAcknowledgeIgnoresSomebodyElsesMessages(t *testing.T) {
	store := openStore(t)
	sender := newAgent(t, store, "claude-a")
	recipient := newAgent(t, store, "claude-b")
	stranger := newAgent(t, store, "claude-c")

	message := core.Message{
		ID: id.New("msg"), FromAgentID: sender.ID, ToAgentID: recipient.ID,
		Kind: "status", Body: []byte(`{"a":1}`), CreatedAt: baseTime,
	}
	if err := store.Messages().Append(t.Context(), message, []string{recipient.ID}); err != nil {
		t.Fatalf("Append() = %v, want nil", err)
	}

	acknowledged, err := store.Messages().Acknowledge(t.Context(), stranger.ID, []string{message.ID}, baseTime)
	if err != nil {
		t.Fatalf("Acknowledge() = %v, want nil", err)
	}
	if acknowledged != 0 {
		t.Errorf("a stranger acknowledged %d messages, want 0", acknowledged)
	}

	pending, err := store.Messages().Pending(t.Context(), recipient.ID, 0)
	if err != nil || len(pending) != 1 {
		t.Errorf("the real recipient now has %d pending messages (%v), want 1", len(pending), err)
	}
}

func TestTopicSubscriptions(t *testing.T) {
	store := openStore(t)
	first := newAgent(t, store, "claude-a")
	second := newAgent(t, store, "claude-b")

	for _, agent := range []core.Agent{first, second} {
		if err := store.Messages().Subscribe(t.Context(), agent.ID, "backend"); err != nil {
			t.Fatalf("Subscribe() = %v, want nil", err)
		}
	}
	// Subscribing twice must be harmless: an agent that reconnects re-subscribes.
	if err := store.Messages().Subscribe(t.Context(), first.ID, "backend"); err != nil {
		t.Fatalf("a repeated Subscribe() = %v, want nil", err)
	}

	subscribers, err := store.Messages().Subscribers(t.Context(), "backend")
	if err != nil {
		t.Fatalf("Subscribers() = %v, want nil", err)
	}
	if len(subscribers) != 2 {
		t.Errorf("Subscribers() returned %d agents, want 2", len(subscribers))
	}

	if err := store.Messages().Unsubscribe(t.Context(), first.ID, "backend"); err != nil {
		t.Fatalf("Unsubscribe() = %v, want nil", err)
	}
	if subscribers, err = store.Messages().Subscribers(t.Context(), "backend"); err != nil || len(subscribers) != 1 {
		t.Errorf("Subscribers() after unsubscribing returned %d agents (%v), want 1", len(subscribers), err)
	}
}

// Housekeeping must remove what nobody is waiting for, and keep what somebody still is.
func TestPruneExpired(t *testing.T) {
	store := openStore(t)
	sender := newAgent(t, store, "claude-a")
	recipient := newAgent(t, store, "claude-b")

	acknowledged := core.Message{
		ID: id.New("msg"), FromAgentID: sender.ID, ToAgentID: recipient.ID,
		Kind: "status", Body: []byte(`{"a":1}`), CreatedAt: baseTime,
	}
	outstanding := core.Message{
		ID: id.New("msg"), FromAgentID: sender.ID, ToAgentID: recipient.ID,
		Kind: "status", Body: []byte(`{"b":2}`), CreatedAt: baseTime,
	}
	for _, message := range []core.Message{acknowledged, outstanding} {
		if err := store.Messages().Append(t.Context(), message, []string{recipient.ID}); err != nil {
			t.Fatalf("Append() = %v, want nil", err)
		}
	}
	if _, err := store.Messages().Acknowledge(
		t.Context(), recipient.ID, []string{acknowledged.ID}, baseTime,
	); err != nil {
		t.Fatalf("Acknowledge() = %v, want nil", err)
	}

	pruned, err := store.Messages().PruneExpired(t.Context(), baseTime.Add(time.Hour))
	if err != nil {
		t.Fatalf("PruneExpired() = %v, want nil", err)
	}
	if pruned != 1 {
		t.Errorf("PruneExpired() removed %d messages, want 1", pruned)
	}

	pending, err := store.Messages().Pending(t.Context(), recipient.ID, 0)
	if err != nil || len(pending) != 1 || pending[0].ID != outstanding.ID {
		t.Errorf("the unacknowledged message did not survive pruning: %d pending (%v)", len(pending), err)
	}
}

// The queue is the fairness guarantee: higher priority first, and equal priorities in the order
// they asked. Getting this wrong is the difference between a queue and a scramble.
func TestQueueServiceOrder(t *testing.T) {
	store := openStore(t)
	defineResource(t, store, "test", 4)

	type request struct {
		name     string
		priority core.Priority
		offset   time.Duration
	}
	requests := []request{
		{name: "second-normal", priority: core.PriorityNormal, offset: 2 * time.Second},
		{name: "interactive", priority: core.PriorityInteractive, offset: 3 * time.Second},
		{name: "first-normal", priority: core.PriorityNormal, offset: time.Second},
		{name: "background", priority: core.PriorityBackground, offset: 0},
	}

	byName := make(map[string]string, len(requests))
	for _, entry := range requests {
		agent := newAgent(t, store, entry.name)
		queueEntry := core.QueueEntry{
			ID: id.New("queue"), ResourceName: "test", AgentID: agent.ID,
			Priority: entry.priority, State: core.QueueWaiting, RequestedAt: baseTime.Add(entry.offset),
		}
		if err := store.Leases().Enqueue(t.Context(), queueEntry); err != nil {
			t.Fatalf("Enqueue(%q) = %v, want nil", entry.name, err)
		}
		byName[agent.ID] = entry.name
	}

	waiting, err := store.Leases().Waiting(t.Context(), "test")
	if err != nil {
		t.Fatalf("Waiting() = %v, want nil", err)
	}

	got := make([]string, 0, len(waiting))
	for _, entry := range waiting {
		got = append(got, byName[entry.AgentID])
	}

	want := []string{"interactive", "first-normal", "second-normal", "background"}
	if len(got) != len(want) {
		t.Fatalf("Waiting() returned %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("queue order = %v, want %v", got, want)
		}
	}
}

// Counting held slots is what the grant decision reads. A released or expired lease is not held.
func TestCountActiveLeases(t *testing.T) {
	store := openStore(t)
	defineResource(t, store, "test", 4)
	holder := newAgent(t, store, "claude-a")

	held := core.Lease{
		ID: id.New("lease"), ResourceName: "test", AgentID: holder.ID,
		AcquiredAt: baseTime, ExpiresAt: baseTime.Add(15 * time.Minute),
	}
	released := core.Lease{
		ID: id.New("lease"), ResourceName: "test", AgentID: holder.ID,
		AcquiredAt: baseTime, ExpiresAt: baseTime.Add(15 * time.Minute), ReleasedAt: baseTime.Add(time.Minute),
	}
	abandoned := core.Lease{
		ID: id.New("lease"), ResourceName: "test", AgentID: holder.ID,
		AcquiredAt: baseTime, ExpiresAt: baseTime.Add(time.Minute),
	}
	for _, lease := range []core.Lease{held, released, abandoned} {
		if err := store.Leases().CreateLease(t.Context(), lease); err != nil {
			t.Fatalf("CreateLease() = %v, want nil", err)
		}
	}

	count, err := store.Leases().CountActiveLeases(t.Context(), "test", baseTime.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("CountActiveLeases() = %v, want nil", err)
	}
	if count != 1 {
		t.Errorf("CountActiveLeases() = %d, want 1: only the unreleased, unexpired lease holds a slot", count)
	}
}

// An agent that dies mid-run must lose its slot, so the reaper has to be able to find it.
func TestExpiredLeases(t *testing.T) {
	store := openStore(t)
	defineResource(t, store, "test", 4)
	holder := newAgent(t, store, "claude-a")

	abandoned := core.Lease{
		ID: id.New("lease"), ResourceName: "test", AgentID: holder.ID,
		AcquiredAt: baseTime, ExpiresAt: baseTime.Add(time.Minute),
	}
	if err := store.Leases().CreateLease(t.Context(), abandoned); err != nil {
		t.Fatalf("CreateLease() = %v, want nil", err)
	}

	expired, err := store.Leases().ExpiredLeases(t.Context(), baseTime.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("ExpiredLeases() = %v, want nil", err)
	}
	if len(expired) != 1 || expired[0].ID != abandoned.ID {
		t.Fatalf("ExpiredLeases() returned %d leases, want the abandoned one", len(expired))
	}

	// Once released, it is nobody's business any more.
	if err := store.Leases().Release(t.Context(), abandoned.ID, baseTime.Add(3*time.Minute)); err != nil {
		t.Fatalf("Release() = %v, want nil", err)
	}
	if expired, err = store.Leases().ExpiredLeases(t.Context(), baseTime.Add(4*time.Minute)); err != nil {
		t.Fatalf("ExpiredLeases() = %v, want nil", err)
	}
	if len(expired) != 0 {
		t.Errorf("ExpiredLeases() returned %d leases after release, want 0", len(expired))
	}
}

// Renewing a lease you no longer hold must fail loudly: the slot may already be somebody else's,
// and carrying on working would break the capacity guarantee.
func TestRenewRefusesALostLease(t *testing.T) {
	store := openStore(t)
	defineResource(t, store, "test", 1)
	holder := newAgent(t, store, "claude-a")

	lease := core.Lease{
		ID: id.New("lease"), ResourceName: "test", AgentID: holder.ID,
		AcquiredAt: baseTime, ExpiresAt: baseTime.Add(time.Minute),
	}
	if err := store.Leases().CreateLease(t.Context(), lease); err != nil {
		t.Fatalf("CreateLease() = %v, want nil", err)
	}

	err := store.Leases().Renew(t.Context(), lease.ID, baseTime.Add(time.Hour), baseTime.Add(10*time.Minute))
	if !errors.Is(err, core.ErrConflict) {
		t.Errorf("Renew() on an expired lease = %v, want ErrConflict", err)
	}

	err = store.Leases().Renew(t.Context(), "lease_nobody_has", baseTime.Add(time.Hour), baseTime)
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("Renew() on an unknown lease = %v, want ErrNotFound", err)
	}
}

// Releasing twice happens whenever a client retries after a dropped connection.
func TestReleaseIsIdempotent(t *testing.T) {
	store := openStore(t)
	defineResource(t, store, "test", 1)
	holder := newAgent(t, store, "claude-a")

	lease := core.Lease{
		ID: id.New("lease"), ResourceName: "test", AgentID: holder.ID,
		AcquiredAt: baseTime, ExpiresAt: baseTime.Add(time.Hour),
	}
	if err := store.Leases().CreateLease(t.Context(), lease); err != nil {
		t.Fatalf("CreateLease() = %v, want nil", err)
	}

	for attempt := range 2 {
		if err := store.Leases().Release(t.Context(), lease.ID, baseTime.Add(time.Minute)); err != nil {
			t.Fatalf("Release() attempt %d = %v, want nil", attempt+1, err)
		}
	}

	stored, err := store.Leases().LeaseByID(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("LeaseByID() = %v, want nil", err)
	}
	if want := baseTime.Add(time.Minute); !stored.ReleasedAt.Equal(want) {
		t.Errorf("ReleasedAt = %s, want the first release at %s", stored.ReleasedAt, want)
	}
}

// A cancellation arriving after the grant must not undo the grant.
func TestResolveQueueEntryOnlyActsOnWaitingEntries(t *testing.T) {
	store := openStore(t)
	defineResource(t, store, "test", 1)
	agent := newAgent(t, store, "claude-a")

	entry := core.QueueEntry{
		ID: id.New("queue"), ResourceName: "test", AgentID: agent.ID,
		State: core.QueueWaiting, RequestedAt: baseTime,
	}
	if err := store.Leases().Enqueue(t.Context(), entry); err != nil {
		t.Fatalf("Enqueue() = %v, want nil", err)
	}

	if err := store.Leases().ResolveQueueEntry(t.Context(), entry.ID, core.QueueGranted, baseTime); err != nil {
		t.Fatalf("ResolveQueueEntry() = %v, want nil", err)
	}

	err := store.Leases().ResolveQueueEntry(t.Context(), entry.ID, core.QueueCancelled, baseTime.Add(time.Second))
	if !errors.Is(err, core.ErrConflict) {
		t.Fatalf("cancelling an already granted entry = %v, want ErrConflict", err)
	}

	stored, err := store.Leases().QueueEntryByID(t.Context(), entry.ID)
	if err != nil {
		t.Fatalf("QueueEntryByID() = %v, want nil", err)
	}
	if stored.State != core.QueueGranted {
		t.Errorf("state = %q, want it to stay %q", stored.State, core.QueueGranted)
	}
}

// Reconfiguring a resource must not disturb the slots already granted from it.
func TestDefineIsAnUpsertThatKeepsLeases(t *testing.T) {
	store := openStore(t)
	defineResource(t, store, "test", 4)
	holder := newAgent(t, store, "claude-a")

	lease := core.Lease{
		ID: id.New("lease"), ResourceName: "test", AgentID: holder.ID,
		AcquiredAt: baseTime, ExpiresAt: baseTime.Add(time.Hour),
	}
	if err := store.Leases().CreateLease(t.Context(), lease); err != nil {
		t.Fatalf("CreateLease() = %v, want nil", err)
	}

	if err := store.Resources().Define(t.Context(), core.Resource{
		Name: "test", Capacity: 2, LeaseTimeToLive: 30 * time.Minute, Description: "narrowed",
	}); err != nil {
		t.Fatalf("Define() = %v, want nil", err)
	}

	resource, err := store.Resources().ByName(t.Context(), "test")
	if err != nil {
		t.Fatalf("ByName() = %v, want nil", err)
	}
	if resource.Capacity != 2 || resource.Description != "narrowed" {
		t.Errorf("Define() did not reconfigure the resource: %+v", resource)
	}

	count, err := store.Leases().CountActiveLeases(t.Context(), "test", baseTime.Add(time.Minute))
	if err != nil || count != 1 {
		t.Errorf("the existing lease did not survive reconfiguration: %d (%v)", count, err)
	}
}

// Deleting a resource takes its queue with it; nobody can be left waiting for something that no
// longer exists.
func TestDeletingAResourceCascades(t *testing.T) {
	store := openStore(t)
	defineResource(t, store, "test", 1)
	agent := newAgent(t, store, "claude-a")

	entry := core.QueueEntry{
		ID: id.New("queue"), ResourceName: "test", AgentID: agent.ID,
		State: core.QueueWaiting, RequestedAt: baseTime,
	}
	if err := store.Leases().Enqueue(t.Context(), entry); err != nil {
		t.Fatalf("Enqueue() = %v, want nil", err)
	}

	if err := store.Resources().Delete(t.Context(), "test"); err != nil {
		t.Fatalf("Delete() = %v, want nil", err)
	}

	if _, err := store.Leases().QueueEntryByID(t.Context(), entry.ID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("the queue entry outlived its resource: %v", err)
	}
	if err := store.Resources().Delete(t.Context(), "test"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("deleting a missing resource = %v, want ErrNotFound", err)
	}
}

func TestReportCollection(t *testing.T) {
	store := openStore(t)
	requester := newAgent(t, store, "operator")
	answering := newAgent(t, store, "claude-a")
	silent := newAgent(t, store, "claude-b")

	request := core.ReportRequest{
		ID: id.New("report"), RequesterID: requester.ID, Question: "what are you working on?",
		AskedAgentIDs: []string{answering.ID, silent.ID},
		CreatedAt:     baseTime, Deadline: baseTime.Add(30 * time.Second),
	}
	if err := store.Reports().CreateRequest(t.Context(), request); err != nil {
		t.Fatalf("CreateRequest() = %v, want nil", err)
	}

	stored, err := store.Reports().RequestByID(t.Context(), request.ID)
	if err != nil {
		t.Fatalf("RequestByID() = %v, want nil", err)
	}
	if len(stored.AskedAgentIDs) != 2 {
		t.Errorf("the asked agents did not round trip: %v", stored.AskedAgentIDs)
	}

	first := core.Report{
		RequestID: request.ID, AgentID: answering.ID, Body: "the parser", CreatedAt: baseTime.Add(time.Second),
	}
	if err := store.Reports().AddReport(t.Context(), first); err != nil {
		t.Fatalf("AddReport() = %v, want nil", err)
	}

	// An agent that corrects itself must replace its answer rather than appear twice.
	corrected := first
	corrected.Body = "the parser, and then the tests"
	corrected.CreatedAt = baseTime.Add(2 * time.Second)
	if err := store.Reports().AddReport(t.Context(), corrected); err != nil {
		t.Fatalf("a second AddReport() = %v, want nil", err)
	}

	reports, err := store.Reports().ReportsFor(t.Context(), request.ID)
	if err != nil {
		t.Fatalf("ReportsFor() = %v, want nil", err)
	}
	if len(reports) != 1 || reports[0].Body != corrected.Body {
		t.Errorf("ReportsFor() = %+v, want one corrected answer", reports)
	}
}

// The audit log is how an operator reconstructs what happened. It must record, filter and never
// hand back a mutable history.
func TestAuditLog(t *testing.T) {
	store := openStore(t)

	entries := []core.AuditEntry{
		{Actor: "agent_a", Action: core.AuditLeaseGranted, Target: "test", At: baseTime},
		{Actor: "agent_b", Action: core.AuditLeaseGranted, Target: "test", At: baseTime.Add(time.Second)},
		{Actor: "agent_a", Action: core.AuditAuthDenied, Target: "reviewer", At: baseTime.Add(2 * time.Second)},
	}
	for _, entry := range entries {
		if err := store.Audit().Append(t.Context(), entry); err != nil {
			t.Fatalf("Append() = %v, want nil", err)
		}
	}

	all, err := store.Audit().List(t.Context(), core.AuditFilter{})
	if err != nil {
		t.Fatalf("List() = %v, want nil", err)
	}
	if len(all) != 3 {
		t.Fatalf("List() returned %d entries, want 3", len(all))
	}
	if all[0].Action != core.AuditAuthDenied {
		t.Errorf("List() is not newest first: %+v", all[0])
	}

	byActor, err := store.Audit().List(t.Context(), core.AuditFilter{Actor: "agent_a"})
	if err != nil || len(byActor) != 2 {
		t.Errorf("filtering by actor returned %d entries (%v), want 2", len(byActor), err)
	}

	byAction, err := store.Audit().List(t.Context(), core.AuditFilter{Actions: []core.AuditAction{
		core.AuditAuthDenied,
	}})
	if err != nil || len(byAction) != 1 {
		t.Errorf("filtering by action returned %d entries (%v), want 1", len(byAction), err)
	}

	since, err := store.Audit().List(t.Context(), core.AuditFilter{Since: baseTime.Add(time.Second)})
	if err != nil || len(since) != 2 {
		t.Errorf("filtering by time returned %d entries (%v), want 2", len(since), err)
	}
}

func TestAuditEntryNeedsAnActorAndAnAction(t *testing.T) {
	store := openStore(t)

	err := store.Audit().Append(t.Context(), core.AuditEntry{Target: "test", At: baseTime})
	if !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("Append() without an actor = %v, want ErrInvalidArgument", err)
	}
}

// Capacity is the guarantee LAC exists to provide, and it is enforced by reading the slot count
// and inserting the lease inside one transaction. This is that sequence, run concurrently.
func TestCapacityHoldsUnderConcurrentGrants(t *testing.T) {
	const (
		capacity   = 4
		contenders = 12
	)

	store := openStore(t)
	defineResource(t, store, "test", capacity)

	agents := make([]core.Agent, 0, contenders)
	for index := range contenders {
		agents = append(agents, newAgent(t, store, "claude-"+string(rune('a'+index))))
	}

	granted := make(chan bool, contenders)
	start := make(chan struct{})

	for _, agent := range agents {
		go func() {
			<-start
			granted <- grantIfRoom(t, store, agent)
		}()
	}
	close(start)

	awarded := 0
	for range contenders {
		if <-granted {
			awarded++
		}
	}

	if awarded != capacity {
		t.Errorf("%d agents were granted a slot, want exactly %d", awarded, capacity)
	}

	held, err := store.Leases().CountActiveLeases(t.Context(), "test", baseTime.Add(time.Minute))
	if err != nil {
		t.Fatalf("CountActiveLeases() = %v, want nil", err)
	}
	if held != capacity {
		t.Errorf("%d slots are held, want %d", held, capacity)
	}
}

// grantIfRoom is the read-then-write sequence the leasing service will use: it must be atomic, or
// two agents can both see a free slot and both take it.
func grantIfRoom(t *testing.T, store *sqlite.Store, agent core.Agent) bool {
	t.Helper()

	awarded := false
	err := store.InTransaction(t.Context(), func(tx core.Store) error {
		count, err := tx.Leases().CountActiveLeases(t.Context(), "test", baseTime)
		if err != nil {
			return err
		}

		resource, err := tx.Resources().ByName(t.Context(), "test")
		if err != nil {
			return err
		}
		if count >= resource.Capacity {
			return nil
		}

		awarded = true

		return tx.Leases().CreateLease(t.Context(), core.Lease{
			ID: id.New("lease"), ResourceName: "test", AgentID: agent.ID,
			AcquiredAt: baseTime, ExpiresAt: baseTime.Add(resource.LeaseTimeToLive),
		})
	})
	if err != nil {
		t.Errorf("granting a slot to %s: %v", agent.Name, err)
		return false
	}

	return awarded
}
