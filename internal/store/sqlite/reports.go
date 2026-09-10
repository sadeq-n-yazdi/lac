package sqlite

import (
	"context"
	"encoding/json"
	"fmt"

	"code.sadeq.uk/lac/internal/core"
)

type reportRepository struct{ queries querier }

var _ core.ReportRepository = reportRepository{}

func (r reportRepository) CreateRequest(ctx context.Context, request core.ReportRequest) error {
	if request.ID == "" || request.RequesterID == "" {
		return fmt.Errorf("%w: a report request needs an id and a requester", core.ErrInvalidArgument)
	}

	asked, err := json.Marshal(request.AskedAgentIDs)
	if err != nil {
		return fmt.Errorf("encoding the asked agents: %w", err)
	}

	_, err = r.queries.ExecContext(ctx, `
		INSERT INTO report_requests (id, requester_id, question, asked_agent_ids, created_at, deadline)
		VALUES (?, ?, ?, ?, ?, ?)`,
		request.ID, request.RequesterID, request.Question, string(asked),
		requireMicros(request.CreatedAt), requireMicros(request.Deadline),
	)

	return translateError("storing a report request", err)
}

func (r reportRepository) RequestByID(ctx context.Context, requestID string) (core.ReportRequest, error) {
	var (
		request   core.ReportRequest
		asked     string
		createdAt int64
		deadline  int64
	)

	err := r.queries.QueryRowContext(ctx, `
		SELECT id, requester_id, question, asked_agent_ids, created_at, deadline
		  FROM report_requests WHERE id = ?`, requestID,
	).Scan(&request.ID, &request.RequesterID, &request.Question, &asked, &createdAt, &deadline)
	if err != nil {
		return core.ReportRequest{}, translateError("reading report request "+requestID, err)
	}

	if err := json.Unmarshal([]byte(asked), &request.AskedAgentIDs); err != nil {
		return core.ReportRequest{}, fmt.Errorf("decoding the asked agents of %s: %w", requestID, err)
	}

	request.CreatedAt = fromRequiredMicros(createdAt)
	request.Deadline = fromRequiredMicros(deadline)

	return request, nil
}

// AddReport stores an answer, replacing any earlier answer from the same agent: an agent that
// corrects itself should not appear twice in the operator's summary.
func (r reportRepository) AddReport(ctx context.Context, report core.Report) error {
	_, err := r.queries.ExecContext(ctx, `
		INSERT INTO reports (request_id, agent_id, body, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (request_id, agent_id) DO UPDATE
		   SET body = excluded.body, created_at = excluded.created_at`,
		report.RequestID, report.AgentID, report.Body, requireMicros(report.CreatedAt),
	)

	return translateError("storing a report from "+report.AgentID, err)
}

func (r reportRepository) ReportsFor(ctx context.Context, requestID string) ([]core.Report, error) {
	rows, err := r.queries.QueryContext(ctx, `
		SELECT request_id, agent_id, body, created_at
		  FROM reports WHERE request_id = ? ORDER BY created_at, agent_id`, requestID)
	if err != nil {
		return nil, translateError("reading the reports for "+requestID, err)
	}
	defer func() { _ = rows.Close() }()

	reports := make([]core.Report, 0, 8)
	for rows.Next() {
		var (
			report    core.Report
			createdAt int64
		)
		if err := rows.Scan(&report.RequestID, &report.AgentID, &report.Body, &createdAt); err != nil {
			return nil, translateError("reading the reports for "+requestID, err)
		}
		report.CreatedAt = fromRequiredMicros(createdAt)
		reports = append(reports, report)
	}
	if err := rows.Err(); err != nil {
		return nil, translateError("reading the reports for "+requestID, err)
	}

	return reports, nil
}
