package core

import (
	"fmt"
	"time"
)

// BroadcastTopic is the topic every registered agent is implicitly subscribed to.
const BroadcastTopic = "all"

// Message is one item an agent sent, either to a single recipient or to a topic.
type Message struct {
	// ID is assigned by the daemon.
	ID string
	// FromAgentID is the sender. It is set by the daemon from the authenticated connection, never
	// from the request body, so a message cannot be forged.
	FromAgentID string
	// ToAgentID is the single recipient, or empty when Topic is set.
	ToAgentID string
	// Topic is the topic the message was published to, or empty for a direct message.
	Topic string
	// Kind is a free-form label the recipient can route on, such as "question" or "status".
	Kind string
	// Body is the payload. It is JSON as produced by the transport; the domain does not interpret it.
	Body []byte
	// CreatedAt is when the daemon accepted the message.
	CreatedAt time.Time
	// ExpiresAt is when an undelivered message may be discarded. Zero means it never expires.
	ExpiresAt time.Time
}

// Direct reports whether the message is addressed to a single agent.
func (m Message) Direct() bool { return m.ToAgentID != "" }

// Validate reports whether the message is well formed enough to store.
func (m Message) Validate() error {
	if m.ID == "" {
		return fmt.Errorf("%w: message id is required", ErrInvalidArgument)
	}
	if m.FromAgentID == "" {
		return fmt.Errorf("%w: message sender is required", ErrInvalidArgument)
	}
	if (m.ToAgentID == "") == (m.Topic == "") {
		return fmt.Errorf("%w: a message needs exactly one of a recipient or a topic", ErrInvalidArgument)
	}
	if m.Topic != "" {
		if err := ValidateName("topic", m.Topic); err != nil {
			return err
		}
	}
	if err := ValidateName("message kind", m.Kind); err != nil {
		return err
	}
	if len(m.Body) == 0 {
		return fmt.Errorf("%w: message body is required", ErrInvalidArgument)
	}
	return nil
}

// Delivery is one recipient's copy of a message. A message is retained until every delivery is
// acknowledged, so an agent that restarts still receives what it missed.
type Delivery struct {
	// MessageID identifies the message being delivered.
	MessageID string
	// AgentID is the recipient.
	AgentID string
	// DeliveredAt is when the recipient first read it. Zero means it has not been read.
	DeliveredAt time.Time
	// AckedAt is when the recipient acknowledged it. Zero means it is still outstanding.
	AckedAt time.Time
}

// Acknowledged reports whether this delivery is complete and may be pruned.
func (d Delivery) Acknowledged() bool { return !d.AckedAt.IsZero() }
