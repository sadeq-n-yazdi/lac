package auth_test

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code.sadeq.uk/lac/internal/auth"
	"code.sadeq.uk/lac/internal/core"
	"code.sadeq.uk/lac/internal/id"
	"code.sadeq.uk/lac/internal/store/sqlite"
)

var baseTime = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newAuthenticator(t *testing.T) (*auth.Authenticator, *sqlite.Store, string) {
	t.Helper()

	databasePath := filepath.Join(t.TempDir(), "lac.db")

	store, err := sqlite.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	secret, err := auth.LoadOrCreateSecret(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatalf("creating the secret: %v", err)
	}

	authenticator, err := auth.New(store, secret, auth.Options{
		Clock:  func() time.Time { return baseTime },
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}

	return authenticator, store, databasePath
}

// storedTokenHash reads the credential straight out of the database file, which is what an
// attacker who copied it would see. Going through the repository would only show what LAC chooses
// to expose; the point here is what is actually written to disk.
func storedTokenHash(t *testing.T, databasePath, agentID string) []byte {
	t.Helper()

	database, err := sql.Open("sqlite", "file:"+databasePath)
	if err != nil {
		t.Fatalf("opening %s: %v", databasePath, err)
	}
	defer func() { _ = database.Close() }()

	var hash []byte
	if err := database.QueryRowContext(t.Context(),
		`SELECT token_hash FROM credentials WHERE agent_id = ?`, agentID,
	).Scan(&hash); err != nil {
		t.Fatalf("reading the stored credential: %v", err)
	}

	return hash
}

func registerAgent(t *testing.T, store *sqlite.Store, name string, capabilities core.Capabilities) core.Agent {
	t.Helper()

	agent := core.Agent{
		ID: id.New("agent"), Name: name, Kind: "claude", Workdir: "/tmp/" + name,
		Capabilities: capabilities, State: core.AgentActive,
		RegisteredAt: baseTime, LastHeartbeatAt: baseTime,
	}
	if err := store.Agents().Create(t.Context(), agent); err != nil {
		t.Fatalf("creating the agent: %v", err)
	}

	return agent
}

// A token must identify exactly the agent it was issued to, and nobody else.
func TestIssueAndAuthenticate(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t)
	agent := registerAgent(t, store, "claude-a", core.Capabilities{})

	token, err := authenticator.Issue(t.Context(), agent.ID)
	if err != nil {
		t.Fatalf("Issue() = %v, want nil", err)
	}
	if !strings.HasPrefix(token, auth.TokenPrefix) {
		t.Errorf("token %q does not start with %q", token, auth.TokenPrefix)
	}

	authenticated, err := authenticator.Authenticate(t.Context(), token)
	if err != nil {
		t.Fatalf("Authenticate() = %v, want nil", err)
	}
	if authenticated.ID != agent.ID {
		t.Errorf("authenticated as %q, want %q", authenticated.ID, agent.ID)
	}
}

// The plaintext token must exist only in the reply to registration. Anyone who copies the database
// must find nothing they can present as a token.
func TestTheDatabaseNeverHoldsThePlaintextToken(t *testing.T) {
	authenticator, store, databasePath := newAuthenticator(t)
	agent := registerAgent(t, store, "claude-a", core.Capabilities{})

	token, err := authenticator.Issue(t.Context(), agent.ID)
	if err != nil {
		t.Fatalf("Issue() = %v, want nil", err)
	}

	stored := storedTokenHash(t, databasePath, agent.ID)

	if bytes.Contains(stored, []byte(token)) {
		t.Fatal("the stored credential contains the plaintext token")
	}

	// And it must not be a bare digest either: that would let anyone who copied the database run an
	// offline guessing attack at full speed.
	bare := sha256.Sum256([]byte(token))
	if bytes.Equal(stored, bare[:]) {
		t.Error("the stored hash is an unkeyed SHA-256 of the token")
	}
}

// A stolen or guessed token must not authenticate, and the failure must not say which part failed.
func TestAuthenticateRejectsBadTokens(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t)
	agent := registerAgent(t, store, "claude-a", core.Capabilities{})

	valid, err := authenticator.Issue(t.Context(), agent.ID)
	if err != nil {
		t.Fatalf("Issue() = %v, want nil", err)
	}

	tests := map[string]string{
		"empty":            "",
		"no prefix":        strings.TrimPrefix(valid, auth.TokenPrefix),
		"wrong prefix":     "tok_" + strings.TrimPrefix(valid, auth.TokenPrefix),
		"truncated":        valid[:len(valid)-4],
		"not base64":       auth.TokenPrefix + "!!!!not-valid!!!!",
		"right shape only": auth.TokenPrefix + strings.Repeat("A", 43),
	}

	var messages []string
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := authenticator.Authenticate(t.Context(), token)
			if !errors.Is(err, core.ErrUnauthorised) {
				t.Fatalf("Authenticate(%q) = %v, want ErrUnauthorised", name, err)
			}
			messages = append(messages, err.Error())
		})
	}

	// Every failure must read the same, so a caller learns whether its token works and nothing more.
	for _, message := range messages {
		if message != messages[0] {
			t.Errorf("failures are distinguishable: %q and %q", messages[0], message)
		}
	}
}

// Deregistering an agent must lock it out at once, not at some later sweep.
func TestRevokedTokensStopWorkingImmediately(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t)
	agent := registerAgent(t, store, "claude-a", core.Capabilities{})

	token, err := authenticator.Issue(t.Context(), agent.ID)
	if err != nil {
		t.Fatalf("Issue() = %v, want nil", err)
	}
	if _, err := authenticator.Authenticate(t.Context(), token); err != nil {
		t.Fatalf("the token did not work before revocation: %v", err)
	}

	if err := authenticator.Revoke(t.Context(), agent.ID); err != nil {
		t.Fatalf("Revoke() = %v, want nil", err)
	}

	if _, err := authenticator.Authenticate(t.Context(), token); !errors.Is(err, core.ErrUnauthorised) {
		t.Errorf("Authenticate() after revocation = %v, want ErrUnauthorised", err)
	}
}

// A deregistered agent's token must not work even if revocation somehow did not happen.
func TestDeregisteredAgentsCannotAuthenticate(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t)
	agent := registerAgent(t, store, "claude-a", core.Capabilities{})

	token, err := authenticator.Issue(t.Context(), agent.ID)
	if err != nil {
		t.Fatalf("Issue() = %v, want nil", err)
	}
	if err := store.Agents().SetState(t.Context(), agent.ID, core.AgentDeregistered, baseTime); err != nil {
		t.Fatalf("SetState() = %v, want nil", err)
	}

	if _, err := authenticator.Authenticate(t.Context(), token); !errors.Is(err, core.ErrUnauthorised) {
		t.Errorf("Authenticate() = %v, want ErrUnauthorised", err)
	}
}

// Re-issuing replaces the old token, so a leaked one can be retired by registering again.
func TestIssuingAgainInvalidatesTheOldToken(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t)
	agent := registerAgent(t, store, "claude-a", core.Capabilities{})

	first, err := authenticator.Issue(t.Context(), agent.ID)
	if err != nil {
		t.Fatalf("Issue() = %v, want nil", err)
	}
	second, err := authenticator.Issue(t.Context(), agent.ID)
	if err != nil {
		t.Fatalf("a second Issue() = %v, want nil", err)
	}

	if _, err := authenticator.Authenticate(t.Context(), second); err != nil {
		t.Errorf("the new token does not work: %v", err)
	}
	if _, err := authenticator.Authenticate(t.Context(), first); !errors.Is(err, core.ErrUnauthorised) {
		t.Errorf("the old token still works: %v", err)
	}
}

// Capabilities are deny by default: an agent registered without them can do nothing.
func TestAuthoriseDeniesByDefault(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t)
	agent := registerAgent(t, store, "claude-a", core.Capabilities{})

	permissions := []struct {
		permission auth.Permission
		target     string
	}{
		{auth.PermissionLease, "test"},
		{auth.PermissionBroadcast, ""},
		{auth.PermissionDefineResource, ""},
		{auth.PermissionRequestReports, ""},
		{auth.Permission("something-new"), ""},
	}

	for _, check := range permissions {
		t.Run(string(check.permission), func(t *testing.T) {
			err := authenticator.Authorise(t.Context(), agent, check.permission, check.target)
			if !errors.Is(err, core.ErrUnauthorised) {
				t.Errorf("Authorise(%s) = %v, want ErrUnauthorised", check.permission, err)
			}
		})
	}
}

func TestAuthoriseGrantsWhatWasGiven(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t)
	agent := registerAgent(t, store, "claude-a", core.Capabilities{
		Resources: []string{"test"}, CanBroadcast: true,
	})

	if err := authenticator.Authorise(t.Context(), agent, auth.PermissionLease, "test"); err != nil {
		t.Errorf("Authorise(lease, test) = %v, want nil", err)
	}
	if err := authenticator.Authorise(t.Context(), agent, auth.PermissionBroadcast, ""); err != nil {
		t.Errorf("Authorise(broadcast) = %v, want nil", err)
	}
	if err := authenticator.Authorise(t.Context(), agent, auth.PermissionLease, "reviewer"); err == nil {
		t.Error("Authorise(lease, reviewer) = nil, want a refusal for a resource that was not granted")
	}
}

// The operator needs to be able to see who was refused what, after the fact.
func TestDenialsAreAudited(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t)
	agent := registerAgent(t, store, "claude-a", core.Capabilities{})

	if err := authenticator.Authorise(t.Context(), agent, auth.PermissionLease, "test"); err == nil {
		t.Fatal("the denial did not happen")
	}

	entries, err := store.Audit().List(t.Context(), core.AuditFilter{
		Actions: []core.AuditAction{core.AuditAuthDenied},
	})
	if err != nil {
		t.Fatalf("List() = %v, want nil", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the audit log holds %d denials, want 1", len(entries))
	}
	if entries[0].Actor != agent.ID || entries[0].Target != "test" {
		t.Errorf("audit entry = %+v, want the agent and the resource it was refused", entries[0])
	}
}

// A token in a log is a token somebody can use, so anything printed must be unusable.
func TestRedact(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t)
	agent := registerAgent(t, store, "claude-a", core.Capabilities{})

	token, err := authenticator.Issue(t.Context(), agent.ID)
	if err != nil {
		t.Fatalf("Issue() = %v, want nil", err)
	}

	redacted := auth.Redact(token)
	if strings.Contains(token, redacted) && len(redacted) >= len(token) {
		t.Errorf("Redact(%q) = %q, which is not shorter than the token", token, redacted)
	}
	if _, err := authenticator.Authenticate(t.Context(), redacted); !errors.Is(err, core.ErrUnauthorised) {
		t.Error("the redacted form authenticates")
	}
}

// The secret keys every token hash. A key another user can read is no protection at all.
func TestLoadOrCreateSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")

	first, err := auth.LoadOrCreateSecret(path)
	if err != nil {
		t.Fatalf("LoadOrCreateSecret() = %v, want nil", err)
	}
	if len(first) != 32 {
		t.Errorf("the secret is %d bytes, want 32", len(first))
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		t.Errorf("the secret file is mode %#o, want 0600", permissions)
	}

	// It must be stable: regenerating it would invalidate every agent's token on restart.
	second, err := auth.LoadOrCreateSecret(path)
	if err != nil {
		t.Fatalf("a second LoadOrCreateSecret() = %v, want nil", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("the secret changed between reads")
	}
}

func TestLoadOrCreateSecretRejectsALooseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")

	if _, err := auth.LoadOrCreateSecret(path); err != nil {
		t.Fatalf("LoadOrCreateSecret() = %v, want nil", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("relaxing the permissions: %v", err)
	}

	_, err := auth.LoadOrCreateSecret(path)
	if err == nil {
		t.Fatal("a world-readable secret was accepted")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("the error does not tell the operator how to fix it: %v", err)
	}
}

func TestLoadOrCreateSecretRejectsATruncatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	if err := os.WriteFile(path, []byte("too short"), 0o600); err != nil {
		t.Fatalf("writing the file: %v", err)
	}

	if _, err := auth.LoadOrCreateSecret(path); err == nil {
		t.Error("a truncated secret was accepted")
	}
}

// Two installs must not share a key by accident: a token from one must not work on the other.
func TestSecretsAreUniquePerInstall(t *testing.T) {
	first, err := auth.LoadOrCreateSecret(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatalf("LoadOrCreateSecret() = %v, want nil", err)
	}
	second, err := auth.LoadOrCreateSecret(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatalf("LoadOrCreateSecret() = %v, want nil", err)
	}

	if bytes.Equal(first, second) {
		t.Error("two installs generated the same secret")
	}
}

func TestNewRejectsAWrongSizedSecret(t *testing.T) {
	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "lac.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	defer func() { _ = store.Close() }()

	if _, err := auth.New(store, []byte("short"), auth.Options{}); err == nil {
		t.Error("New() accepted a short secret")
	}
}
