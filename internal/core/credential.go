package core

import "time"

// Credential is an agent's authentication secret, kept apart from Agent so that the value objects
// handed to transports and printed in listings can never carry it by accident.
//
// Only the hash is ever stored. The plaintext token exists once, in the response to registration.
type Credential struct {
	// AgentID is the agent this credential authenticates.
	AgentID string
	// TokenHash is a keyed hash of the token. Never a bare digest, so a stolen database does not
	// permit an offline guessing attack against short-lived tokens.
	TokenHash []byte
	// IssuedAt is when the token was created.
	IssuedAt time.Time
	// RevokedAt is when it stopped being valid. Zero means it is still valid.
	RevokedAt time.Time
}

// Revoked reports whether the credential can no longer authenticate.
func (c Credential) Revoked() bool { return !c.RevokedAt.IsZero() }
