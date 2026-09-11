package core

import (
	"fmt"
	"regexp"
	"slices"
	"time"
)

// AgentState describes whether an agent is still considered present.
type AgentState string

const (
	// AgentActive means the agent has sent a heartbeat within its time to live.
	AgentActive AgentState = "active"
	// AgentStale means the agent stopped sending heartbeats. Its leases and queue entries are
	// released, but its record and its messages are kept so the operator can see what happened.
	AgentStale AgentState = "stale"
	// AgentDeregistered means the agent said goodbye. Its token no longer authenticates.
	AgentDeregistered AgentState = "deregistered"
)

// Valid reports whether the state is one this domain defines.
func (s AgentState) Valid() bool {
	return s == AgentActive || s == AgentStale || s == AgentDeregistered
}

// nameSyntax keeps agent, resource and topic names safe to print, to type and to use as map keys.
var nameSyntax = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidateName checks a user-supplied name used as an identity, describing the field in errors.
func ValidateName(field, name string) error {
	if !nameSyntax.MatchString(name) {
		return fmt.Errorf("%w: %s %q must be 1-63 characters of a-z, 0-9, dot, dash or underscore, "+
			"starting with a letter or digit", ErrInvalidArgument, field, name)
	}
	return nil
}

// Capabilities is what an agent is permitted to do. The zero value grants nothing, so a caller
// that forgets to set it is denied rather than trusted.
type Capabilities struct {
	// Resources lists the resource names the agent may queue for. The single entry "*" grants
	// every resource, including ones defined later.
	Resources []string
	// CanBroadcast allows sending to a topic or to every agent at once.
	CanBroadcast bool
	// CanDefineResources allows creating and reconfiguring resources.
	CanDefineResources bool
	// CanRequestReports allows asking the other agents to report. This is an operator power.
	CanRequestReports bool
	// CanReadLog allows reading every agent's traffic, not only your own inbox. An operator power:
	// it is the difference between hearing what was said to you and reading everyone's post.
	CanReadLog bool
}

// WildcardResource grants access to every resource when it appears in Capabilities.Resources.
const WildcardResource = "*"

// MayLease reports whether these capabilities permit queueing for the named resource.
func (c Capabilities) MayLease(resource string) bool {
	return slices.Contains(c.Resources, WildcardResource) || slices.Contains(c.Resources, resource)
}

// Agent is a process that has registered with the daemon: an AI coding session, a worker, or the
// operator's own CLI.
type Agent struct {
	// ID is assigned by the daemon and never reused.
	ID string
	// Name is chosen by the agent and unique among agents that are not deregistered.
	Name string
	// Kind is a free-form label for the tool behind the agent, such as "claude" or "codex".
	Kind string
	// Workdir is the directory the agent works in. It is reported to other agents and to the
	// operator; the daemon never executes anything in it.
	Workdir string
	// ProcessID is the agent's PID, used to detect a re-registration from the same process.
	ProcessID int
	// Capabilities is what this agent is allowed to do.
	Capabilities Capabilities
	// Internal marks one of the daemon's own components rather than a session somebody is working
	// in. It is on the roster so it can be addressed by name, but nobody is behind it to answer a
	// question, so it is not asked for reports.
	Internal bool
	// State is the agent's presence.
	State AgentState
	// RegisteredAt is when the agent first registered.
	RegisteredAt time.Time
	// LastHeartbeatAt is when the daemon last heard from the agent.
	LastHeartbeatAt time.Time
}

// Validate reports whether the agent is well formed enough to store.
func (a Agent) Validate() error {
	if a.ID == "" {
		return fmt.Errorf("%w: agent id is required", ErrInvalidArgument)
	}
	if err := ValidateName("agent name", a.Name); err != nil {
		return err
	}
	if err := ValidateName("agent kind", a.Kind); err != nil {
		return err
	}
	if a.Workdir == "" {
		return fmt.Errorf("%w: agent workdir is required", ErrInvalidArgument)
	}
	if !a.State.Valid() {
		return fmt.Errorf("%w: unknown agent state %q", ErrInvalidArgument, a.State)
	}
	return nil
}

// IsAlive reports whether the agent has sent a heartbeat within timeToLive of the given instant.
func (a Agent) IsAlive(at time.Time, timeToLive time.Duration) bool {
	return a.State == AgentActive && at.Sub(a.LastHeartbeatAt) <= timeToLive
}
