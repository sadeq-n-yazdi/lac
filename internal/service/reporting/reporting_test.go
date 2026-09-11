package reporting_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"sadeq.uk/lac/internal/auth"
	"sadeq.uk/lac/internal/core"
	"sadeq.uk/lac/internal/service/messaging"
	"sadeq.uk/lac/internal/service/registry"
	"sadeq.uk/lac/internal/service/reporting"
	"sadeq.uk/lac/internal/store/sqlite"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type harness struct {
	service   *reporting.Service
	registry  *registry.Service
	messaging *messaging.Service
	store     *sqlite.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "lac.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	secret, err := auth.LoadOrCreateSecret(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatalf("creating the secret: %v", err)
	}

	authenticator, err := auth.New(store, secret, auth.Options{Logger: quietLogger()})
	if err != nil {
		t.Fatalf("creating the authenticator: %v", err)
	}

	registryService := registry.New(store, authenticator, registry.Options{
		TimeToLive: time.Hour,
		CapabilitiesFor: func(string) core.Capabilities {
			return core.Capabilities{Resources: []string{core.WildcardResource}, CanBroadcast: true}
		},
		Logger: quietLogger(),
	})

	messagingService := messaging.New(store, messaging.Options{Logger: quietLogger()})

	return &harness{
		service:   reporting.New(store, registryService, messagingService, reporting.Options{Logger: quietLogger()}),
		registry:  registryService,
		messaging: messagingService,
		store:     store,
	}
}

func (h *harness) register(t *testing.T, name string, processID int) core.Agent {
	t.Helper()

	registration, err := h.registry.Register(t.Context(), registry.RegisterRequest{
		Name: name, Kind: "claude", Workdir: "/tmp/" + name, ProcessID: processID,
	})
	if err != nil {
		t.Fatalf("registering %q: %v", name, err)
	}

	return registration.Agent
}

// The operator asks, the agents answer, and the answers come back together. This is the whole
// feature.
func TestRequestAndCollect(t *testing.T) {
	subject := newHarness(t)
	operator := subject.register(t, "operator", 1)
	first := subject.register(t, "claude-a", 2)
	second := subject.register(t, "claude-b", 3)

	request, err := subject.service.Request(t.Context(), operator.ID, "what are you working on?", time.Minute)
	if err != nil {
		t.Fatalf("Request() = %v, want nil", err)
	}
	if len(request.AskedAgentIDs) != 2 {
		t.Fatalf("asked %d agents, want 2", len(request.AskedAgentIDs))
	}

	if err := subject.service.Submit(t.Context(), request.ID, first.ID, "the parser"); err != nil {
		t.Fatalf("Submit() = %v, want nil", err)
	}
	if err := subject.service.Submit(t.Context(), request.ID, second.ID, "the tests"); err != nil {
		t.Fatalf("Submit() = %v, want nil", err)
	}

	collection, err := subject.service.Collect(t.Context(), request.ID)
	if err != nil {
		t.Fatalf("Collect() = %v, want nil", err)
	}
	if len(collection.Reports) != 2 {
		t.Errorf("collected %d reports, want 2", len(collection.Reports))
	}
	if len(collection.SilentAgentIDs) != 0 {
		t.Errorf("silent = %v, want nobody", collection.SilentAgentIDs)
	}
}

// The operator does not need a report from their own shell.
func TestTheRequesterIsNotAsked(t *testing.T) {
	subject := newHarness(t)
	operator := subject.register(t, "operator", 1)
	subject.register(t, "claude-a", 2)

	request, err := subject.service.Request(t.Context(), operator.ID, "status?", time.Minute)
	if err != nil {
		t.Fatalf("Request() = %v, want nil", err)
	}

	for _, agentID := range request.AskedAgentIDs {
		if agentID == operator.ID {
			t.Error("the requester was asked to report to itself")
		}
	}
}

// An agent that never answers must be named, because that is the interesting case: it may be stuck.
func TestSilentAgentsAreListed(t *testing.T) {
	subject := newHarness(t)
	operator := subject.register(t, "operator", 1)
	answering := subject.register(t, "claude-a", 2)
	silent := subject.register(t, "claude-b", 3)

	// A short deadline, so collection gives up quickly rather than the test waiting.
	request, err := subject.service.Request(t.Context(), operator.ID, "status?", 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Request() = %v, want nil", err)
	}

	if err := subject.service.Submit(t.Context(), request.ID, answering.ID, "the parser"); err != nil {
		t.Fatalf("Submit() = %v, want nil", err)
	}

	collection, err := subject.service.Collect(t.Context(), request.ID)
	if err != nil {
		t.Fatalf("Collect() = %v, want nil", err)
	}

	if len(collection.Reports) != 1 {
		t.Errorf("collected %d reports, want 1", len(collection.Reports))
	}
	if len(collection.SilentAgentIDs) != 1 || collection.SilentAgentIDs[0] != silent.ID {
		t.Errorf("silent = %v, want just the agent that never answered", collection.SilentAgentIDs)
	}
}

// Collection must return as soon as the last answer lands, not sit out the deadline.
func TestCollectReturnsAsSoonAsEveryoneHasAnswered(t *testing.T) {
	subject := newHarness(t)
	operator := subject.register(t, "operator", 1)
	agent := subject.register(t, "claude-a", 2)

	request, err := subject.service.Request(t.Context(), operator.ID, "status?", time.Hour)
	if err != nil {
		t.Fatalf("Request() = %v, want nil", err)
	}

	collected := make(chan core.ReportCollection, 1)
	go func() {
		collection, err := subject.service.Collect(t.Context(), request.ID)
		if err != nil {
			t.Errorf("Collect() = %v, want nil", err)
			return
		}
		collected <- collection
	}()

	// Give the collector a moment to start waiting, then answer.
	time.Sleep(20 * time.Millisecond)
	if err := subject.service.Submit(t.Context(), request.ID, agent.ID, "the parser"); err != nil {
		t.Fatalf("Submit() = %v, want nil", err)
	}

	select {
	case collection := <-collected:
		if len(collection.Reports) != 1 {
			t.Errorf("collected %d reports, want 1", len(collection.Reports))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Collect() waited for the deadline instead of returning when the last answer arrived")
	}
}

// The request reaches the agents as an ordinary message, which is what makes it survive an agent
// that is busy or restarting.
func TestTheQuestionArrivesAsAMessage(t *testing.T) {
	subject := newHarness(t)
	operator := subject.register(t, "operator", 1)
	agent := subject.register(t, "claude-a", 2)

	request, err := subject.service.Request(t.Context(), operator.ID, "what are you working on?", time.Minute)
	if err != nil {
		t.Fatalf("Request() = %v, want nil", err)
	}

	inbox, err := subject.messaging.Inbox(t.Context(), agent.ID, 0)
	if err != nil {
		t.Fatalf("Inbox() = %v, want nil", err)
	}
	if len(inbox) != 1 {
		t.Fatalf("the agent received %d messages, want the report request", len(inbox))
	}
	if inbox[0].Kind != reporting.MessageKind {
		t.Errorf("message kind = %q, want %q", inbox[0].Kind, reporting.MessageKind)
	}

	var body struct {
		RequestID string `json:"request_id"`
		Question  string `json:"question"`
	}
	if err := json.Unmarshal(inbox[0].Body, &body); err != nil {
		t.Fatalf("decoding the request body: %v", err)
	}
	if body.RequestID != request.ID {
		t.Errorf("request_id = %q, want %q", body.RequestID, request.ID)
	}
	if body.Question != "what are you working on?" {
		t.Errorf("question = %q, want what was asked", body.Question)
	}
}

// An agent that corrects itself must replace its answer rather than appear twice.
func TestAnAgentCanCorrectItsAnswer(t *testing.T) {
	subject := newHarness(t)
	operator := subject.register(t, "operator", 1)
	agent := subject.register(t, "claude-a", 2)

	request, err := subject.service.Request(t.Context(), operator.ID, "status?", time.Minute)
	if err != nil {
		t.Fatalf("Request() = %v, want nil", err)
	}

	for _, answer := range []string{"the parser", "the parser, and now the tests"} {
		if err := subject.service.Submit(t.Context(), request.ID, agent.ID, answer); err != nil {
			t.Fatalf("Submit() = %v, want nil", err)
		}
	}

	collection, err := subject.service.Collect(t.Context(), request.ID)
	if err != nil {
		t.Fatalf("Collect() = %v, want nil", err)
	}
	if len(collection.Reports) != 1 {
		t.Fatalf("collected %d reports, want 1", len(collection.Reports))
	}
	if collection.Reports[0].Body != "the parser, and now the tests" {
		t.Errorf("body = %q, want the corrected answer", collection.Reports[0].Body)
	}
}

// Asking when nobody is there must be a quiet, complete answer rather than a wait.
func TestRequestWithNobodyToAsk(t *testing.T) {
	subject := newHarness(t)
	operator := subject.register(t, "operator", 1)

	request, err := subject.service.Request(t.Context(), operator.ID, "status?", time.Hour)
	if err != nil {
		t.Fatalf("Request() = %v, want nil", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	collection, err := subject.service.Collect(ctx, request.ID)
	if err != nil {
		t.Fatalf("Collect() = %v, want nil", err)
	}
	if len(collection.Reports) != 0 || len(collection.SilentAgentIDs) != 0 {
		t.Errorf("collection = %+v, want nothing asked and nothing silent", collection)
	}
}

func TestSubmitRejectsUnknownRequests(t *testing.T) {
	subject := newHarness(t)
	agent := subject.register(t, "claude-a", 1)

	err := subject.service.Submit(t.Context(), "report_nobody_asked", agent.ID, "the parser")
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("Submit() = %v, want ErrNotFound", err)
	}
}

func TestRequestNeedsAQuestion(t *testing.T) {
	subject := newHarness(t)
	operator := subject.register(t, "operator", 1)

	_, err := subject.service.Request(t.Context(), operator.ID, "", time.Minute)
	if !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("Request() = %v, want ErrInvalidArgument", err)
	}
}
