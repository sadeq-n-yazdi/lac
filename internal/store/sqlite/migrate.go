package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migration is one numbered schema step. Steps are applied in order, each in its own transaction,
// and never re-applied.
type migration struct {
	version   int
	name      string
	statement string
}

// migrate brings the database up to the schema this binary expects.
//
// A database already at a higher version than this binary knows about is refused rather than used:
// an older daemon writing through a newer schema is how data gets quietly corrupted.
func migrate(ctx context.Context, database *sql.DB) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	if _, err := database.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		    version    INTEGER PRIMARY KEY,
		    name       TEXT    NOT NULL,
		    applied_at INTEGER NOT NULL
		) STRICT`); err != nil {
		return fmt.Errorf("creating the migration table: %w", err)
	}

	applied, err := appliedVersions(ctx, database)
	if err != nil {
		return err
	}

	known := migrations[len(migrations)-1].version
	if highest := highestVersion(applied); highest > known {
		return fmt.Errorf("the database is at schema version %d but this build only knows version %d; "+
			"upgrade lac rather than running an older daemon against a newer database", highest, known)
	}

	for _, step := range migrations {
		if _, done := applied[step.version]; done {
			continue
		}
		if err := applyMigration(ctx, database, step); err != nil {
			return err
		}
	}

	return nil
}

// applyMigration runs one step and records it, atomically, so a crash mid-migration cannot leave
// the database half-way through a schema change.
func applyMigration(ctx context.Context, database *sql.DB, step migration) error {
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning migration %d: %w", step.version, err)
	}
	defer func() { _ = transaction.Rollback() }()

	if _, err := transaction.ExecContext(ctx, step.statement); err != nil {
		return fmt.Errorf("applying migration %d (%s): %w", step.version, step.name, err)
	}

	if _, err := transaction.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		step.version, step.name, time.Now().UTC().UnixMicro(),
	); err != nil {
		return fmt.Errorf("recording migration %d: %w", step.version, err)
	}

	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("committing migration %d: %w", step.version, err)
	}

	return nil
}

func appliedVersions(ctx context.Context, database *sql.DB) (map[int]struct{}, error) {
	rows, err := database.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("reading applied migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[int]struct{})
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scanning an applied migration: %w", err)
		}
		applied[version] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading applied migrations: %w", err)
	}

	return applied, nil
}

func highestVersion(versions map[int]struct{}) int {
	highest := 0
	for version := range versions {
		highest = max(highest, version)
	}
	return highest
}

// loadMigrations reads the embedded steps, in version order. File names are "0001_init.sql":
// a zero-padded version, an underscore, and a name.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("reading the embedded migrations: %w", err)
	}

	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		step, err := parseMigrationName(entry.Name())
		if err != nil {
			return nil, err
		}

		contents, err := migrationFiles.ReadFile(path.Join("migrations", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading migration %s: %w", entry.Name(), err)
		}
		step.statement = string(contents)

		migrations = append(migrations, step)
	}

	if len(migrations) == 0 {
		return nil, errors.New("no migrations are embedded in this build")
	}

	slices.SortFunc(migrations, func(a, b migration) int { return a.version - b.version })

	for index := 1; index < len(migrations); index++ {
		if migrations[index].version == migrations[index-1].version {
			return nil, fmt.Errorf("two migrations share version %d", migrations[index].version)
		}
	}

	return migrations, nil
}

func parseMigrationName(fileName string) (migration, error) {
	base := strings.TrimSuffix(fileName, ".sql")

	prefix, name, found := strings.Cut(base, "_")
	if !found {
		return migration{}, fmt.Errorf("migration %q is not named <version>_<name>.sql", fileName)
	}

	version, err := strconv.Atoi(prefix)
	if err != nil || version < 1 {
		return migration{}, fmt.Errorf("migration %q does not start with a positive version number", fileName)
	}

	return migration{version: version, name: name}, nil
}
