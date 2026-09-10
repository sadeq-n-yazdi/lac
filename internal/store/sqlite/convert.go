package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"code.sadeq.uk/lac/internal/core"
)

// Instants are stored as microseconds since the Unix epoch, UTC. SQLite has no time type, and a
// fixed integer keeps ordering, indexing and comparison unambiguous.

// toMicros converts an instant for storage. The zero time becomes NULL, meaning "has not happened".
func toMicros(instant time.Time) sql.NullInt64 {
	if instant.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: instant.UTC().UnixMicro(), Valid: true}
}

// requireMicros converts an instant for a column that is NOT NULL.
func requireMicros(instant time.Time) int64 { return instant.UTC().UnixMicro() }

// fromMicros converts a stored instant back. NULL becomes the zero time.
func fromMicros(stored sql.NullInt64) time.Time {
	if !stored.Valid {
		return time.Time{}
	}
	return time.UnixMicro(stored.Int64).UTC()
}

// fromRequiredMicros converts a stored NOT NULL instant back.
func fromRequiredMicros(stored int64) time.Time { return time.UnixMicro(stored).UTC() }

// nullString stores an empty string as NULL, which is how "no topic" and "no recipient" are
// represented so the schema's exactly-one-destination check can enforce them.
func nullString(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

func fromNullString(stored sql.NullString) string {
	if !stored.Valid {
		return ""
	}
	return stored.String
}

// translateError maps a driver error onto a domain error, so services and transports never have to
// know what database is underneath.
func translateError(operation string, err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", operation, core.ErrNotFound)
	}

	message := err.Error()
	switch {
	case strings.Contains(message, "UNIQUE constraint failed"):
		return fmt.Errorf("%s: %w", operation, core.ErrAlreadyExists)
	case strings.Contains(message, "FOREIGN KEY constraint failed"):
		return fmt.Errorf("%s: %w: it refers to something that does not exist", operation, core.ErrInvalidArgument)
	case strings.Contains(message, "CHECK constraint failed"):
		return fmt.Errorf("%s: %w: %s", operation, core.ErrInvalidArgument, message)
	}

	return fmt.Errorf("%s: %w", operation, err)
}

// affectedOrNotFound turns an update that matched nothing into ErrNotFound, so callers do not have
// to inspect a driver-specific result.
func affectedOrNotFound(operation string, result sql.Result, err error) error {
	if err != nil {
		return translateError(operation, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: reading the affected row count: %w", operation, err)
	}
	if affected == 0 {
		return fmt.Errorf("%s: %w", operation, core.ErrNotFound)
	}

	return nil
}

// placeholders builds "?, ?, ?" for an IN clause of the given size.
func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

// prefixed qualifies a comma-separated column list with a table alias, so a join can reuse the
// same column constant as a plain select.
func prefixed(alias, columns string) string {
	parts := strings.Split(columns, ", ")
	for index, column := range parts {
		parts[index] = alias + "." + column
	}

	return strings.Join(parts, ", ")
}
