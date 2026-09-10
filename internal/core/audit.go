package core

import "time"

// AuditAction names something that changed state. Keeping these as constants means the audit log
// can be searched reliably rather than by guessing at free-form strings.
type AuditAction string

// The audited actions.
const (
	AuditAgentRegistered   AuditAction = "agent.registered"
	AuditAgentDeregistered AuditAction = "agent.deregistered"
	AuditAgentWentStale    AuditAction = "agent.stale"
	AuditAuthDenied        AuditAction = "auth.denied"
	AuditMessageSent       AuditAction = "message.sent"
	AuditResourceDefined   AuditAction = "resource.defined"
	AuditLeaseGranted      AuditAction = "lease.granted"
	AuditLeaseRenewed      AuditAction = "lease.renewed"
	AuditLeaseReleased     AuditAction = "lease.released"
	AuditLeaseExpired      AuditAction = "lease.expired"
	AuditQueueJoined       AuditAction = "queue.joined"
	AuditQueueCancelled    AuditAction = "queue.cancelled"
	AuditReportRequested   AuditAction = "report.requested"
)

// AuditEntry is one append-only record of a state change. Nothing ever updates or deletes these.
type AuditEntry struct {
	// ID is assigned by the store and increases monotonically.
	ID int64
	// Actor is the agent id responsible, or "system" for the daemon's own housekeeping.
	Actor string
	// Action is what happened.
	Action AuditAction
	// Target is what it happened to: an agent id, a resource name, a lease id.
	Target string
	// Detail is free-form context. It must never contain a token or any other secret.
	Detail string
	// At is when it happened.
	At time.Time
}

// SystemActor is the actor recorded for the daemon's own housekeeping, such as reaping a lease.
const SystemActor = "system"
