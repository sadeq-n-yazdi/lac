package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"code.sadeq.uk/lac/internal/auth"
	"code.sadeq.uk/lac/internal/core"
	"code.sadeq.uk/lac/internal/service/leasing"
	"code.sadeq.uk/lac/internal/transport/jsonrpc"
)

// DefineResourceParams creates or reconfigures a resource.
type DefineResourceParams struct {
	Name string `json:"name"`
	// Capacity is how many agents may hold a slot at once.
	Capacity int `json:"capacity"`
	// LeaseTimeToLive is a duration such as "15m". Empty uses the daemon default.
	LeaseTimeToLive string `json:"lease_time_to_live,omitempty"`
	Description     string `json:"description,omitempty"`
}

// ResourceResult is one resource and how busy it is.
type ResourceResult struct {
	Resource ResourceView `json:"resource"`
}

func (a *API) handleDefineResource(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments DefineResourceParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	// The machine's limits are the operator's decision. An agent that could redefine them could
	// give itself as many test slots as it liked.
	if err := a.authenticator.Authorise(ctx, caller, auth.PermissionDefineResource, arguments.Name); err != nil {
		return nil, err
	}

	timeToLive, err := parseDuration(arguments.LeaseTimeToLive)
	if err != nil {
		return nil, err
	}

	resource := core.Resource{
		Name:            arguments.Name,
		Capacity:        arguments.Capacity,
		LeaseTimeToLive: timeToLive,
		Description:     arguments.Description,
	}

	if err := a.leasing.Define(ctx, caller.ID, resource); err != nil {
		return nil, err
	}

	status, err := a.leasing.Status(ctx, arguments.Name)
	if err != nil {
		return nil, err
	}

	return ResourceResult{Resource: viewOfResource(status)}, nil
}

// ResourceListResult is every resource on this machine.
type ResourceListResult struct {
	Resources []ResourceView `json:"resources"`
}

func (a *API) handleResourceList(
	ctx context.Context, _ core.Agent, _ *jsonrpc.Session, _ json.RawMessage,
) (any, error) {
	statuses, err := a.leasing.List(ctx)
	if err != nil {
		return nil, err
	}

	views := make([]ResourceView, 0, len(statuses))
	for _, status := range statuses {
		views = append(views, viewOfResource(status))
	}

	return ResourceListResult{Resources: views}, nil
}

// ResourceNameParams names a resource.
type ResourceNameParams struct {
	Resource string `json:"resource"`
}

func (a *API) handleResourceStatus(
	ctx context.Context, _ core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments ResourceNameParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	status, err := a.leasing.Status(ctx, arguments.Resource)
	if err != nil {
		return nil, err
	}

	return ResourceResult{Resource: viewOfResource(status)}, nil
}

// AcquireParams asks for a slot.
//
// The call blocks until the slot is granted or the caller gives up, which is the point: an agent
// that has to poll for its turn is an agent burning the very resource it is waiting for.
type AcquireParams struct {
	Resource string `json:"resource"`
	// Priority orders this request against the others waiting. Higher goes first.
	Priority int `json:"priority,omitempty"`
	// Reason is shown in the queue, so the operator can see who is waiting for what.
	Reason string `json:"reason,omitempty"`
	// Metadata is free-form JSON attached to the lease, such as the command about to run.
	Metadata json.RawMessage `json:"metadata,omitempty"`
	// NoWait asks for an immediate answer instead of a place in the queue.
	NoWait bool `json:"no_wait,omitempty"`
	// Timeout gives up after a duration such as "5m". Empty waits as long as the connection lives.
	Timeout string `json:"timeout,omitempty"`
}

// LeaseResult is a granted slot.
type LeaseResult struct {
	Lease LeaseView `json:"lease"`
}

func (a *API) handleAcquire(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments AcquireParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	if err := a.authenticator.Authorise(ctx, caller, auth.PermissionLease, arguments.Resource); err != nil {
		return nil, err
	}

	timeout, err := parseDuration(arguments.Timeout)
	if err != nil {
		return nil, err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	lease, err := a.leasing.Acquire(ctx, leasing.AcquireRequest{
		AgentID:      caller.ID,
		ResourceName: arguments.Resource,
		Priority:     core.Priority(arguments.Priority),
		Reason:       arguments.Reason,
		Metadata:     arguments.Metadata,
		NoWait:       arguments.NoWait,
	})
	if err != nil {
		return nil, err
	}

	return LeaseResult{Lease: viewOfLease(lease)}, nil
}

// LeaseParams names a lease the caller holds.
type LeaseParams struct {
	LeaseID string `json:"lease_id"`
}

func (a *API) handleRenew(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments LeaseParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	lease, err := a.leasing.Renew(ctx, caller.ID, arguments.LeaseID)
	if err != nil {
		return nil, err
	}

	return LeaseResult{Lease: viewOfLease(lease)}, nil
}

// ReleaseResult confirms the slot is back.
type ReleaseResult struct {
	Released bool `json:"released"`
}

func (a *API) handleRelease(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments LeaseParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	if err := a.leasing.Release(ctx, caller.ID, arguments.LeaseID); err != nil {
		return nil, err
	}

	return ReleaseResult{Released: true}, nil
}

// HeldResult is every slot the caller holds.
type HeldResult struct {
	Leases []LeaseView `json:"leases"`
}

func (a *API) handleHeld(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, _ json.RawMessage,
) (any, error) {
	leases, err := a.leasing.Held(ctx, caller.ID)
	if err != nil {
		return nil, err
	}

	views := make([]LeaseView, 0, len(leases))
	for _, lease := range leases {
		views = append(views, viewOfLease(lease))
	}

	return HeldResult{Leases: views}, nil
}

// QueueStatusResult is who is waiting, in the order they will be served.
type QueueStatusResult struct {
	Resource ResourceView     `json:"resource"`
	Waiting  []QueueEntryView `json:"waiting"`
}

func (a *API) handleQueueStatus(
	ctx context.Context, _ core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments ResourceNameParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	status, err := a.leasing.Status(ctx, arguments.Resource)
	if err != nil {
		return nil, err
	}

	entries, err := a.leasing.Queue(ctx, arguments.Resource)
	if err != nil {
		return nil, err
	}

	waiting := make([]QueueEntryView, 0, len(entries))
	for position, entry := range entries {
		waiting = append(waiting, QueueEntryView{
			ID:          entry.ID,
			Resource:    entry.ResourceName,
			AgentID:     entry.AgentID,
			AgentName:   a.nameOf(ctx, entry.AgentID),
			Priority:    int(entry.Priority),
			Reason:      entry.Reason,
			RequestedAt: formatTime(entry.RequestedAt),
			Position:    position + 1,
		})
	}

	return QueueStatusResult{Resource: viewOfResource(status), Waiting: waiting}, nil
}

// QueueCancelParams withdraws a request from a queue.
type QueueCancelParams struct {
	EntryID string `json:"entry_id"`
}

// QueueCancelResult confirms the withdrawal.
type QueueCancelResult struct {
	Cancelled bool `json:"cancelled"`
}

func (a *API) handleQueueCancel(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	var arguments QueueCancelParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	if err := a.leasing.Cancel(ctx, caller.ID, arguments.EntryID); err != nil {
		return nil, err
	}

	return QueueCancelResult{Cancelled: true}, nil
}

// parseDuration reads a duration such as "15m". An empty value means "not set", which each caller
// interprets for itself.
func parseDuration(text string) (time.Duration, error) {
	if text == "" {
		return 0, nil
	}

	parsed, err := time.ParseDuration(text)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a duration such as \"15m\"", core.ErrInvalidArgument, text)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%w: the duration %q must not be negative", core.ErrInvalidArgument, text)
	}

	return parsed, nil
}
