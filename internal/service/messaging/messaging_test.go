package messaging_test

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"sadeq.uk/lac/internal/core"
	"sadeq.uk/lac/internal/id"
	"sadeq.uk/lac/internal/service/messaging"
	"sadeq.uk/lac/internal/store/sqlite"
)

var baseTime = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// recordingNotifier stands in for the connected sessions, so a test can see who was nudged.
type recordingNotifier struct {
	mutex     sync.Mutex
	delivered map[string]int
	connected map[string]bool
}

func newRecordingNotifier(connected ...string) *recordingNotifier {
	notifier := &recordingNotifier{
		delivered: make(map[string]int),
		connected: make(map[string]bool, len(connected)),
	}
	for _, agentID := range connected {
		notifier.connected[agentID] = true
	}

	return notifier
}

func (n *recordingNotifier) Broadcast(agentID, _ string, _ any) int {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	if !n.connected[agentID] {
		return 0
	}
	n.delivered[agentID]++

	return 1
}

func (n *recordingNotifier) count(agentID string) int {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	return n.delivered[agentID]
}

type harness struct {
	service *messaging.Service
	store   *sqlite.Store
}

func newHarness(t *testing.T, notifier messaging.Notifier) *harness {
	t.Helper()

	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "lac.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	service := messaging.New(store, messaging.Options{
		Notifier: notifier,
		Clock:    func() time.Time { return baseTime },
		Logger:   quietLogger(),
	})

	return &harness{service: service, store: store}
}

func (h *harness) newAgent(t *testing.T, name string) core.Agent {
	t.Helper()

	agent := core.Agent{
		ID: id.New("agent"), Name: name, Kind: "claude", Workdir: "/tmp/" + name,
		State: core.AgentActive, RegisteredAt: baseTime, LastHeartbeatAt: baseTime,
	}
	if err := h.store.Agents().Create(t.Context(), agent); err != nil {
		t.Fatalf("creating the agent: %v", err)
	}

	return agent
}

func TestDirectMessage(t *testing.T) {
	notifier := newRecordingNotifier()
	subject := newHarness(t, notifier)

	sender := subject.newAgent(t, "claude-a")
	recipient := subject.newAgent(t, "claude-b")

	sent, err := subject.service.Send(t.Context(), messaging.SendRequest{
		FromAgentID: sender.ID, ToAgentName: recipient.Name,
		Kind: "status", Body: []byte(`{"working":"on the parser"}`),
	})
	if err != nil {
		t.Fatalf("Send() = %v, want nil", err)
	}
	if len(sent.RecipientIDs) != 1 || sent.RecipientIDs[0] != recipient.ID {
		t.Errorf("recipients = %v, want just %q", sent.RecipientIDs, recipient.ID)
	}

	inbox, err := subject.service.Inbox(t.Context(), recipient.ID, 0)
	if err != nil {
		t.Fatalf("Inbox() = %v, want nil", err)
	}
	if len(inbox) != 1 || inbox[0].FromAgentID != sender.ID {
		t.Fatalf("Inbox() = %v, want the message that was sent", inbox)
	}
	if string(inbox[0].Body) != `{"working":"on the parser"}` {
		t.Errorf("body = %s, want it unchanged", inbox[0].Body)
	}
}

// The sender is taken from the authenticated connection, so a message can always be trusted to say
// who really sent it.
func TestTheSenderIsRecordedFromTheConnection(t *testing.T) {
	subject := newHarness(t, nil)
	sender := subject.newAgent(t, "claude-a")
	recipient := subject.newAgent(t, "claude-b")

	sent, err := subject.service.Send(t.Context(), messaging.SendRequest{
		FromAgentID: sender.ID, ToAgentName: recipient.Name,
		Kind: "status", Body: []byte(`{"a":1}`),
	})
	if err != nil {
		t.Fatalf("Send() = %v, want nil", err)
	}
	if sent.Message.FromAgentID != sender.ID {
		t.Errorf("sender = %q, want %q", sent.Message.FromAgentID, sender.ID)
	}
}

// Reading is not acknowledging: an agent that crashes between the two must see the message again.
func TestReadingIsNotAcknowledging(t *testing.T) {
	subject := newHarness(t, nil)
	sender := subject.newAgent(t, "claude-a")
	recipient := subject.newAgent(t, "claude-b")

	if _, err := subject.service.Send(t.Context(), messaging.SendRequest{
		FromAgentID: sender.ID, ToAgentName: recipient.Name, Kind: "status", Body: []byte(`{"a":1}`),
	}); err != nil {
		t.Fatalf("Send() = %v, want nil", err)
	}

	first, err := subject.service.Inbox(t.Context(), recipient.ID, 0)
	if err != nil || len(first) != 1 {
		t.Fatalf("Inbox() = %v (%v), want one message", first, err)
	}

	again, err := subject.service.Inbox(t.Context(), recipient.ID, 0)
	if err != nil || len(again) != 1 {
		t.Fatalf("a second Inbox() = %v (%v), want the message again", again, err)
	}

	acknowledged, err := subject.service.Acknowledge(t.Context(), recipient.ID, []string{first[0].ID})
	if err != nil || acknowledged != 1 {
		t.Fatalf("Acknowledge() = %d (%v), want 1", acknowledged, err)
	}

	empty, err := subject.service.Inbox(t.Context(), recipient.ID, 0)
	if err != nil || len(empty) != 0 {
		t.Errorf("Inbox() after acknowledgement = %v (%v), want nothing", empty, err)
	}
}

// A message sent to an agent that is not connected must simply wait for it.
func TestMessagesWaitForAnAbsentRecipient(t *testing.T) {
	notifier := newRecordingNotifier() // nobody is connected
	subject := newHarness(t, notifier)

	sender := subject.newAgent(t, "claude-a")
	recipient := subject.newAgent(t, "claude-b")

	sent, err := subject.service.Send(t.Context(), messaging.SendRequest{
		FromAgentID: sender.ID, ToAgentName: recipient.Name, Kind: "status", Body: []byte(`{"a":1}`),
	})
	if err != nil {
		t.Fatalf("Send() = %v, want nil", err)
	}
	if sent.Notified != 0 {
		t.Errorf("Notified = %d, want 0 when nobody is connected", sent.Notified)
	}

	inbox, err := subject.service.Inbox(t.Context(), recipient.ID, 0)
	if err != nil || len(inbox) != 1 {
		t.Errorf("the message did not wait for the recipient: %v (%v)", inbox, err)
	}
}

// A connected recipient hears about a message immediately, so nothing has to poll.
func TestConnectedRecipientsAreNotified(t *testing.T) {
	subject := newHarness(t, nil)
	sender := subject.newAgent(t, "claude-a")
	recipient := subject.newAgent(t, "claude-b")

	notifier := newRecordingNotifier(recipient.ID)
	subject.service = messaging.New(subject.store, messaging.Options{
		Notifier: notifier,
		Clock:    func() time.Time { return baseTime },
		Logger:   quietLogger(),
	})

	sent, err := subject.service.Send(t.Context(), messaging.SendRequest{
		FromAgentID: sender.ID, ToAgentName: recipient.Name, Kind: "status", Body: []byte(`{"a":1}`),
	})
	if err != nil {
		t.Fatalf("Send() = %v, want nil", err)
	}
	if sent.Notified != 1 {
		t.Errorf("Notified = %d, want 1", sent.Notified)
	}
	if notifier.count(recipient.ID) != 1 {
		t.Errorf("the recipient was notified %d times, want 1", notifier.count(recipient.ID))
	}
	if notifier.count(sender.ID) != 0 {
		t.Error("the sender was notified about its own message")
	}
}

// A broadcast reaches everyone else, and never the sender: an agent does not need telling what it
// just said.
func TestBroadcastReachesEveryoneElse(t *testing.T) {
	subject := newHarness(t, nil)
	sender := subject.newAgent(t, "claude-a")
	first := subject.newAgent(t, "claude-b")
	second := subject.newAgent(t, "claude-c")

	sent, err := subject.service.Send(t.Context(), messaging.SendRequest{
		FromAgentID: sender.ID, Topic: core.BroadcastTopic,
		Kind: "status", Body: []byte(`{"note":"rebasing now"}`),
	})
	if err != nil {
		t.Fatalf("Send() = %v, want nil", err)
	}
	if len(sent.RecipientIDs) != 2 {
		t.Fatalf("the broadcast reached %d agents, want 2", len(sent.RecipientIDs))
	}

	for _, agent := range []core.Agent{first, second} {
		inbox, err := subject.service.Inbox(t.Context(), agent.ID, 0)
		if err != nil || len(inbox) != 1 {
			t.Errorf("%q received %d messages (%v), want 1", agent.Name, len(inbox), err)
		}
	}

	own, err := subject.service.Inbox(t.Context(), sender.ID, 0)
	if err != nil || len(own) != 0 {
		t.Errorf("the sender received its own broadcast: %v (%v)", own, err)
	}
}

func TestTopicsDeliverToSubscribersOnly(t *testing.T) {
	subject := newHarness(t, nil)
	sender := subject.newAgent(t, "claude-a")
	subscriber := subject.newAgent(t, "claude-b")
	bystander := subject.newAgent(t, "claude-c")

	if err := subject.service.Subscribe(t.Context(), subscriber.ID, "backend"); err != nil {
		t.Fatalf("Subscribe() = %v, want nil", err)
	}

	if _, err := subject.service.Send(t.Context(), messaging.SendRequest{
		FromAgentID: sender.ID, Topic: "backend", Kind: "status", Body: []byte(`{"a":1}`),
	}); err != nil {
		t.Fatalf("Send() = %v, want nil", err)
	}

	subscribed, err := subject.service.Inbox(t.Context(), subscriber.ID, 0)
	if err != nil || len(subscribed) != 1 {
		t.Errorf("the subscriber received %d messages (%v), want 1", len(subscribed), err)
	}

	ignored, err := subject.service.Inbox(t.Context(), bystander.ID, 0)
	if err != nil || len(ignored) != 0 {
		t.Errorf("a non-subscriber received %d messages (%v), want 0", len(ignored), err)
	}

	if err := subject.service.Unsubscribe(t.Context(), subscriber.ID, "backend"); err != nil {
		t.Fatalf("Unsubscribe() = %v, want nil", err)
	}

	_, err = subject.service.Send(t.Context(), messaging.SendRequest{
		FromAgentID: sender.ID, Topic: "backend", Kind: "status", Body: []byte(`{"a":2}`),
	})
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("sending to an empty topic = %v, want ErrNotFound", err)
	}
}

// Sending to a name nobody answers to must say so, rather than quietly going nowhere.
func TestSendingToAnUnknownAgent(t *testing.T) {
	subject := newHarness(t, nil)
	sender := subject.newAgent(t, "claude-a")

	_, err := subject.service.Send(t.Context(), messaging.SendRequest{
		FromAgentID: sender.ID, ToAgentName: "nobody", Kind: "status", Body: []byte(`{"a":1}`),
	})
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("Send() = %v, want ErrNotFound", err)
	}
}

func TestSendRejectsBadMessages(t *testing.T) {
	subject := newHarness(t, nil)
	sender := subject.newAgent(t, "claude-a")
	recipient := subject.newAgent(t, "claude-b")

	tests := map[string]messaging.SendRequest{
		"no kind":       {FromAgentID: sender.ID, ToAgentName: recipient.Name, Body: []byte(`{"a":1}`)},
		"no body":       {FromAgentID: sender.ID, ToAgentName: recipient.Name, Kind: "status"},
		"body not json": {FromAgentID: sender.ID, ToAgentName: recipient.Name, Kind: "status", Body: []byte(`not json`)},
		"bad kind":      {FromAgentID: sender.ID, ToAgentName: recipient.Name, Kind: "Status Update", Body: []byte(`{}`)},
	}

	for name, request := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := subject.service.Send(t.Context(), request); !errors.Is(err, core.ErrInvalidArgument) {
				t.Errorf("Send() = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

// One agent must not be able to clear another's inbox.
func TestAcknowledgingSomebodyElsesMessagesDoesNothing(t *testing.T) {
	subject := newHarness(t, nil)
	sender := subject.newAgent(t, "claude-a")
	recipient := subject.newAgent(t, "claude-b")
	stranger := subject.newAgent(t, "claude-c")

	sent, err := subject.service.Send(t.Context(), messaging.SendRequest{
		FromAgentID: sender.ID, ToAgentName: recipient.Name, Kind: "status", Body: []byte(`{"a":1}`),
	})
	if err != nil {
		t.Fatalf("Send() = %v, want nil", err)
	}

	acknowledged, err := subject.service.Acknowledge(t.Context(), stranger.ID, []string{sent.Message.ID})
	if err != nil {
		t.Fatalf("Acknowledge() = %v, want nil", err)
	}
	if acknowledged != 0 {
		t.Errorf("a stranger acknowledged %d messages, want 0", acknowledged)
	}

	inbox, err := subject.service.Inbox(t.Context(), recipient.ID, 0)
	if err != nil || len(inbox) != 1 {
		t.Errorf("the real recipient lost its message: %v (%v)", inbox, err)
	}
}

// Every agent is on the broadcast topic already; a subscription would look like it did something.
func TestSubscribingToTheBroadcastTopicIsRefused(t *testing.T) {
	subject := newHarness(t, nil)
	agent := subject.newAgent(t, "claude-a")

	err := subject.service.Subscribe(t.Context(), agent.ID, core.BroadcastTopic)
	if !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("Subscribe(%q) = %v, want ErrInvalidArgument", core.BroadcastTopic, err)
	}
}

// An agent that stopped heartbeating is quiet, not gone. Its inbox must still accumulate, or
// "the message waits for the recipient" stops being true for exactly the agent most likely to
// need it — one that was restarting, or busy for a long time.
func TestAMessageReachesAnAgentThatWentQuiet(t *testing.T) {
	subject := newHarness(t, nil)
	sender := subject.newAgent(t, "claude-a")
	quiet := subject.newAgent(t, "claude-b")

	if err := subject.store.Agents().SetState(t.Context(), quiet.ID, core.AgentStale, baseTime); err != nil {
		t.Fatalf("SetState() = %v, want nil", err)
	}

	sent, err := subject.service.Send(t.Context(), messaging.SendRequest{
		FromAgentID: sender.ID, ToAgentName: quiet.Name,
		Kind: "status", Body: []byte(`{"text":"your CI passed"}`),
	})
	if err != nil {
		t.Fatalf("Send() to a stale agent = %v, want nil", err)
	}
	if len(sent.RecipientIDs) != 1 || sent.RecipientIDs[0] != quiet.ID {
		t.Fatalf("recipients = %v, want the quiet agent", sent.RecipientIDs)
	}

	// And it is there when the agent comes back.
	inbox, err := subject.service.Inbox(t.Context(), quiet.ID, 0)
	if err != nil || len(inbox) != 1 {
		t.Errorf("the quiet agent has %d messages waiting (%v), want 1", len(inbox), err)
	}
}

// An agent that said goodbye is a different matter: it is not addressable, and pretending the
// message went somewhere would be worse than saying so.
func TestAMessageToADeregisteredAgentIsRefused(t *testing.T) {
	subject := newHarness(t, nil)
	sender := subject.newAgent(t, "claude-a")
	gone := subject.newAgent(t, "claude-b")

	if err := subject.store.Agents().SetState(t.Context(), gone.ID, core.AgentDeregistered, baseTime); err != nil {
		t.Fatalf("SetState() = %v, want nil", err)
	}

	_, err := subject.service.Send(t.Context(), messaging.SendRequest{
		FromAgentID: sender.ID, ToAgentName: gone.Name,
		Kind: "status", Body: []byte(`{"a":1}`),
	})
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("Send() to a deregistered agent = %v, want ErrNotFound", err)
	}
}

// When a name has been used more than once, the live agent wins: a message should go to whoever is
// answering to it now, not to a predecessor.
func TestDeliveryPrefersTheActiveAgent(t *testing.T) {
	subject := newHarness(t, nil)
	sender := subject.newAgent(t, "claude-a")
	first := subject.newAgent(t, "claude-b")

	// The first one goes quiet, and another registers under the same name.
	if err := subject.store.Agents().SetState(t.Context(), first.ID, core.AgentStale, baseTime); err != nil {
		t.Fatalf("SetState() = %v, want nil", err)
	}
	second := subject.newAgent(t, "claude-b")

	sent, err := subject.service.Send(t.Context(), messaging.SendRequest{
		FromAgentID: sender.ID, ToAgentName: "claude-b",
		Kind: "status", Body: []byte(`{"a":1}`),
	})
	if err != nil {
		t.Fatalf("Send() = %v, want nil", err)
	}
	if len(sent.RecipientIDs) != 1 || sent.RecipientIDs[0] != second.ID {
		t.Errorf("the message went to %v, want the agent answering to the name now (%s)",
			sent.RecipientIDs, second.ID)
	}
}
