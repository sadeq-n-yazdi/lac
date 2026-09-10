package auth

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"code.sadeq.uk/lac/internal/core"
)

// Permission is something an agent might be allowed to do.
type Permission string

// The permissions LAC checks. Every one of them is denied by default.
const (
	// PermissionLease is queueing for a named resource.
	PermissionLease Permission = "lease"
	// PermissionBroadcast is sending to a topic or to every agent at once.
	PermissionBroadcast Permission = "broadcast"
	// PermissionDefineResource is creating or reconfiguring a resource.
	PermissionDefineResource Permission = "define_resource"
	// PermissionRequestReports is asking the other agents to report. An operator power.
	PermissionRequestReports Permission = "request_reports"
)

// Clock returns the current time. Tests replace it; production leaves it alone.
type Clock func() time.Time

// Options configure an Authenticator. The zero value is fine.
type Options struct {
	// Clock defaults to the wall clock in UTC.
	Clock Clock
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Authenticator issues and verifies agent tokens, and answers "may this agent do that?".
type Authenticator struct {
	store  core.Store
	secret []byte
	now    Clock
	logger *slog.Logger
}

// New returns an authenticator keyed by the install secret.
func New(store core.Store, secret []byte, options Options) (*Authenticator, error) {
	if len(secret) != secretSize {
		return nil, fmt.Errorf("the secret is %d bytes, want %d", len(secret), secretSize)
	}

	clock := options.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}

	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Authenticator{store: store, secret: secret, now: clock, logger: logger}, nil
}

// Issue creates a token for an agent and stores only its hash. The returned token is the single
// copy that will ever exist: if the caller loses it, the agent must register again.
func (a *Authenticator) Issue(ctx context.Context, agentID string) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}

	credential := core.Credential{
		AgentID:   agentID,
		TokenHash: hashToken(a.secret, token),
		IssuedAt:  a.now(),
	}

	if err := a.store.Credentials().Store(ctx, credential); err != nil {
		return "", fmt.Errorf("storing the credential for %s: %w", agentID, err)
	}

	return token, nil
}

// Authenticate resolves a token to the agent it belongs to.
//
// Every failure returns the same error, whatever went wrong: a caller learns whether its token
// works, and nothing else. The reason is recorded in the audit log, where the operator can see it.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (core.Agent, error) {
	if !looksLikeToken(token) {
		a.audit(ctx, core.SystemActor, "", "the token is malformed")
		return core.Agent{}, unauthorised()
	}

	credential, err := a.store.Credentials().ByTokenHash(ctx, hashToken(a.secret, token))
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			a.audit(ctx, core.SystemActor, Redact(token), "no credential matches the token")
			return core.Agent{}, unauthorised()
		}
		return core.Agent{}, fmt.Errorf("looking up a credential: %w", err)
	}

	// The lookup already matched on the hash; comparing again in constant time keeps the check
	// correct even if the storage layer is ever changed to something less exact.
	if !hmac.Equal(credential.TokenHash, hashToken(a.secret, token)) {
		a.audit(ctx, core.SystemActor, credential.AgentID, "the token hash did not match")
		return core.Agent{}, unauthorised()
	}

	if credential.Revoked() {
		a.audit(ctx, credential.AgentID, credential.AgentID, "the credential has been revoked")
		return core.Agent{}, unauthorised()
	}

	agent, err := a.store.Agents().ByID(ctx, credential.AgentID)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			a.audit(ctx, credential.AgentID, credential.AgentID, "the agent no longer exists")
			return core.Agent{}, unauthorised()
		}
		return core.Agent{}, fmt.Errorf("reading agent %s: %w", credential.AgentID, err)
	}

	if agent.State == core.AgentDeregistered {
		a.audit(ctx, agent.ID, agent.ID, "the agent has deregistered")
		return core.Agent{}, unauthorised()
	}

	return agent, nil
}

// Revoke invalidates an agent's token immediately.
func (a *Authenticator) Revoke(ctx context.Context, agentID string) error {
	if err := a.store.Credentials().Revoke(ctx, agentID, a.now()); err != nil {
		return fmt.Errorf("revoking the credential for %s: %w", agentID, err)
	}

	return nil
}

// Authorise reports whether the agent may perform the permission on the target, which is a
// resource name for PermissionLease and empty otherwise.
//
// Denials are audited: "who tried to do what" is exactly what an operator needs after the fact.
func (a *Authenticator) Authorise(
	ctx context.Context, agent core.Agent, permission Permission, target string,
) error {
	if a.permitted(agent, permission, target) {
		return nil
	}

	a.audit(ctx, agent.ID, target, fmt.Sprintf("%s is not permitted", permission))

	if permission == PermissionLease {
		return fmt.Errorf("%w: agent %q may not queue for the resource %q",
			core.ErrUnauthorised, agent.Name, target)
	}

	return fmt.Errorf("%w: agent %q may not %s", core.ErrUnauthorised, agent.Name, permission)
}

func (a *Authenticator) permitted(agent core.Agent, permission Permission, target string) bool {
	switch permission {
	case PermissionLease:
		return agent.Capabilities.MayLease(target)
	case PermissionBroadcast:
		return agent.Capabilities.CanBroadcast
	case PermissionDefineResource:
		return agent.Capabilities.CanDefineResources
	case PermissionRequestReports:
		return agent.Capabilities.CanRequestReports
	default:
		// An unknown permission is denied: a typo in a new call site must not grant anything.
		return false
	}
}

// audit records a refusal. A failure to write the audit entry must not change the outcome of the
// decision, so it is deliberately not returned — but it is never silent either.
func (a *Authenticator) audit(ctx context.Context, actor, target, detail string) {
	if actor == "" {
		actor = core.SystemActor
	}

	entry := core.AuditEntry{
		Actor:  actor,
		Action: core.AuditAuthDenied,
		Target: target,
		Detail: detail,
		At:     a.now(),
	}

	if err := a.store.Audit().Append(ctx, entry); err != nil {
		// The audit log is the record of last resort. Losing an entry must not change the decision
		// that was just made, but it must not pass unnoticed either.
		a.logger.Error("could not record an authorisation denial",
			"actor", actor, "target", target, "detail", detail, "error", err)
	}
}

// unauthorised is the single answer every authentication failure gives the caller.
func unauthorised() error {
	return fmt.Errorf("%w: the token is not valid", core.ErrUnauthorised)
}
