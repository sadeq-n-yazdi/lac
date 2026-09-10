package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"code.sadeq.uk/lac/internal/core"
)

type agentRepository struct{ queries querier }

var _ core.AgentRepository = agentRepository{}

const agentColumns = `id, name, kind, workdir, process_id, capabilities, state, registered_at, last_heartbeat_at`

// storedCapabilities is the on-disk shape of core.Capabilities. It is defined here rather than in
// the domain so that a change to the storage format never forces a change to the domain type.
type storedCapabilities struct {
	Resources          []string `json:"resources"`
	CanBroadcast       bool     `json:"can_broadcast"`
	CanDefineResources bool     `json:"can_define_resources"`
	CanRequestReports  bool     `json:"can_request_reports"`
}

func encodeCapabilities(capabilities core.Capabilities) (string, error) {
	encoded, err := json.Marshal(storedCapabilities{
		Resources:          capabilities.Resources,
		CanBroadcast:       capabilities.CanBroadcast,
		CanDefineResources: capabilities.CanDefineResources,
		CanRequestReports:  capabilities.CanRequestReports,
	})
	if err != nil {
		return "", fmt.Errorf("encoding capabilities: %w", err)
	}

	return string(encoded), nil
}

func decodeCapabilities(encoded string) (core.Capabilities, error) {
	var stored storedCapabilities
	if err := json.Unmarshal([]byte(encoded), &stored); err != nil {
		return core.Capabilities{}, fmt.Errorf("decoding capabilities: %w", err)
	}

	return core.Capabilities{
		Resources:          stored.Resources,
		CanBroadcast:       stored.CanBroadcast,
		CanDefineResources: stored.CanDefineResources,
		CanRequestReports:  stored.CanRequestReports,
	}, nil
}

func (r agentRepository) Create(ctx context.Context, agent core.Agent) error {
	if err := agent.Validate(); err != nil {
		return err
	}

	capabilities, err := encodeCapabilities(agent.Capabilities)
	if err != nil {
		return err
	}

	_, err = r.queries.ExecContext(ctx, `
		INSERT INTO agents (`+agentColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		agent.ID, agent.Name, agent.Kind, agent.Workdir, agent.ProcessID, capabilities,
		string(agent.State), requireMicros(agent.RegisteredAt), requireMicros(agent.LastHeartbeatAt),
	)

	return translateError("creating agent "+agent.Name, err)
}

func (r agentRepository) Update(ctx context.Context, agent core.Agent) error {
	if err := agent.Validate(); err != nil {
		return err
	}

	capabilities, err := encodeCapabilities(agent.Capabilities)
	if err != nil {
		return err
	}

	result, err := r.queries.ExecContext(ctx, `
		UPDATE agents
		   SET name = ?, kind = ?, workdir = ?, process_id = ?, capabilities = ?,
		       state = ?, registered_at = ?, last_heartbeat_at = ?
		 WHERE id = ?`,
		agent.Name, agent.Kind, agent.Workdir, agent.ProcessID, capabilities, string(agent.State),
		requireMicros(agent.RegisteredAt), requireMicros(agent.LastHeartbeatAt), agent.ID,
	)

	return affectedOrNotFound("updating agent "+agent.ID, result, err)
}

func (r agentRepository) ByID(ctx context.Context, agentID string) (core.Agent, error) {
	row := r.queries.QueryRowContext(ctx, `SELECT `+agentColumns+` FROM agents WHERE id = ?`, agentID)

	agent, err := scanAgent(row)
	if err != nil {
		return core.Agent{}, translateError("reading agent "+agentID, err)
	}

	return agent, nil
}

func (r agentRepository) ByName(ctx context.Context, name string) (core.Agent, error) {
	row := r.queries.QueryRowContext(ctx,
		`SELECT `+agentColumns+` FROM agents WHERE name = ? AND state = ?`, name, string(core.AgentActive))

	agent, err := scanAgent(row)
	if err != nil {
		return core.Agent{}, translateError("reading agent "+name, err)
	}

	return agent, nil
}

func (r agentRepository) List(ctx context.Context, filter core.AgentFilter) ([]core.Agent, error) {
	query := `SELECT ` + agentColumns + ` FROM agents`
	conditions := make([]string, 0, 2)
	arguments := make([]any, 0, len(filter.States)+len(filter.Kinds))

	if len(filter.States) > 0 {
		conditions = append(conditions, `state IN (`+placeholders(len(filter.States))+`)`)
		for _, state := range filter.States {
			arguments = append(arguments, string(state))
		}
	}
	if len(filter.Kinds) > 0 {
		conditions = append(conditions, `kind IN (`+placeholders(len(filter.Kinds))+`)`)
		for _, kind := range filter.Kinds {
			arguments = append(arguments, kind)
		}
	}
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	query += ` ORDER BY registered_at, id`

	rows, err := r.queries.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, translateError("listing agents", err)
	}
	defer func() { _ = rows.Close() }()

	agents := make([]core.Agent, 0, 8)
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, translateError("listing agents", err)
		}
		agents = append(agents, agent)
	}
	if err := rows.Err(); err != nil {
		return nil, translateError("listing agents", err)
	}

	return agents, nil
}

func (r agentRepository) Heartbeat(ctx context.Context, agentID string, at time.Time) error {
	result, err := r.queries.ExecContext(ctx, `
		UPDATE agents
		   SET last_heartbeat_at = ?, state = ?
		 WHERE id = ? AND state <> ?`,
		requireMicros(at), string(core.AgentActive), agentID, string(core.AgentDeregistered),
	)

	// A heartbeat revives a stale agent: it went quiet, but it is evidently still there.
	return affectedOrNotFound("recording a heartbeat for "+agentID, result, err)
}

func (r agentRepository) SetState(ctx context.Context, agentID string, state core.AgentState, at time.Time) error {
	if !state.Valid() {
		return fmt.Errorf("%w: unknown agent state %q", core.ErrInvalidArgument, state)
	}

	result, err := r.queries.ExecContext(ctx, `
		UPDATE agents SET state = ?, last_heartbeat_at = max(last_heartbeat_at, ?) WHERE id = ?`,
		string(state), requireMicros(at), agentID,
	)

	return affectedOrNotFound("setting the state of "+agentID, result, err)
}

func (r agentRepository) StaleBefore(ctx context.Context, cutoff time.Time) ([]core.Agent, error) {
	rows, err := r.queries.QueryContext(ctx, `
		SELECT `+agentColumns+`
		  FROM agents
		 WHERE state = ? AND last_heartbeat_at < ?
		 ORDER BY last_heartbeat_at`,
		string(core.AgentActive), requireMicros(cutoff),
	)
	if err != nil {
		return nil, translateError("finding stale agents", err)
	}
	defer func() { _ = rows.Close() }()

	agents := make([]core.Agent, 0, 4)
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, translateError("finding stale agents", err)
		}
		agents = append(agents, agent)
	}
	if err := rows.Err(); err != nil {
		return nil, translateError("finding stale agents", err)
	}

	return agents, nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows, so one scan function serves single reads
// and listings alike.
type scanner interface {
	Scan(destination ...any) error
}

func scanAgent(source scanner) (core.Agent, error) {
	var (
		agent           core.Agent
		capabilities    string
		state           string
		registeredAt    int64
		lastHeartbeatAt int64
	)

	if err := source.Scan(&agent.ID, &agent.Name, &agent.Kind, &agent.Workdir, &agent.ProcessID,
		&capabilities, &state, &registeredAt, &lastHeartbeatAt); err != nil {
		return core.Agent{}, err
	}

	decoded, err := decodeCapabilities(capabilities)
	if err != nil {
		return core.Agent{}, err
	}

	agent.Capabilities = decoded
	agent.State = core.AgentState(state)
	agent.RegisteredAt = fromRequiredMicros(registeredAt)
	agent.LastHeartbeatAt = fromRequiredMicros(lastHeartbeatAt)

	return agent, nil
}
