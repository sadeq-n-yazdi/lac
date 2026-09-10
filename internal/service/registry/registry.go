// Package registry keeps the roster of agents: who is here, where they are working, what they are
// allowed to do, and whether they are still alive.
package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"code.sadeq.uk/lac/internal/auth"
	"code.sadeq.uk/lac/internal/core"
	"code.sadeq.uk/lac/internal/id"
)

// Options configure a Service.
type Options struct {
	// TimeToLive is how long an agent may go without a heartbeat before it is treated as gone.
	TimeToLive time.Duration
	// WorkdirRoots limits where an agent may claim to be working. Empty allows anywhere, which is
	// only sensible in tests.
	WorkdirRoots []string
	// CapabilitiesFor returns what an agent registering under a name is allowed to do. It is the
	// policy hook: an agent never names its own capabilities.
	CapabilitiesFor func(agentName string) core.Capabilities
	// Clock defaults to the wall clock in UTC.
	Clock auth.Clock
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Service is the agent registry.
type Service struct {
	store         core.Store
	authenticator *auth.Authenticator
	options       Options
	now           auth.Clock
	logger        *slog.Logger
}

// New returns a registry service.
func New(store core.Store, authenticator *auth.Authenticator, options Options) *Service {
	if options.Clock == nil {
		options.Clock = func() time.Time { return time.Now().UTC() }
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.CapabilitiesFor == nil {
		// No policy means no powers. A service wired up without one must not become a back door.
		options.CapabilitiesFor = func(string) core.Capabilities { return core.Capabilities{} }
	}
	if options.TimeToLive <= 0 {
		options.TimeToLive = 90 * time.Second
	}

	return &Service{
		store:         store,
		authenticator: authenticator,
		options:       options,
		now:           options.Clock,
		logger:        options.Logger,
	}
}

// RegisterRequest is an agent introducing itself.
//
// It carries no capabilities on purpose: what an agent may do is decided by policy from its name,
// never by the agent asking nicely.
type RegisterRequest struct {
	// Name identifies the agent to its peers and to the operator.
	Name string
	// Kind labels the tool behind it, such as "claude" or "codex".
	Kind string
	// Workdir is the directory it is working in.
	Workdir string
	// ProcessID lets a restarting process reclaim its own registration.
	ProcessID int
}

// Registration is the answer to a successful registration. The token exists only here: it is
// stored as a hash and can never be read back.
type Registration struct {
	Agent core.Agent
	Token string
}

// Register admits an agent and issues its token.
//
// A process re-registering under its own name reclaims its registration and gets a fresh token,
// which is what makes a restarted agent recover cleanly. A different process asking for a name
// that is in use is refused, because two agents answering to one name makes every message
// ambiguous.
func (s *Service) Register(ctx context.Context, request RegisterRequest) (Registration, error) {
	if err := core.ValidateName("agent name", request.Name); err != nil {
		return Registration{}, err
	}
	if err := core.ValidateName("agent kind", request.Kind); err != nil {
		return Registration{}, err
	}

	workdir, err := s.resolveWorkdir(request.Workdir)
	if err != nil {
		return Registration{}, err
	}

	now := s.now()
	agent := core.Agent{
		ID:              id.New("agent"),
		Name:            request.Name,
		Kind:            request.Kind,
		Workdir:         workdir,
		ProcessID:       request.ProcessID,
		Capabilities:    s.options.CapabilitiesFor(request.Name),
		State:           core.AgentActive,
		RegisteredAt:    now,
		LastHeartbeatAt: now,
	}

	if err := s.store.InTransaction(ctx, func(tx core.Store) error {
		existing, err := tx.Agents().ByName(ctx, request.Name)
		switch {
		case errors.Is(err, core.ErrNotFound):
			return tx.Agents().Create(ctx, agent)
		case err != nil:
			return err
		}

		if existing.ProcessID != request.ProcessID {
			return fmt.Errorf("%w: the name %q belongs to another live agent (pid %d). "+
				"Choose a different name, or wait for that agent to go away",
				core.ErrAlreadyExists, request.Name, existing.ProcessID)
		}

		// The same process, back again: keep its identity so its messages and leases stay attached.
		agent.ID = existing.ID
		agent.RegisteredAt = existing.RegisteredAt

		return tx.Agents().Update(ctx, agent)
	}); err != nil {
		return Registration{}, err
	}

	token, err := s.authenticator.Issue(ctx, agent.ID)
	if err != nil {
		return Registration{}, err
	}

	s.audit(ctx, agent.ID, core.AuditAgentRegistered, agent.Name,
		fmt.Sprintf("kind=%s workdir=%s pid=%d", agent.Kind, agent.Workdir, agent.ProcessID))

	s.logger.Info("agent registered",
		"agent", agent.Name, "kind", agent.Kind, "workdir", agent.Workdir, "id", agent.ID)

	return Registration{Agent: agent, Token: token}, nil
}

// Heartbeat records that an agent is still there.
func (s *Service) Heartbeat(ctx context.Context, agentID string) error {
	if err := s.store.Agents().Heartbeat(ctx, agentID, s.now()); err != nil {
		return fmt.Errorf("recording a heartbeat: %w", err)
	}

	return nil
}

// List returns the roster.
func (s *Service) List(ctx context.Context, filter core.AgentFilter) ([]core.Agent, error) {
	agents, err := s.store.Agents().List(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("listing agents: %w", err)
	}

	return agents, nil
}

// Active returns the agents that have heartbeated recently enough to count as present.
func (s *Service) Active(ctx context.Context) ([]core.Agent, error) {
	agents, err := s.List(ctx, core.AgentFilter{States: []core.AgentState{core.AgentActive}})
	if err != nil {
		return nil, err
	}

	now := s.now()
	present := make([]core.Agent, 0, len(agents))
	for _, agent := range agents {
		if agent.IsAlive(now, s.options.TimeToLive) {
			present = append(present, agent)
		}
	}

	return present, nil
}

// Deregister retires an agent and invalidates its token immediately.
func (s *Service) Deregister(ctx context.Context, agentID string) error {
	now := s.now()

	if err := s.store.InTransaction(ctx, func(tx core.Store) error {
		return tx.Agents().SetState(ctx, agentID, core.AgentDeregistered, now)
	}); err != nil {
		return err
	}

	if err := s.authenticator.Revoke(ctx, agentID); err != nil {
		return err
	}

	s.audit(ctx, agentID, core.AuditAgentDeregistered, agentID, "the agent said goodbye")
	s.logger.Info("agent deregistered", "id", agentID)

	return nil
}

// ReapStale marks the agents that stopped heartbeating and reports how many it found. Whatever
// they were holding is released by the leasing service, which watches for the same condition.
func (s *Service) ReapStale(ctx context.Context) ([]core.Agent, error) {
	now := s.now()
	cutoff := now.Add(-s.options.TimeToLive)

	stale, err := s.store.Agents().StaleBefore(ctx, cutoff)
	if err != nil {
		return nil, fmt.Errorf("finding stale agents: %w", err)
	}

	for _, agent := range stale {
		if err := s.store.Agents().SetState(ctx, agent.ID, core.AgentStale, now); err != nil {
			return nil, fmt.Errorf("marking %s stale: %w", agent.Name, err)
		}

		s.audit(ctx, core.SystemActor, core.AuditAgentWentStale, agent.ID,
			fmt.Sprintf("no heartbeat since %s", agent.LastHeartbeatAt.Format(time.RFC3339)))
		s.logger.Warn("agent went stale",
			"agent", agent.Name, "last_heartbeat", agent.LastHeartbeatAt, "id", agent.ID)
	}

	return stale, nil
}

// resolveWorkdir checks that a claimed working directory is somewhere the operator allows.
//
// The value is only ever reported to other agents — LAC never executes anything in it — but a
// registration pointing somewhere unexpected is worth refusing rather than advertising.
func (s *Service) resolveWorkdir(workdir string) (string, error) {
	if workdir == "" {
		return "", fmt.Errorf("%w: a working directory is required", core.ErrInvalidArgument)
	}
	if !filepath.IsAbs(workdir) {
		return "", fmt.Errorf("%w: the working directory %q must be absolute", core.ErrInvalidArgument, workdir)
	}

	cleaned := filepath.Clean(workdir)

	if len(s.options.WorkdirRoots) == 0 {
		return cleaned, nil
	}

	for _, root := range s.options.WorkdirRoots {
		if within(cleaned, filepath.Clean(root)) {
			return cleaned, nil
		}
	}

	return "", fmt.Errorf("%w: the working directory %q is outside the allowed roots %s",
		core.ErrInvalidArgument, cleaned, strings.Join(s.options.WorkdirRoots, ", "))
}

// within reports whether path is root or sits underneath it. Comparing cleaned paths component by
// component avoids the classic mistake where "/home/user-evil" passes a prefix test for "/home/user".
func within(path, root string) bool {
	if path == root {
		return true
	}

	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}

	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (s *Service) audit(ctx context.Context, actor string, action core.AuditAction, target, detail string) {
	entry := core.AuditEntry{Actor: actor, Action: action, Target: target, Detail: detail, At: s.now()}

	if err := s.store.Audit().Append(ctx, entry); err != nil {
		s.logger.Error("could not write an audit entry",
			"action", action, "target", target, "error", err)
	}
}
