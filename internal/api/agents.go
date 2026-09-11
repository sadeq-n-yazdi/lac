package api

import (
	"context"
	"encoding/json"
	"fmt"

	"sadeq.uk/lac/internal/core"
	"sadeq.uk/lac/internal/service/registry"
	"sadeq.uk/lac/internal/transport/jsonrpc"
)

// RegisterParams introduces an agent. It deliberately has no capabilities field: what an agent may
// do is the operator's policy, not the agent's request.
type RegisterParams struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Workdir   string `json:"workdir"`
	ProcessID int    `json:"pid"`
}

// RegisterResult carries the token. It is the only time the token is ever sent: LAC stores a hash
// and cannot tell it to you again.
type RegisterResult struct {
	Agent AgentView `json:"agent"`
	Token string    `json:"token"`
}

func (a *API) handleRegister(
	ctx context.Context, session *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments RegisterParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	registration, err := a.registry.Register(ctx, registry.RegisterRequest{
		Name:      arguments.Name,
		Kind:      arguments.Kind,
		Workdir:   arguments.Workdir,
		ProcessID: arguments.ProcessID,
	})
	if err != nil {
		return nil, err
	}

	// Registering authenticates this connection too, so a client does not have to turn round and
	// present the token it was handed a moment ago.
	session.SetAgentID(registration.Agent.ID)

	return RegisterResult{Agent: viewOfAgent(registration.Agent), Token: registration.Token}, nil
}

// AuthenticateParams presents an existing token.
type AuthenticateParams struct {
	Token string `json:"token"`
}

// AuthenticateResult confirms who the connection now belongs to.
type AuthenticateResult struct {
	Agent AgentView `json:"agent"`
}

func (a *API) handleAuthenticate(
	ctx context.Context, session *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments AuthenticateParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	agent, err := a.authenticator.Authenticate(ctx, arguments.Token)
	if err != nil {
		return nil, err
	}

	session.SetAgentID(agent.ID)

	return AuthenticateResult{Agent: viewOfAgent(agent)}, nil
}

// HeartbeatResult tells the agent when it must be heard from again.
type HeartbeatResult struct {
	// NextBySeconds is how long the agent may stay silent before it is treated as gone.
	NextBySeconds int `json:"next_by_seconds"`
}

func (a *API) handleHeartbeat(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, _ json.RawMessage,
) (any, error) {
	if err := a.registry.Heartbeat(ctx, caller.ID); err != nil {
		return nil, err
	}

	return HeartbeatResult{NextBySeconds: int(a.registry.TimeToLive().Seconds())}, nil
}

// AgentListParams narrows the roster.
type AgentListParams struct {
	// States filters by presence. Empty means active agents only, which is what a caller asking
	// "who is here?" almost always means.
	States []string `json:"states,omitempty"`
	// Kinds filters by the tool behind the agent.
	Kinds []string `json:"kinds,omitempty"`
}

// AgentListResult is the roster.
type AgentListResult struct {
	Agents []AgentView `json:"agents"`
}

func (a *API) handleAgentList(
	ctx context.Context, _ core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments AgentListParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	filter := core.AgentFilter{Kinds: arguments.Kinds}
	if len(arguments.States) == 0 {
		filter.States = []core.AgentState{core.AgentActive}
	} else {
		for _, state := range arguments.States {
			agentState := core.AgentState(state)
			if !agentState.Valid() {
				return nil, fmt.Errorf("%w: unknown agent state %q", core.ErrInvalidArgument, state)
			}
			filter.States = append(filter.States, agentState)
		}
	}

	agents, err := a.registry.List(ctx, filter)
	if err != nil {
		return nil, err
	}

	return AgentListResult{Agents: viewsOfAgents(agents)}, nil
}

// DeregisterResult reports what the departing agent gave back.
type DeregisterResult struct {
	ReleasedLeases int `json:"released_leases"`
}

func (a *API) handleDeregister(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, _ json.RawMessage,
) (any, error) {
	// Give the slots back before retiring the agent, so nothing is stranded if the second step
	// fails.
	released, err := a.leasing.ReleaseEverythingHeldBy(ctx, caller.ID, "the agent deregistered")
	if err != nil {
		return nil, err
	}

	if err := a.registry.Deregister(ctx, caller.ID); err != nil {
		return nil, err
	}

	return DeregisterResult{ReleasedLeases: released}, nil
}
