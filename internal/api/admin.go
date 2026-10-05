package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"sadeq.uk/lac/internal/auth"
	"sadeq.uk/lac/internal/core"
	"sadeq.uk/lac/internal/transport/jsonrpc"
)

// The admin methods are the operator's remedies for a machine that has got stuck: an agent that is
// alive but holding on, a queue full of requests nobody will come back for. Every one of them acts
// on other agents, so every one of them checks the caller first.

// AgentParams names another agent, by name or by id.
type AgentParams struct {
	Agent string `json:"agent"`
}

// ReleasedResult counts the slots given back.
type ReleasedResult struct {
	Released int `json:"released"`
}

// CancelledResult counts the queued requests withdrawn.
type CancelledResult struct {
	Cancelled int `json:"cancelled"`
}

func (a *API) handleReleaseAgent(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	if err := a.authenticator.Authorise(ctx, caller, auth.PermissionForceRelease, ""); err != nil {
		return nil, err
	}

	target, err := a.agentNamed(ctx, params)
	if err != nil {
		return nil, err
	}

	released, err := a.leasing.ReleaseEverythingHeldBy(ctx, caller.ID, target.ID, "released by "+caller.Name)
	if err != nil {
		return nil, err
	}

	return ReleasedResult{Released: released}, nil
}

func (a *API) handleFreeResource(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	if err := a.authenticator.Authorise(ctx, caller, auth.PermissionForceRelease, ""); err != nil {
		return nil, err
	}

	var arguments ResourceNameParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	released, err := a.leasing.FreeResource(ctx, caller.ID, arguments.Resource)
	if err != nil {
		return nil, err
	}

	return ReleasedResult{Released: released}, nil
}

func (a *API) handleClearQueue(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	if err := a.authenticator.Authorise(ctx, caller, auth.PermissionForceRelease, ""); err != nil {
		return nil, err
	}

	var arguments ResourceNameParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	cancelled, err := a.leasing.ClearQueue(ctx, caller.ID, arguments.Resource)
	if err != nil {
		return nil, err
	}

	return CancelledResult{Cancelled: cancelled}, nil
}

func (a *API) handleEvict(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	if err := a.authenticator.Authorise(ctx, caller, auth.PermissionEvict, ""); err != nil {
		return nil, err
	}

	target, err := a.agentNamed(ctx, params)
	if err != nil {
		return nil, err
	}
	if target.Internal {
		return nil, fmt.Errorf("%w: %s is part of the daemon and cannot be evicted",
			core.ErrInvalidArgument, target.Name)
	}

	// The slots go back before the agent is retired, so nothing is stranded if the second step fails.
	released, err := a.leasing.ReleaseEverythingHeldBy(ctx, caller.ID, target.ID, "evicted by "+caller.Name)
	if err != nil {
		return nil, err
	}

	if err := a.registry.Evict(ctx, caller.ID, target.ID); err != nil {
		return nil, err
	}

	return ReleasedResult{Released: released}, nil
}

// agentNamed reads AgentParams and finds the agent. An id is taken as given; a name must belong to
// an agent that is still on the roster.
func (a *API) agentNamed(ctx context.Context, params json.RawMessage) (core.Agent, error) {
	var arguments AgentParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return core.Agent{}, err
	}
	if arguments.Agent == "" {
		return core.Agent{}, fmt.Errorf("%w: agent is required", core.ErrInvalidArgument)
	}

	if strings.HasPrefix(arguments.Agent, "agent_") {
		return a.registry.ByID(ctx, arguments.Agent)
	}

	return a.registry.ByName(ctx, arguments.Agent)
}
