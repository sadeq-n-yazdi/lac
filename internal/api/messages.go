package api

import (
	"context"
	"encoding/json"
	"fmt"

	"code.sadeq.uk/lac/internal/auth"
	"code.sadeq.uk/lac/internal/core"
	"code.sadeq.uk/lac/internal/service/messaging"
	"code.sadeq.uk/lac/internal/transport/jsonrpc"
)

// SendParams is one message. Exactly one of To and Topic is set.
type SendParams struct {
	To    string          `json:"to,omitempty"`
	Topic string          `json:"topic,omitempty"`
	Kind  string          `json:"kind"`
	Body  json.RawMessage `json:"body"`
}

// SendResult says where the message went.
type SendResult struct {
	MessageID  string `json:"message_id"`
	Recipients int    `json:"recipients"`
	Notified   int    `json:"notified"`
}

func (a *API) handleSend(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments SendParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	if (arguments.To == "") == (arguments.Topic == "") {
		return nil, fmt.Errorf("%w: give either a recipient or a topic, not both", core.ErrInvalidArgument)
	}

	// Reaching more than one agent at a time is a capability, because a broadcast is how one
	// confused agent interrupts every other one on the machine.
	if arguments.Topic != "" {
		if err := a.authenticator.Authorise(ctx, caller, auth.PermissionBroadcast, arguments.Topic); err != nil {
			return nil, err
		}
	}

	sent, err := a.messaging.Send(ctx, messaging.SendRequest{
		FromAgentID: caller.ID,
		ToAgentName: arguments.To,
		Topic:       arguments.Topic,
		Kind:        arguments.Kind,
		Body:        arguments.Body,
	})
	if err != nil {
		return nil, err
	}

	return SendResult{
		MessageID:  sent.Message.ID,
		Recipients: len(sent.RecipientIDs),
		Notified:   sent.Notified,
	}, nil
}

// InboxParams reads an agent's own messages.
type InboxParams struct {
	// Limit caps how many are returned. Zero means the daemon's default.
	Limit int `json:"limit,omitempty"`
}

// InboxResult is what is waiting. Messages stay here until they are acknowledged.
type InboxResult struct {
	Messages []MessageView `json:"messages"`
}

func (a *API) handleInbox(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments InboxParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	// The inbox read is always the caller's own: an agent id in the request would be a claim, and
	// honouring it would let one agent read another's messages.
	messages, err := a.messaging.Inbox(ctx, caller.ID, arguments.Limit)
	if err != nil {
		return nil, err
	}

	views := make([]MessageView, 0, len(messages))
	for _, message := range messages {
		views = append(views, viewOfMessage(message, a.nameOf(ctx, message.FromAgentID)))
	}

	return InboxResult{Messages: views}, nil
}

// AcknowledgeParams confirms messages have been dealt with.
type AcknowledgeParams struct {
	MessageIDs []string `json:"message_ids"`
}

// AcknowledgeResult reports how many were actually cleared. A count lower than what was asked for
// means some of those messages were not this agent's to clear.
type AcknowledgeResult struct {
	Acknowledged int `json:"acknowledged"`
}

func (a *API) handleAcknowledge(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments AcknowledgeParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}
	if len(arguments.MessageIDs) == 0 {
		return nil, fmt.Errorf("%w: give at least one message id", core.ErrInvalidArgument)
	}

	acknowledged, err := a.messaging.Acknowledge(ctx, caller.ID, arguments.MessageIDs)
	if err != nil {
		return nil, err
	}

	return AcknowledgeResult{Acknowledged: acknowledged}, nil
}

// TopicParams names a topic.
type TopicParams struct {
	Topic string `json:"topic"`
}

// TopicResult confirms the change.
type TopicResult struct {
	Topic string `json:"topic"`
}

func (a *API) handleSubscribe(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments TopicParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	if err := a.messaging.Subscribe(ctx, caller.ID, arguments.Topic); err != nil {
		return nil, err
	}

	return TopicResult(arguments), nil
}

func (a *API) handleUnsubscribe(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments TopicParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	if err := a.messaging.Unsubscribe(ctx, caller.ID, arguments.Topic); err != nil {
		return nil, err
	}

	return TopicResult(arguments), nil
}

// nameOf resolves an agent id to its name for display. A message from an agent that has since gone
// is still worth showing, so a failed lookup is not an error here.
func (a *API) nameOf(ctx context.Context, agentID string) string {
	agent, err := a.registry.ByID(ctx, agentID)
	if err != nil {
		return ""
	}

	return agent.Name
}
