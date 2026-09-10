// Package auth turns a token into an identity and decides what that identity may do.
//
// Tokens are never stored. What is stored is a keyed hash of the token, so a copy of the database
// alone does not let anyone impersonate an agent, and an offline guessing attack has to steal the
// install's secret key as well.
package auth

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// secretSize is the length of the per-install key. Thirty-two bytes is the block size of the hash
// it keys, which is what SHA-256 HMAC is defined for.
const secretSize = 32

// secretFileMode keeps the key readable only by its owner.
const secretFileMode os.FileMode = 0o600

// LoadOrCreateSecret returns the per-install key used to hash tokens, creating it on first use.
//
// The key is what makes a stored hash useless to anyone who only has the database. If it is
// readable by another user, that protection is gone, so a loose file is refused rather than used.
func LoadOrCreateSecret(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("the secret path %q must be absolute", path)
	}

	secret, err := os.ReadFile(path) //nolint:gosec // the daemon's own state directory
	switch {
	case err == nil:
		return validateSecret(path, secret)
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	return createSecret(path)
}

func validateSecret(path string, secret []byte) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspecting %s: %w", path, err)
	}
	if permissions := info.Mode().Perm(); permissions&0o077 != 0 {
		return nil, fmt.Errorf("%s is mode %#o; it keys every agent token and must not be readable "+
			"by other users. Run: chmod 600 %s", path, permissions, path)
	}
	if len(secret) != secretSize {
		return nil, fmt.Errorf("%s is %d bytes, want %d; delete it to have a new key generated, "+
			"which invalidates every agent token", path, len(secret), secretSize)
	}

	return secret, nil
}

// createSecret writes a new key, refusing to overwrite one that appeared in the meantime.
func createSecret(path string) ([]byte, error) {
	secret := make([]byte, secretSize)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generating a secret: %w", err)
	}

	// O_EXCL, so a key that appeared since the read above is never overwritten: doing so would
	// invalidate every token issued by the daemon that wrote it.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, secretFileMode) //nolint:gosec // the daemon's own state directory
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	if _, err := file.Write(secret); err != nil {
		return nil, fmt.Errorf("writing %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("flushing %s: %w", path, err)
	}

	return secret, nil
}
