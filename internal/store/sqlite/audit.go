package sqlite

import (
	"context"
	"fmt"
	"strings"

	"sadeq.uk/lac/internal/core"
)

type auditLog struct{ queries querier }

var _ core.AuditLog = auditLog{}

// defaultAuditLimit caps a listing that does not ask for one, so a long-lived install cannot
// return its entire history by accident.
const defaultAuditLimit = 200

func (l auditLog) Append(ctx context.Context, entry core.AuditEntry) error {
	if entry.Actor == "" || entry.Action == "" {
		return fmt.Errorf("%w: an audit entry needs an actor and an action", core.ErrInvalidArgument)
	}

	_, err := l.queries.ExecContext(ctx,
		`INSERT INTO audit_log (actor, action, target, detail, at) VALUES (?, ?, ?, ?, ?)`,
		entry.Actor, string(entry.Action), entry.Target, entry.Detail, requireMicros(entry.At),
	)

	return translateError("appending to the audit log", err)
}

func (l auditLog) List(ctx context.Context, filter core.AuditFilter) ([]core.AuditEntry, error) {
	query := `SELECT id, actor, action, target, detail, at FROM audit_log`
	conditions := make([]string, 0, 3)
	arguments := make([]any, 0, len(filter.Actions)+2)

	if filter.Actor != "" {
		conditions = append(conditions, `actor = ?`)
		arguments = append(arguments, filter.Actor)
	}
	if len(filter.Actions) > 0 {
		conditions = append(conditions, `action IN (`+placeholders(len(filter.Actions))+`)`)
		for _, action := range filter.Actions {
			arguments = append(arguments, string(action))
		}
	}
	if !filter.Since.IsZero() {
		conditions = append(conditions, `at >= ?`)
		arguments = append(arguments, requireMicros(filter.Since))
	}
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = defaultAuditLimit
	}
	query += ` ORDER BY at DESC, id DESC LIMIT ?`
	arguments = append(arguments, limit)

	rows, err := l.queries.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, translateError("listing the audit log", err)
	}
	defer func() { _ = rows.Close() }()

	entries := make([]core.AuditEntry, 0, 16)
	for rows.Next() {
		var (
			entry  core.AuditEntry
			action string
			at     int64
		)
		if err := rows.Scan(&entry.ID, &entry.Actor, &action, &entry.Target, &entry.Detail, &at); err != nil {
			return nil, translateError("listing the audit log", err)
		}
		entry.Action = core.AuditAction(action)
		entry.At = fromRequiredMicros(at)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, translateError("listing the audit log", err)
	}

	return entries, nil
}
