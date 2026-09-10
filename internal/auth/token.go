package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

// TokenPrefix marks a LAC token, so one found in a log or a shell history is recognisable and can
// be revoked rather than puzzled over.
const TokenPrefix = "lac_"

// tokenBytes is the amount of randomness in a token. Two hundred and fifty-six bits is far beyond
// guessing, and the token is only ever typed by a program.
const tokenBytes = 32

// tokenEncoding is URL and shell safe, so a token can go in an environment variable unquoted.
var tokenEncoding = base64.RawURLEncoding

// newToken returns a fresh token. It exists exactly once, in the reply to registration; only its
// hash is ever stored.
func newToken() (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a token: %w", err)
	}

	return TokenPrefix + tokenEncoding.EncodeToString(raw), nil
}

// hashToken computes the keyed hash stored for a token.
//
// It is keyed rather than a bare digest on purpose: a bare SHA-256 of a token would let anyone who
// copied the database test candidate tokens offline at full speed.
func hashToken(secret []byte, token string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(token))

	return mac.Sum(nil)
}

// looksLikeToken reports whether the value has the shape of a LAC token. It rejects obvious
// rubbish before a database lookup, and never says anything about a real token's validity.
func looksLikeToken(token string) bool {
	if !strings.HasPrefix(token, TokenPrefix) {
		return false
	}

	decoded, err := tokenEncoding.DecodeString(strings.TrimPrefix(token, TokenPrefix))

	return err == nil && len(decoded) == tokenBytes
}

// Redact turns a token into something safe to log: enough to tell two tokens apart, not enough to
// use one.
func Redact(token string) string {
	if len(token) <= len(TokenPrefix)+6 {
		return TokenPrefix + "…"
	}

	return token[:len(TokenPrefix)+6] + "…"
}
