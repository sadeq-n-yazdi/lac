package lacclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The daemon's error codes. Compare with ErrorCode to react to a failure without reading English.
const (
	CodeUnauthorised    = -32000
	CodeNotFound        = -32001
	CodeAlreadyExists   = -32002
	CodeConflict        = -32003
	CodeCapacityReached = -32004
	CodeShuttingDown    = -32005
	CodeInvalidParams   = -32602
)

// ErrorCode returns the daemon's code for an error, or zero if it did not come from the daemon.
func ErrorCode(err error) int {
	var daemonError *Error
	if errors.As(err, &daemonError) {
		return daemonError.Code
	}

	return 0
}

// IsCapacityReached reports whether a non-blocking acquire failed only because the resource was
// full. It is an ordinary answer, not a malfunction.
func IsCapacityReached(err error) bool { return ErrorCode(err) == CodeCapacityReached }

// Agent is an agent as the daemon reports it.
type Agent struct {
	ID              string       `json:"id"`
	Name            string       `json:"name"`
	Kind            string       `json:"kind"`
	Workdir         string       `json:"workdir"`
	ProcessID       int          `json:"pid"`
	State           string       `json:"state"`
	Capabilities    Capabilities `json:"capabilities"`
	RegisteredAt    string       `json:"registered_at"`
	LastHeartbeatAt string       `json:"last_heartbeat_at"`
}

// Capabilities is what an agent is allowed to do.
type Capabilities struct {
	Resources          []string `json:"resources"`
	CanBroadcast       bool     `json:"can_broadcast"`
	CanDefineResources bool     `json:"can_define_resources"`
	CanRequestReports  bool     `json:"can_request_reports"`
}

// Registration is the answer to registering. The token is shown once and never again.
type Registration struct {
	Agent Agent  `json:"agent"`
	Token string `json:"token"`
}

// Register introduces this process to the daemon and authenticates the connection.
func (c *Client) Register(ctx context.Context, name, kind, workdir string, processID int) (Registration, error) {
	var result Registration

	err := c.Call(ctx, "agent.register", map[string]any{
		"name": name, "kind": kind, "workdir": workdir, "pid": processID,
	}, &result)

	return result, err
}

// Authenticate presents an existing token.
func (c *Client) Authenticate(ctx context.Context, token string) (Agent, error) {
	var result struct {
		Agent Agent `json:"agent"`
	}

	err := c.Call(ctx, "agent.authenticate", map[string]any{"token": token}, &result)

	return result.Agent, err
}

// Heartbeat tells the daemon this agent is still here, and returns how long it may stay quiet.
func (c *Client) Heartbeat(ctx context.Context) (time.Duration, error) {
	var result struct {
		NextBySeconds int `json:"next_by_seconds"`
	}

	if err := c.Call(ctx, "agent.heartbeat", nil, &result); err != nil {
		return 0, err
	}

	return time.Duration(result.NextBySeconds) * time.Second, nil
}

// Agents returns the roster. With no states, it returns the agents that are present.
func (c *Client) Agents(ctx context.Context, states ...string) ([]Agent, error) {
	var result struct {
		Agents []Agent `json:"agents"`
	}

	params := map[string]any{}
	if len(states) > 0 {
		params["states"] = states
	}

	err := c.Call(ctx, "agent.list", params, &result)

	return result.Agents, err
}

// Deregister retires this agent, giving back everything it holds.
func (c *Client) Deregister(ctx context.Context) (int, error) {
	var result struct {
		ReleasedLeases int `json:"released_leases"`
	}

	err := c.Call(ctx, "agent.deregister", nil, &result)

	return result.ReleasedLeases, err
}

// Message is a message as the daemon reports it.
type Message struct {
	ID        string          `json:"id"`
	From      string          `json:"from"`
	FromName  string          `json:"from_name"`
	To        string          `json:"to"`
	Topic     string          `json:"topic"`
	Kind      string          `json:"kind"`
	Body      json.RawMessage `json:"body"`
	CreatedAt string          `json:"created_at"`
}

// Sent says where a message went.
type Sent struct {
	MessageID  string `json:"message_id"`
	Recipients int    `json:"recipients"`
	Notified   int    `json:"notified"`
}

// SendTo delivers a message to one agent by name.
func (c *Client) SendTo(ctx context.Context, agentName, kind string, body any) (Sent, error) {
	return c.send(ctx, map[string]any{"to": agentName, "kind": kind}, body)
}

// SendToTopic publishes a message to a topic. The topic "all" reaches every agent.
func (c *Client) SendToTopic(ctx context.Context, topic, kind string, body any) (Sent, error) {
	return c.send(ctx, map[string]any{"topic": topic, "kind": kind}, body)
}

func (c *Client) send(ctx context.Context, params map[string]any, body any) (Sent, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return Sent{}, fmt.Errorf("encoding the message body: %w", err)
	}
	params["body"] = json.RawMessage(encoded)

	var result Sent
	err = c.Call(ctx, "message.send", params, &result)

	return result, err
}

// Inbox returns the messages this agent has not acknowledged. Reading is not acknowledging: a
// message comes back until Acknowledge is called for it.
func (c *Client) Inbox(ctx context.Context, limit int) ([]Message, error) {
	var result struct {
		Messages []Message `json:"messages"`
	}

	params := map[string]any{}
	if limit > 0 {
		params["limit"] = limit
	}

	err := c.Call(ctx, "message.inbox", params, &result)

	return result.Messages, err
}

// Acknowledge confirms messages have been dealt with, and reports how many were cleared.
func (c *Client) Acknowledge(ctx context.Context, messageIDs ...string) (int, error) {
	var result struct {
		Acknowledged int `json:"acknowledged"`
	}

	err := c.Call(ctx, "message.ack", map[string]any{"message_ids": messageIDs}, &result)

	return result.Acknowledged, err
}

// Subscribe joins a topic.
func (c *Client) Subscribe(ctx context.Context, topic string) error {
	return c.Call(ctx, "message.subscribe", map[string]any{"topic": topic}, nil)
}

// Unsubscribe leaves a topic.
func (c *Client) Unsubscribe(ctx context.Context, topic string) error {
	return c.Call(ctx, "message.unsubscribe", map[string]any{"topic": topic}, nil)
}

// Resource is a resource and how busy it is.
type Resource struct {
	Name            string `json:"name"`
	Capacity        int    `json:"capacity"`
	Held            int    `json:"held"`
	Free            int    `json:"free"`
	Waiting         int    `json:"waiting"`
	LeaseTimeToLive string `json:"lease_time_to_live"`
	Description     string `json:"description"`
}

// DefineResource creates or reconfigures a resource. It needs the operator capability.
func (c *Client) DefineResource(
	ctx context.Context, name string, capacity int, timeToLive time.Duration, description string,
) (Resource, error) {
	params := map[string]any{"name": name, "capacity": capacity, "description": description}
	if timeToLive > 0 {
		params["lease_time_to_live"] = timeToLive.String()
	}

	var result struct {
		Resource Resource `json:"resource"`
	}

	err := c.Call(ctx, "resource.define", params, &result)

	return result.Resource, err
}

// Resources lists every resource on this machine.
func (c *Client) Resources(ctx context.Context) ([]Resource, error) {
	var result struct {
		Resources []Resource `json:"resources"`
	}

	err := c.Call(ctx, "resource.list", nil, &result)

	return result.Resources, err
}

// ResourceStatus describes one resource.
func (c *Client) ResourceStatus(ctx context.Context, name string) (Resource, error) {
	var result struct {
		Resource Resource `json:"resource"`
	}

	err := c.Call(ctx, "resource.status", map[string]any{"resource": name}, &result)

	return result.Resource, err
}

// Lease is a granted slot.
type Lease struct {
	ID                string `json:"id"`
	Resource          string `json:"resource"`
	AgentID           string `json:"agent_id"`
	AcquiredAt        string `json:"acquired_at"`
	ExpiresAt         string `json:"expires_at"`
	RenewAfterSeconds int    `json:"renew_after_seconds"`
}

// RenewAfter is how long the holder may wait before renewing without risking its slot.
func (l Lease) RenewAfter() time.Duration {
	return time.Duration(l.RenewAfterSeconds) * time.Second
}

// AcquireRequest asks for a slot on a resource.
type AcquireRequest struct {
	// Resource is what to queue for.
	Resource string
	// Priority orders this request against the others waiting. Higher goes first.
	Priority int
	// Reason is shown in the queue so the operator can see who is waiting for what.
	Reason string
	// Metadata is free-form JSON attached to the lease, such as the command about to run.
	Metadata any
	// NoWait returns immediately with a capacity error rather than queueing.
	NoWait bool
	// Timeout gives up after this long. Zero waits for as long as the caller's context allows.
	Timeout time.Duration
}

// Acquire waits for a slot and returns the lease that holds it.
//
// This is the call an agent makes before doing anything heavy. It blocks — that is the point —
// until the slot is granted, the context ends, or the timeout expires.
func (c *Client) Acquire(ctx context.Context, request AcquireRequest) (Lease, error) {
	params := map[string]any{"resource": request.Resource}
	if request.Priority != 0 {
		params["priority"] = request.Priority
	}
	if request.Reason != "" {
		params["reason"] = request.Reason
	}
	if request.NoWait {
		params["no_wait"] = true
	}
	if request.Timeout > 0 {
		params["timeout"] = request.Timeout.String()
	}
	if request.Metadata != nil {
		encoded, err := json.Marshal(request.Metadata)
		if err != nil {
			return Lease{}, fmt.Errorf("encoding the lease metadata: %w", err)
		}
		params["metadata"] = json.RawMessage(encoded)
	}

	var result struct {
		Lease Lease `json:"lease"`
	}

	err := c.Call(ctx, "lease.acquire", params, &result)

	return result.Lease, err
}

// Renew extends a slot this agent holds.
func (c *Client) Renew(ctx context.Context, leaseID string) (Lease, error) {
	var result struct {
		Lease Lease `json:"lease"`
	}

	err := c.Call(ctx, "lease.renew", map[string]any{"lease_id": leaseID}, &result)

	return result.Lease, err
}

// Release gives a slot back.
func (c *Client) Release(ctx context.Context, leaseID string) error {
	return c.Call(ctx, "lease.release", map[string]any{"lease_id": leaseID}, nil)
}

// Held returns the slots this agent currently holds.
func (c *Client) Held(ctx context.Context) ([]Lease, error) {
	var result struct {
		Leases []Lease `json:"leases"`
	}

	err := c.Call(ctx, "lease.held", nil, &result)

	return result.Leases, err
}

// QueueEntry is one agent's place in line.
type QueueEntry struct {
	ID          string `json:"id"`
	Resource    string `json:"resource"`
	AgentID     string `json:"agent_id"`
	AgentName   string `json:"agent_name"`
	Priority    int    `json:"priority"`
	Reason      string `json:"reason"`
	RequestedAt string `json:"requested_at"`
	Position    int    `json:"position"`
}

// QueueStatus is a resource and who is waiting for it.
type QueueStatus struct {
	Resource Resource     `json:"resource"`
	Waiting  []QueueEntry `json:"waiting"`
}

// Queue returns who is waiting for a resource, in the order they will be served.
func (c *Client) Queue(ctx context.Context, resource string) (QueueStatus, error) {
	var result QueueStatus

	err := c.Call(ctx, "queue.status", map[string]any{"resource": resource}, &result)

	return result, err
}

// CancelQueueEntry withdraws a request from a queue.
func (c *Client) CancelQueueEntry(ctx context.Context, entryID string) error {
	return c.Call(ctx, "queue.cancel", map[string]any{"entry_id": entryID}, nil)
}

// DaemonInfo describes the daemon on the other end.
type DaemonInfo struct {
	Version       string   `json:"version"`
	Commit        string   `json:"commit"`
	Protocol      string   `json:"protocol"`
	StartedAt     string   `json:"started_at"`
	UptimeSeconds int64    `json:"uptime_seconds"`
	Methods       []string `json:"methods"`
}

// Info asks the daemon what it is and what it can do.
func (c *Client) Info(ctx context.Context) (DaemonInfo, error) {
	var result DaemonInfo

	err := c.Call(ctx, "daemon.info", nil, &result)

	return result, err
}

// Ping checks the daemon is answering.
func (c *Client) Ping(ctx context.Context) error {
	return c.Call(ctx, "daemon.ping", nil, nil)
}

// Report is one agent's answer to a report request.
type Report struct {
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// ReportCollection is everything known about a report request: the answers, and who stayed silent.
type ReportCollection struct {
	RequestID string   `json:"request_id"`
	Question  string   `json:"question"`
	Asked     int      `json:"asked"`
	Reports   []Report `json:"reports"`
	// Silent names the agents that were asked and did not answer.
	Silent   []string `json:"silent"`
	Complete bool     `json:"complete"`
}

// RequestReports asks every agent what it is doing.
//
// With wait set, the call returns once everyone has answered or the deadline passes; without it,
// it returns immediately and the caller collects later with CollectReports.
func (c *Client) RequestReports(
	ctx context.Context, question string, deadline time.Duration, wait bool,
) (ReportCollection, error) {
	params := map[string]any{"question": question, "wait": wait}
	if deadline > 0 {
		params["deadline"] = deadline.String()
	}

	var result ReportCollection
	err := c.Call(ctx, "report.request", params, &result)

	return result, err
}

// SubmitReport answers a report request. The request id comes from the message the daemon sent.
func (c *Client) SubmitReport(ctx context.Context, requestID, body string) error {
	return c.Call(ctx, "report.submit", map[string]any{"request_id": requestID, "body": body}, nil)
}

// CollectReports reads the answers to a request, optionally waiting for the stragglers.
func (c *Client) CollectReports(ctx context.Context, requestID string, wait bool) (ReportCollection, error) {
	var result ReportCollection

	err := c.Call(ctx, "report.collect", map[string]any{"request_id": requestID, "wait": wait}, &result)

	return result, err
}
