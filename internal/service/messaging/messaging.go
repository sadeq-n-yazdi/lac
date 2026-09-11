// Package messaging carries what one agent wants to tell another.
//
// Delivery is durable: a message is kept until its recipient acknowledges it, so an agent that was
// restarting, or busy, or not yet started, still gets what it missed. Agents that are connected are
// also pushed a notification, so nothing has to poll.
package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"sadeq.uk/lac/internal/auth"
	"sadeq.uk/lac/internal/core"
	"sadeq.uk/lac/internal/id"
)

// NotificationMethod is the method name pushed to a connected recipient when a message arrives.
const NotificationMethod = "message.received"

// Notifier pushes a notification to whatever sessions an agent has open. The JSON-RPC server
// implements it; the service depends on the interface so it never has to know about transports.
type Notifier interface {
	// Broadcast sends the notification to every open session of the agent and reports how many it
	// reached. Zero simply means the agent is not connected right now.
	Broadcast(agentID, method string, params any) int
}

// Options configure a Service.
type Options struct {
	// DefaultTimeToLive is how long an unacknowledged message is kept. Zero means forever.
	DefaultTimeToLive time.Duration
	// Notifier pushes arrivals to connected recipients. Optional: without one, messages are still
	// delivered, just only when the recipient next looks.
	Notifier Notifier
	// Clock defaults to the wall clock in UTC.
	Clock auth.Clock
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Service delivers messages between agents.
type Service struct {
	store    core.Store
	notifier Notifier
	now      auth.Clock
	logger   *slog.Logger
	options  Options
}

// New returns a messaging service.
func New(store core.Store, options Options) *Service {
	if options.Clock == nil {
		options.Clock = func() time.Time { return time.Now().UTC() }
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}

	return &Service{
		store:    store,
		notifier: options.Notifier,
		now:      options.Clock,
		logger:   options.Logger,
		options:  options,
	}
}

// SendRequest is one message being sent.
type SendRequest struct {
	// FromAgentID is the sender, taken from the authenticated connection so it cannot be forged.
	FromAgentID string
	// ToAgentName addresses a single agent by name. Exactly one of this and Topic is set.
	ToAgentName string
	// Topic addresses everyone subscribed to it. The topic "all" reaches every live agent.
	Topic string
	// Kind is a label the recipient can route on, such as "question" or "status".
	Kind string
	// Body is the payload, as JSON.
	Body json.RawMessage
}

// Sent describes what happened to a message.
type Sent struct {
	// Message is what was stored.
	Message core.Message
	// RecipientIDs are the agents it was queued for.
	RecipientIDs []string
	// Notified is how many of them were connected and heard about it immediately.
	Notified int
}

// Send stores a message for its recipients and nudges the ones that are connected.
//
// The whole thing is one transaction: a message never exists without the deliveries that make it
// reach somebody.
func (s *Service) Send(ctx context.Context, request SendRequest) (Sent, error) {
	if err := core.ValidateName("message kind", request.Kind); err != nil {
		return Sent{}, err
	}
	if len(request.Body) == 0 {
		return Sent{}, fmt.Errorf("%w: a message needs a body", core.ErrInvalidArgument)
	}
	if !json.Valid(request.Body) {
		return Sent{}, fmt.Errorf("%w: the message body must be JSON", core.ErrInvalidArgument)
	}

	message := core.Message{
		ID:          id.New("msg"),
		FromAgentID: request.FromAgentID,
		Topic:       request.Topic,
		Kind:        request.Kind,
		Body:        request.Body,
		CreatedAt:   s.now(),
	}
	if s.options.DefaultTimeToLive > 0 {
		message.ExpiresAt = message.CreatedAt.Add(s.options.DefaultTimeToLive)
	}

	var recipients []string

	if err := s.store.InTransaction(ctx, func(tx core.Store) error {
		resolved, err := s.resolveRecipients(ctx, tx, request, message.FromAgentID)
		if err != nil {
			return err
		}
		if len(resolved) == 0 {
			return fmt.Errorf("%w: nobody is listening", core.ErrNotFound)
		}

		if request.ToAgentName != "" {
			message.ToAgentID = resolved[0]
		}
		recipients = resolved

		if err := tx.Messages().Append(ctx, message, recipients); err != nil {
			return err
		}

		return tx.Audit().Append(ctx, core.AuditEntry{
			Actor:  message.FromAgentID,
			Action: core.AuditMessageSent,
			Target: destinationOf(message),
			Detail: fmt.Sprintf("kind=%s recipients=%d", message.Kind, len(recipients)),
			At:     message.CreatedAt,
		})
	}); err != nil {
		return Sent{}, err
	}

	return Sent{
		Message:      message,
		RecipientIDs: recipients,
		Notified:     s.notify(message, recipients),
	}, nil
}

// resolveRecipients turns an address into the agents that should receive the message.
func (s *Service) resolveRecipients(
	ctx context.Context, tx core.Store, request SendRequest, senderID string,
) ([]string, error) {
	if request.ToAgentName != "" {
		recipient, err := tx.Agents().ByName(ctx, request.ToAgentName)
		if err != nil {
			return nil, fmt.Errorf("no live agent is called %q: %w", request.ToAgentName, err)
		}

		return []string{recipient.ID}, nil
	}

	if request.Topic == core.BroadcastTopic {
		agents, err := tx.Agents().List(ctx, core.AgentFilter{States: []core.AgentState{core.AgentActive}})
		if err != nil {
			return nil, err
		}

		return everyoneBut(agents, senderID), nil
	}

	subscribers, err := tx.Messages().Subscribers(ctx, request.Topic)
	if err != nil {
		return nil, err
	}

	return withoutSender(subscribers, senderID), nil
}

// Inbox returns what an agent has not acknowledged yet, and marks it delivered.
func (s *Service) Inbox(ctx context.Context, agentID string, limit int) ([]core.Message, error) {
	messages, err := s.store.Messages().Pending(ctx, agentID, limit)
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return messages, nil
	}

	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}

	// Marking them delivered is not acknowledging them: an agent that crashes before it acts on a
	// message must see it again.
	if err := s.store.Messages().MarkDelivered(ctx, agentID, ids, s.now()); err != nil {
		return nil, err
	}

	return messages, nil
}

// Acknowledge confirms an agent has dealt with messages, and reports how many it actually changed.
func (s *Service) Acknowledge(ctx context.Context, agentID string, messageIDs []string) (int, error) {
	acknowledged, err := s.store.Messages().Acknowledge(ctx, agentID, messageIDs, s.now())
	if err != nil {
		return 0, err
	}

	return acknowledged, nil
}

// Subscribe adds an agent to a topic.
func (s *Service) Subscribe(ctx context.Context, agentID, topic string) error {
	if topic == core.BroadcastTopic {
		// Everyone is on the broadcast topic by definition; a subscription would be a no-op that
		// looks like it did something.
		return fmt.Errorf("%w: every agent already receives the %q topic", core.ErrInvalidArgument, topic)
	}

	return s.store.Messages().Subscribe(ctx, agentID, topic)
}

// Unsubscribe removes an agent from a topic.
func (s *Service) Unsubscribe(ctx context.Context, agentID, topic string) error {
	return s.store.Messages().Unsubscribe(ctx, agentID, topic)
}

// Prune removes messages nobody is waiting for any more.
func (s *Service) Prune(ctx context.Context) (int, error) {
	removed, err := s.store.Messages().PruneExpired(ctx, s.now())
	if err != nil {
		return 0, err
	}

	return removed, nil
}

// notify pushes the arrival to the recipients that are connected. Failure to notify is not failure
// to deliver: the message is already stored, and the recipient will see it when it next looks.
func (s *Service) notify(message core.Message, recipients []string) int {
	if s.notifier == nil {
		return 0
	}

	params := map[string]any{
		"message_id": message.ID,
		"from":       message.FromAgentID,
		"kind":       message.Kind,
		"topic":      message.Topic,
		"created_at": message.CreatedAt.Format(time.RFC3339Nano),
	}

	notified := 0
	for _, recipient := range recipients {
		notified += s.notifier.Broadcast(recipient, NotificationMethod, params)
	}

	return notified
}

func everyoneBut(agents []core.Agent, senderID string) []string {
	recipients := make([]string, 0, len(agents))
	for _, agent := range agents {
		if agent.ID != senderID {
			recipients = append(recipients, agent.ID)
		}
	}

	return recipients
}

func withoutSender(agentIDs []string, senderID string) []string {
	recipients := make([]string, 0, len(agentIDs))
	for _, agentID := range agentIDs {
		if agentID != senderID {
			recipients = append(recipients, agentID)
		}
	}

	return recipients
}

func destinationOf(message core.Message) string {
	if message.Direct() {
		return message.ToAgentID
	}

	return "topic:" + message.Topic
}
