package sqlite

import (
	"context"
	"fmt"
	"time"
)

// RecordFutureMigrationForTest claims a schema version this build does not know about, so tests
// can check that the daemon refuses to run against a database written by a newer version.
func RecordFutureMigrationForTest(ctx context.Context, store *Store, version int) error {
	_, err := store.database.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		version, "from-the-future", time.Now().UTC().UnixMicro(),
	)
	if err != nil {
		return fmt.Errorf("recording migration %d: %w", version, err)
	}

	return nil
}
