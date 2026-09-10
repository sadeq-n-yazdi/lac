package core

import "time"

// ReportRequest is the operator asking every live agent what it is doing.
type ReportRequest struct {
	// ID is assigned by the daemon and is what replies are correlated by.
	ID string
	// RequesterID is the agent — usually the operator's CLI — that asked.
	RequesterID string
	// Question is the free-form prompt sent to every agent.
	Question string
	// AskedAgentIDs is who the request went out to, so the collector can name those that stayed silent.
	AskedAgentIDs []string
	// CreatedAt is when the request was broadcast.
	CreatedAt time.Time
	// Deadline is when collection stops waiting for further answers.
	Deadline time.Time
}

// Report is one agent's answer to a report request.
type Report struct {
	// RequestID is the request being answered.
	RequestID string
	// AgentID is the agent answering.
	AgentID string
	// Body is the agent's answer as free-form text.
	Body string
	// CreatedAt is when the answer arrived.
	CreatedAt time.Time
}

// ReportCollection is everything known about one request: the answers, and who never replied.
type ReportCollection struct {
	// Request is the original question.
	Request ReportRequest
	// Reports are the answers received so far, in arrival order.
	Reports []Report
	// SilentAgentIDs are the agents that were asked and have not answered. They are listed
	// explicitly rather than omitted, because "nobody told me" is the interesting case.
	SilentAgentIDs []string
}
