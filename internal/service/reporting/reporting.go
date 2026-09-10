// Package reporting lets the operator ask every agent what it is doing, and read the answers in
// one place.
//
// A request goes out as an ordinary message, so an agent that is busy or restarting still receives
// it, and collection waits for a deadline rather than forever: an agent that never answers is
// listed as silent, because "nobody told me" is the interesting case.
package reporting

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"code.sadeq.uk/lac/internal/auth"
	"code.sadeq.uk/lac/internal/core"
	"code.sadeq.uk/lac/internal/id"
	"code.sadeq.uk/lac/internal/service/messaging"
	"code.sadeq.uk/lac/internal/service/registry"
)

// MessageKind is the kind given to the message that carries a report request, so an agent can
// route on it rather than reading English.
const MessageKind = "report-request"

// DefaultDeadline is how long collection waits when the caller does not say.
const DefaultDeadline = 30 * time.Second

// recheckInterval is a safety net, not the mechanism: an answer normally wakes the collector at
// once. It matters when the answer is recorded through a different path — a second daemon process,
// or a future front end with its own service instance — where the in-process signal would not
// reach us and only the deadline would.
const recheckInterval = 250 * time.Millisecond

// Options configure a Service.
type Options struct {
	// Clock defaults to the wall clock in UTC.
	Clock auth.Clock
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Service asks the agents for reports and collects the answers.
type Service struct {
	store     core.Store
	registry  *registry.Service
	messaging *messaging.Service
	arrivals  *arrivals
	now       auth.Clock
	logger    *slog.Logger
}

// New returns a reporting service.
func New(
	store core.Store,
	registryService *registry.Service,
	messagingService *messaging.Service,
	options Options,
) *Service {
	if options.Clock == nil {
		options.Clock = func() time.Time { return time.Now().UTC() }
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}

	return &Service{
		store:     store,
		registry:  registryService,
		messaging: messagingService,
		arrivals:  newArrivals(),
		now:       options.Clock,
		logger:    options.Logger,
	}
}

// Request asks every agent that is present what it is doing.
//
// The requester is not asked: an operator wanting a report does not need one from their own shell.
func (s *Service) Request(
	ctx context.Context, requesterID, question string, deadline time.Duration,
) (core.ReportRequest, error) {
	if question == "" {
		return core.ReportRequest{}, fmt.Errorf("%w: a report request needs a question", core.ErrInvalidArgument)
	}
	if deadline <= 0 {
		deadline = DefaultDeadline
	}

	present, err := s.registry.Active(ctx)
	if err != nil {
		return core.ReportRequest{}, err
	}

	asked := make([]string, 0, len(present))
	for _, agent := range present {
		if agent.ID != requesterID {
			asked = append(asked, agent.ID)
		}
	}

	now := s.now()
	request := core.ReportRequest{
		ID:            id.New("report"),
		RequesterID:   requesterID,
		Question:      question,
		AskedAgentIDs: asked,
		CreatedAt:     now,
		Deadline:      now.Add(deadline),
	}

	if err := s.store.Reports().CreateRequest(ctx, request); err != nil {
		return core.ReportRequest{}, err
	}

	s.audit(ctx, requesterID, request.ID,
		fmt.Sprintf("asked %d agent(s): %s", len(asked), question))

	if len(asked) == 0 {
		return request, nil
	}

	if err := s.deliver(ctx, request); err != nil {
		return core.ReportRequest{}, err
	}

	return request, nil
}

// deliver sends the question to the agents as an ordinary message, which is what makes it reach an
// agent that is busy, restarting, or simply not looking yet.
func (s *Service) deliver(ctx context.Context, request core.ReportRequest) error {
	body, err := json.Marshal(map[string]string{
		"request_id": request.ID,
		"question":   request.Question,
		"deadline":   request.Deadline.Format(time.RFC3339),
		"reply_with": "report.submit",
	})
	if err != nil {
		return fmt.Errorf("encoding the report request: %w", err)
	}

	for _, agentID := range request.AskedAgentIDs {
		agent, err := s.registry.ByID(ctx, agentID)
		if err != nil {
			s.logger.Warn("could not address a report request", "agent", agentID, "error", err)
			continue
		}

		if _, err := s.messaging.Send(ctx, messaging.SendRequest{
			FromAgentID: request.RequesterID,
			ToAgentName: agent.Name,
			Kind:        MessageKind,
			Body:        body,
		}); err != nil {
			s.logger.Warn("could not deliver a report request",
				"agent", agent.Name, "error", err)
		}
	}

	return nil
}

// Submit records one agent's answer.
func (s *Service) Submit(ctx context.Context, requestID, agentID, body string) error {
	if body == "" {
		return fmt.Errorf("%w: a report needs something in it", core.ErrInvalidArgument)
	}

	request, err := s.store.Reports().RequestByID(ctx, requestID)
	if err != nil {
		return err
	}

	report := core.Report{
		RequestID: request.ID,
		AgentID:   agentID,
		Body:      body,
		CreatedAt: s.now(),
	}

	if err := s.store.Reports().AddReport(ctx, report); err != nil {
		return err
	}

	// Whoever is collecting can stop waiting as soon as the last answer lands.
	s.arrivals.signal(requestID)

	return nil
}

// Collect waits for the answers and returns them with the agents that stayed silent.
//
// It returns as soon as everyone who was asked has answered, and otherwise at the deadline. A
// caller that does not want to wait passes a context it cancels.
func (s *Service) Collect(ctx context.Context, requestID string) (core.ReportCollection, error) {
	request, err := s.store.Reports().RequestByID(ctx, requestID)
	if err != nil {
		return core.ReportCollection{}, err
	}

	for {
		// Watch before reading, so an answer that lands while we are reading still wakes us.
		arrived := s.arrivals.watch(requestID)

		collection, complete, err := s.snapshot(ctx, request)
		if err != nil {
			return core.ReportCollection{}, err
		}
		if complete {
			return collection, nil
		}

		remaining := request.Deadline.Sub(s.now())
		if remaining <= 0 {
			return collection, nil
		}

		timer := time.NewTimer(min(remaining, recheckInterval))

		select {
		case <-arrived:
			timer.Stop()
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return collection, nil
		}
	}
}

// Snapshot returns what has been collected so far, without waiting.
func (s *Service) Snapshot(ctx context.Context, requestID string) (core.ReportCollection, error) {
	request, err := s.store.Reports().RequestByID(ctx, requestID)
	if err != nil {
		return core.ReportCollection{}, err
	}

	collection, _, err := s.snapshot(ctx, request)

	return collection, err
}

func (s *Service) snapshot(
	ctx context.Context, request core.ReportRequest,
) (collection core.ReportCollection, complete bool, err error) {
	reports, err := s.store.Reports().ReportsFor(ctx, request.ID)
	if err != nil {
		return core.ReportCollection{}, false, err
	}

	answered := make(map[string]struct{}, len(reports))
	for _, report := range reports {
		answered[report.AgentID] = struct{}{}
	}

	silent := make([]string, 0, len(request.AskedAgentIDs))
	for _, agentID := range request.AskedAgentIDs {
		if _, found := answered[agentID]; !found {
			silent = append(silent, agentID)
		}
	}

	return core.ReportCollection{
		Request:        request,
		Reports:        reports,
		SilentAgentIDs: silent,
	}, len(silent) == 0, nil
}

func (s *Service) audit(ctx context.Context, actor, target, detail string) {
	entry := core.AuditEntry{
		Actor:  actor,
		Action: core.AuditReportRequested,
		Target: target,
		Detail: detail,
		At:     s.now(),
	}

	if err := s.store.Audit().Append(ctx, entry); err != nil {
		s.logger.Error("could not write an audit entry", "action", entry.Action, "error", err)
	}
}

// arrivals wakes a collector when an answer lands, so collection is not a poll.
type arrivals struct {
	mutex    sync.Mutex
	channels map[string]chan struct{}
}

func newArrivals() *arrivals {
	return &arrivals{channels: make(map[string]chan struct{})}
}

func (a *arrivals) watch(requestID string) <-chan struct{} {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	channel, found := a.channels[requestID]
	if !found {
		channel = make(chan struct{})
		a.channels[requestID] = channel
	}

	return channel
}

func (a *arrivals) signal(requestID string) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if channel, found := a.channels[requestID]; found {
		close(channel)
		delete(a.channels, requestID)
	}
}
