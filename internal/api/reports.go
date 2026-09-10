package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"code.sadeq.uk/lac/internal/auth"
	"code.sadeq.uk/lac/internal/core"
	"code.sadeq.uk/lac/internal/transport/jsonrpc"
)

// ReportRequestParams asks every agent what it is doing.
type ReportRequestParams struct {
	// Question is what to ask, in plain language.
	Question string `json:"question"`
	// Deadline is how long to wait for answers, such as "30s". Empty uses the default.
	Deadline string `json:"deadline,omitempty"`
	// Wait collects the answers before returning. Without it the call returns the request id and
	// the caller collects separately, which is what a bridge such as Telegram wants.
	Wait bool `json:"wait,omitempty"`
}

// ReportView is one agent's answer.
type ReportView struct {
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name,omitempty"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// ReportCollectionResult is everything known about a request.
type ReportCollectionResult struct {
	RequestID string       `json:"request_id"`
	Question  string       `json:"question"`
	Asked     int          `json:"asked"`
	Reports   []ReportView `json:"reports"`
	// Silent names the agents that were asked and did not answer. They are listed rather than
	// omitted, because an agent that has stopped answering is exactly what an operator wants to see.
	Silent   []string `json:"silent"`
	Complete bool     `json:"complete"`
}

func (a *API) handleReportRequest(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments ReportRequestParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	// Asking everyone to stop and report is an operator's power, not something an agent should be
	// able to do to the rest of the machine.
	if err := a.authenticator.Authorise(ctx, caller, auth.PermissionRequestReports, ""); err != nil {
		return nil, err
	}

	deadline, err := parseDuration(arguments.Deadline)
	if err != nil {
		return nil, err
	}

	request, err := a.reporting.Request(ctx, caller.ID, arguments.Question, deadline)
	if err != nil {
		return nil, err
	}

	if !arguments.Wait {
		collection, err := a.reporting.Snapshot(ctx, request.ID)
		if err != nil {
			return nil, err
		}

		return a.viewOfCollection(ctx, collection), nil
	}

	collection, err := a.reporting.Collect(ctx, request.ID)
	if err != nil {
		return nil, err
	}

	return a.viewOfCollection(ctx, collection), nil
}

// ReportSubmitParams is one agent answering.
type ReportSubmitParams struct {
	RequestID string `json:"request_id"`
	Body      string `json:"body"`
}

// ReportSubmitResult confirms the answer was recorded.
type ReportSubmitResult struct {
	Recorded bool `json:"recorded"`
}

func (a *API) handleReportSubmit(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments ReportSubmitParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}
	if arguments.RequestID == "" {
		return nil, fmt.Errorf("%w: which request? give request_id", core.ErrInvalidArgument)
	}

	if err := a.reporting.Submit(ctx, arguments.RequestID, caller.ID, arguments.Body); err != nil {
		return nil, err
	}

	return ReportSubmitResult{Recorded: true}, nil
}

// ReportCollectParams reads the answers to a request.
type ReportCollectParams struct {
	RequestID string `json:"request_id"`
	// Wait blocks until everyone has answered or the deadline passes.
	Wait bool `json:"wait,omitempty"`
}

func (a *API) handleReportCollect(
	ctx context.Context, _ core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments ReportCollectParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}
	if arguments.RequestID == "" {
		return nil, fmt.Errorf("%w: which request? give request_id", core.ErrInvalidArgument)
	}

	collect := a.reporting.Snapshot
	if arguments.Wait {
		collect = a.reporting.Collect
	}

	collection, err := collect(ctx, arguments.RequestID)
	if err != nil {
		return nil, err
	}

	return a.viewOfCollection(ctx, collection), nil
}

// viewOfCollection turns agent ids into names, because an operator reading a report wants to know
// which session went quiet, not which identifier did.
func (a *API) viewOfCollection(ctx context.Context, collection core.ReportCollection) ReportCollectionResult {
	reports := make([]ReportView, 0, len(collection.Reports))
	for _, report := range collection.Reports {
		reports = append(reports, ReportView{
			AgentID:   report.AgentID,
			AgentName: a.nameOf(ctx, report.AgentID),
			Body:      report.Body,
			CreatedAt: report.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}

	silent := make([]string, 0, len(collection.SilentAgentIDs))
	for _, agentID := range collection.SilentAgentIDs {
		name := a.nameOf(ctx, agentID)
		if name == "" {
			name = agentID
		}
		silent = append(silent, name)
	}

	return ReportCollectionResult{
		RequestID: collection.Request.ID,
		Question:  collection.Request.Question,
		Asked:     len(collection.Request.AskedAgentIDs),
		Reports:   reports,
		Silent:    silent,
		Complete:  len(silent) == 0,
	}
}
