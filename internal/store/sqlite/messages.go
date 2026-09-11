package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"sadeq.uk/lac/internal/core"
)

type messageRepository struct{ queries querier }

var _ core.MessageRepository = messageRepository{}

const messageColumns = `id, from_agent_id, to_agent_id, topic, kind, body, created_at, expires_at`

// defaultInboxLimit caps a Pending call that does not ask for a limit, so one very behind agent
// cannot pull its entire history into memory in a single call.
const defaultInboxLimit = 100

// defaultLogLimit caps a log listing that does not ask for one. The log is read by a person, and a
// screenful of the most recent traffic is what they usually want.
const defaultLogLimit = 50

// Append stores the message and one pending delivery per recipient. The caller is expected to be
// inside a transaction; the message and its deliveries must never exist without each other.
func (r messageRepository) Append(ctx context.Context, message core.Message, recipientIDs []string) error {
	if err := message.Validate(); err != nil {
		return err
	}
	if len(recipientIDs) == 0 {
		return fmt.Errorf("%w: a message needs at least one recipient", core.ErrInvalidArgument)
	}

	if _, err := r.queries.ExecContext(ctx, `
		INSERT INTO messages (`+messageColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		message.ID, message.FromAgentID, nullString(message.ToAgentID), nullString(message.Topic),
		message.Kind, message.Body, requireMicros(message.CreatedAt), toMicros(message.ExpiresAt),
	); err != nil {
		return translateError("storing a message", err)
	}

	for _, recipientID := range recipientIDs {
		if _, err := r.queries.ExecContext(ctx,
			`INSERT OR IGNORE INTO deliveries (message_id, agent_id) VALUES (?, ?)`,
			message.ID, recipientID,
		); err != nil {
			return translateError("storing a delivery", err)
		}
	}

	return nil
}

// Pending returns the messages this agent has not acknowledged, oldest first.
func (r messageRepository) Pending(ctx context.Context, agentID string, limit int) ([]core.Message, error) {
	if limit <= 0 {
		limit = defaultInboxLimit
	}

	rows, err := r.queries.QueryContext(ctx, `
		SELECT `+prefixed("m", messageColumns)+`
		  FROM messages m
		  JOIN deliveries d ON d.message_id = m.id
		 WHERE d.agent_id = ? AND d.acked_at IS NULL
		 ORDER BY m.created_at, m.id
		 LIMIT ?`,
		agentID, limit,
	)
	if err != nil {
		return nil, translateError("reading the inbox of "+agentID, err)
	}
	defer func() { _ = rows.Close() }()

	messages := make([]core.Message, 0, 8)
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return nil, translateError("reading the inbox of "+agentID, err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, translateError("reading the inbox of "+agentID, err)
	}

	return messages, nil
}

func (r messageRepository) MarkDelivered(
	ctx context.Context, agentID string, messageIDs []string, at time.Time,
) error {
	if len(messageIDs) == 0 {
		return nil
	}

	arguments := make([]any, 0, len(messageIDs)+2)
	arguments = append(arguments, requireMicros(at), agentID)
	for _, messageID := range messageIDs {
		arguments = append(arguments, messageID)
	}

	_, err := r.queries.ExecContext(ctx, `
		UPDATE deliveries
		   SET delivered_at = ?
		 WHERE agent_id = ? AND delivered_at IS NULL AND message_id IN (`+placeholders(len(messageIDs))+`)`,
		arguments...,
	)

	return translateError("marking messages delivered for "+agentID, err)
}

// Acknowledge completes deliveries and reports how many it changed. The agent id is part of the
// statement, so acknowledging another agent's message changes nothing.
func (r messageRepository) Acknowledge(
	ctx context.Context, agentID string, messageIDs []string, at time.Time,
) (int, error) {
	if len(messageIDs) == 0 {
		return 0, nil
	}

	arguments := make([]any, 0, len(messageIDs)+3)
	arguments = append(arguments, requireMicros(at), requireMicros(at), agentID)
	for _, messageID := range messageIDs {
		arguments = append(arguments, messageID)
	}

	result, err := r.queries.ExecContext(ctx, `
		UPDATE deliveries
		   SET acked_at = ?, delivered_at = COALESCE(delivered_at, ?)
		 WHERE agent_id = ? AND acked_at IS NULL AND message_id IN (`+placeholders(len(messageIDs))+`)`,
		arguments...,
	)
	if err != nil {
		return 0, translateError("acknowledging messages for "+agentID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("acknowledging messages for %s: reading the affected row count: %w", agentID, err)
	}

	return int(affected), nil
}

// List returns the traffic between agents, most recent first, with how far each message got.
//
// Unlike Pending, this is not scoped to one recipient: it is the operator's view, and the caller is
// responsible for having checked that the caller may see it.
func (r messageRepository) List(ctx context.Context, filter core.MessageFilter) ([]core.MessageRecord, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultLogLimit
	}

	query := `
		SELECT ` + prefixed("m", messageColumns) + `,
		       COUNT(d.delivered_at),
		       COUNT(d.acked_at),
		       COALESCE(GROUP_CONCAT(d.agent_id, char(31)), '')
		  FROM messages m
		  LEFT JOIN deliveries d ON d.message_id = m.id
		 WHERE 1 = 1`
	arguments := make([]any, 0, 4)

	if filter.AgentID != "" {
		// Either half of the conversation: what this agent said, and what was said to it.
		query += ` AND (m.from_agent_id = ? OR EXISTS (
		        SELECT 1 FROM deliveries dd WHERE dd.message_id = m.id AND dd.agent_id = ?))`
		arguments = append(arguments, filter.AgentID, filter.AgentID)
	}
	if filter.Topic != "" {
		query += ` AND m.topic = ?`
		arguments = append(arguments, filter.Topic)
	}
	if !filter.Since.IsZero() {
		query += ` AND m.created_at >= ?`
		arguments = append(arguments, requireMicros(filter.Since))
	}

	query += ` GROUP BY m.id ORDER BY m.created_at DESC, m.id DESC LIMIT ?`
	arguments = append(arguments, limit)

	rows, err := r.queries.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, translateError("reading the message log", err)
	}
	defer func() { _ = rows.Close() }()

	records := make([]core.MessageRecord, 0, 16)
	for rows.Next() {
		record, err := scanMessageRecord(rows)
		if err != nil {
			return nil, translateError("reading the message log", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, translateError("reading the message log", err)
	}

	return records, nil
}

func scanMessageRecord(rows *sql.Rows) (core.MessageRecord, error) {
	var (
		record    core.MessageRecord
		toAgentID sql.NullString
		topic     sql.NullString
		createdAt int64
		expiresAt sql.NullInt64
		delivered int
		acked     int
		agentIDs  string
	)

	if err := rows.Scan(&record.Message.ID, &record.Message.FromAgentID, &toAgentID, &topic,
		&record.Message.Kind, &record.Message.Body, &createdAt, &expiresAt,
		&delivered, &acked, &agentIDs); err != nil {
		return core.MessageRecord{}, err
	}

	record.Message.ToAgentID = toAgentID.String
	record.Message.Topic = topic.String
	record.Message.CreatedAt = fromRequiredMicros(createdAt)
	record.Message.ExpiresAt = fromMicros(expiresAt)
	record.DeliveredCount = delivered
	record.AcknowledgedCount = acked

	if agentIDs != "" {
		// Unit separator rather than a comma: an agent id never contains one, so this cannot be
		// confused by an id that does.
		record.RecipientIDs = strings.Split(agentIDs, "\x1f")
	}

	return record, nil
}

// PruneExpired removes messages nobody is waiting for any more: those that passed their expiry, and
// those every recipient acknowledged longer ago than the retention period.
//
// Acknowledged messages are kept for that period rather than deleted on the spot, so the operator's
// log still shows the conversations that were handled properly. A retention of zero keeps the old
// behaviour of deleting them as soon as they are acknowledged.
func (r messageRepository) PruneExpired(
	ctx context.Context, at time.Time, retention time.Duration,
) (int, error) {
	result, err := r.queries.ExecContext(ctx, `
		DELETE FROM messages
		 WHERE (expires_at IS NOT NULL AND expires_at <= ?)
		    OR (created_at <= ? AND NOT EXISTS (
		        SELECT 1 FROM deliveries d WHERE d.message_id = messages.id AND d.acked_at IS NULL
		    ))`,
		requireMicros(at), requireMicros(at.Add(-retention)),
	)
	if err != nil {
		return 0, translateError("pruning messages", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("pruning messages: reading the affected row count: %w", err)
	}

	return int(affected), nil
}

func (r messageRepository) Subscribe(ctx context.Context, agentID, topic string) error {
	if err := core.ValidateName("topic", topic); err != nil {
		return err
	}

	_, err := r.queries.ExecContext(ctx,
		`INSERT OR IGNORE INTO subscriptions (agent_id, topic, subscribed_at) VALUES (?, ?, ?)`,
		agentID, topic, requireMicros(time.Now()),
	)

	return translateError("subscribing "+agentID+" to "+topic, err)
}

func (r messageRepository) Unsubscribe(ctx context.Context, agentID, topic string) error {
	_, err := r.queries.ExecContext(ctx,
		`DELETE FROM subscriptions WHERE agent_id = ? AND topic = ?`, agentID, topic)

	return translateError("unsubscribing "+agentID+" from "+topic, err)
}

func (r messageRepository) Subscribers(ctx context.Context, topic string) ([]string, error) {
	rows, err := r.queries.QueryContext(ctx,
		`SELECT agent_id FROM subscriptions WHERE topic = ? ORDER BY subscribed_at, agent_id`, topic)
	if err != nil {
		return nil, translateError("listing the subscribers of "+topic, err)
	}
	defer func() { _ = rows.Close() }()

	subscribers := make([]string, 0, 8)
	for rows.Next() {
		var agentID string
		if err := rows.Scan(&agentID); err != nil {
			return nil, translateError("listing the subscribers of "+topic, err)
		}
		subscribers = append(subscribers, agentID)
	}
	if err := rows.Err(); err != nil {
		return nil, translateError("listing the subscribers of "+topic, err)
	}

	return subscribers, nil
}

func scanMessage(source scanner) (core.Message, error) {
	var (
		message   core.Message
		recipient sql.NullString
		topic     sql.NullString
		createdAt int64
		expiresAt sql.NullInt64
	)

	if err := source.Scan(&message.ID, &message.FromAgentID, &recipient, &topic,
		&message.Kind, &message.Body, &createdAt, &expiresAt); err != nil {
		return core.Message{}, err
	}

	message.ToAgentID = fromNullString(recipient)
	message.Topic = fromNullString(topic)
	message.CreatedAt = fromRequiredMicros(createdAt)
	message.ExpiresAt = fromMicros(expiresAt)

	return message, nil
}
