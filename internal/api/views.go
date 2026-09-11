// Package api exposes LAC's services as JSON-RPC methods.
//
// It is the only place that knows about both the wire and the services, and it holds no business
// rules of its own: it authenticates the caller, checks what they are allowed to do, translates
// between wire shapes and domain types, and hands off.
package api

import (
	"encoding/json"
	"time"

	"sadeq.uk/lac/internal/core"
)

// The wire types are defined here rather than reusing the domain types directly, so that renaming
// a field in the domain does not silently break every client, and so that nothing internal — a
// token hash, say — can ever be serialised by accident.

// AgentView is an agent as clients see it.
type AgentView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Workdir   string `json:"workdir"`
	ProcessID int    `json:"pid"`
	// Internal marks one of the daemon's own components rather than somebody's session.
	Internal        bool             `json:"internal,omitempty"`
	State           string           `json:"state"`
	Capabilities    CapabilitiesView `json:"capabilities"`
	RegisteredAt    string           `json:"registered_at"`
	LastHeartbeatAt string           `json:"last_heartbeat_at"`
}

// CapabilitiesView is what an agent is allowed to do.
type CapabilitiesView struct {
	Resources          []string `json:"resources"`
	CanBroadcast       bool     `json:"can_broadcast"`
	CanDefineResources bool     `json:"can_define_resources"`
	CanRequestReports  bool     `json:"can_request_reports"`
}

func viewOfAgent(agent core.Agent) AgentView {
	resources := agent.Capabilities.Resources
	if resources == nil {
		resources = []string{}
	}

	return AgentView{
		ID:        agent.ID,
		Name:      agent.Name,
		Kind:      agent.Kind,
		Workdir:   agent.Workdir,
		ProcessID: agent.ProcessID,
		Internal:  agent.Internal,
		State:     string(agent.State),
		Capabilities: CapabilitiesView{
			Resources:          resources,
			CanBroadcast:       agent.Capabilities.CanBroadcast,
			CanDefineResources: agent.Capabilities.CanDefineResources,
			CanRequestReports:  agent.Capabilities.CanRequestReports,
		},
		RegisteredAt:    formatTime(agent.RegisteredAt),
		LastHeartbeatAt: formatTime(agent.LastHeartbeatAt),
	}
}

func viewsOfAgents(agents []core.Agent) []AgentView {
	views := make([]AgentView, 0, len(agents))
	for _, agent := range agents {
		views = append(views, viewOfAgent(agent))
	}

	return views
}

// MessageView is a message as clients see it.
type MessageView struct {
	ID        string          `json:"id"`
	From      string          `json:"from"`
	FromName  string          `json:"from_name,omitempty"`
	To        string          `json:"to,omitempty"`
	Topic     string          `json:"topic,omitempty"`
	Kind      string          `json:"kind"`
	Body      json.RawMessage `json:"body"`
	CreatedAt string          `json:"created_at"`
}

func viewOfMessage(message core.Message, senderName string) MessageView {
	return MessageView{
		ID:        message.ID,
		From:      message.FromAgentID,
		FromName:  senderName,
		To:        message.ToAgentID,
		Topic:     message.Topic,
		Kind:      message.Kind,
		Body:      json.RawMessage(message.Body),
		CreatedAt: formatTime(message.CreatedAt),
	}
}

// ResourceView is a resource and how busy it is.
type ResourceView struct {
	Name            string `json:"name"`
	Capacity        int    `json:"capacity"`
	Held            int    `json:"held"`
	Free            int    `json:"free"`
	Waiting         int    `json:"waiting"`
	LeaseTimeToLive string `json:"lease_time_to_live"`
	Description     string `json:"description,omitempty"`
}

func viewOfResource(status core.ResourceStatus) ResourceView {
	return ResourceView{
		Name:            status.Resource.Name,
		Capacity:        status.Resource.Capacity,
		Held:            status.ActiveLeases,
		Free:            status.Free(),
		Waiting:         status.Waiting,
		LeaseTimeToLive: status.Resource.LeaseTimeToLive.String(),
		Description:     status.Resource.Description,
	}
}

// LeaseView is a granted slot.
type LeaseView struct {
	ID         string `json:"id"`
	Resource   string `json:"resource"`
	AgentID    string `json:"agent_id"`
	AcquiredAt string `json:"acquired_at"`
	ExpiresAt  string `json:"expires_at"`
	// RenewAfterSeconds is a hint: renew sooner than this and the slot is never at risk. It saves
	// every client from having to work the same arithmetic out for itself.
	RenewAfterSeconds int `json:"renew_after_seconds"`
}

func viewOfLease(lease core.Lease) LeaseView {
	lifetime := lease.ExpiresAt.Sub(lease.AcquiredAt)

	return LeaseView{
		ID:                lease.ID,
		Resource:          lease.ResourceName,
		AgentID:           lease.AgentID,
		AcquiredAt:        formatTime(lease.AcquiredAt),
		ExpiresAt:         formatTime(lease.ExpiresAt),
		RenewAfterSeconds: int((lifetime / 2).Seconds()),
	}
}

// QueueEntryView is one agent's place in line.
type QueueEntryView struct {
	ID          string `json:"id"`
	Resource    string `json:"resource"`
	AgentID     string `json:"agent_id"`
	AgentName   string `json:"agent_name,omitempty"`
	Priority    int    `json:"priority"`
	Reason      string `json:"reason,omitempty"`
	RequestedAt string `json:"requested_at"`
	Position    int    `json:"position"`
}

func formatTime(instant time.Time) string {
	if instant.IsZero() {
		return ""
	}

	return instant.UTC().Format(time.RFC3339Nano)
}
