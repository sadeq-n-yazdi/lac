package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"sadeq.uk/lac/internal/core"
)

type credentialRepository struct{ queries querier }

var _ core.CredentialRepository = credentialRepository{}

func (r credentialRepository) Store(ctx context.Context, credential core.Credential) error {
	if credential.AgentID == "" || len(credential.TokenHash) == 0 {
		return fmt.Errorf("%w: a credential needs an agent and a token hash", core.ErrInvalidArgument)
	}

	_, err := r.queries.ExecContext(ctx, `
		INSERT INTO credentials (agent_id, token_hash, issued_at, revoked_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (agent_id) DO UPDATE
		   SET token_hash = excluded.token_hash,
		       issued_at  = excluded.issued_at,
		       revoked_at = excluded.revoked_at`,
		credential.AgentID, credential.TokenHash,
		requireMicros(credential.IssuedAt), toMicros(credential.RevokedAt),
	)

	return translateError("storing the credential for "+credential.AgentID, err)
}

// ByTokenHash looks the hash up through its unique index. The comparison happens in the index, so
// no application code ever walks a list of hashes.
func (r credentialRepository) ByTokenHash(ctx context.Context, tokenHash []byte) (core.Credential, error) {
	if len(tokenHash) == 0 {
		return core.Credential{}, fmt.Errorf("%w: an empty token hash", core.ErrInvalidArgument)
	}

	var (
		credential core.Credential
		issuedAt   int64
		revokedAt  sql.NullInt64
	)

	err := r.queries.QueryRowContext(ctx, `
		SELECT agent_id, token_hash, issued_at, revoked_at FROM credentials WHERE token_hash = ?`,
		tokenHash,
	).Scan(&credential.AgentID, &credential.TokenHash, &issuedAt, &revokedAt)
	if err != nil {
		// The message deliberately says nothing about the value that was looked up.
		return core.Credential{}, translateError("looking up a credential", err)
	}

	credential.IssuedAt = fromRequiredMicros(issuedAt)
	credential.RevokedAt = fromMicros(revokedAt)

	return credential, nil
}

// Revoke invalidates an agent's credential. Revoking one that is not there is not an error, so
// deregistration can be retried safely.
func (r credentialRepository) Revoke(ctx context.Context, agentID string, at time.Time) error {
	_, err := r.queries.ExecContext(ctx,
		`UPDATE credentials SET revoked_at = ? WHERE agent_id = ? AND revoked_at IS NULL`,
		requireMicros(at), agentID,
	)

	return translateError("revoking the credential for "+agentID, err)
}
