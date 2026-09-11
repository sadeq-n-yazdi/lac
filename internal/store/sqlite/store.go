// Package sqlite implements LAC's storage contracts on top of SQLite.
//
// The database is opened in WAL mode with foreign keys enforced, and every write runs in an
// immediate transaction. That is what lets the leasing service read the current slot count and
// insert a lease as one atomic step, which is the guarantee the whole coordinator rests on.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // the pure-Go SQLite driver; no cgo, so `go install` needs no toolchain

	"sadeq.uk/lac/internal/core"
)

// fileMode is the mode of the database file. It holds every message the agents exchanged, so no
// other user may read it.
const fileMode os.FileMode = 0o600

// busyTimeout is how long a statement waits for the write lock before giving up. Writes here are
// microseconds long, so anything approaching this means a genuine problem rather than contention.
const busyTimeout = "5000"

// querier is the subset of database/sql shared by *sql.DB, *sql.Conn and *sql.Tx, so a repository
// works the same whether or not it is running inside a transaction.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store is the SQLite implementation of core.Store.
type Store struct {
	database *sql.DB
	// queries is the database itself, or the transaction when this Store is transaction-bound.
	queries querier
	// inTransaction says whether this Store already runs inside one, so InTransaction does not
	// try to open a second.
	inTransaction bool
}

// compile-time proof that the contract is satisfied.
var _ core.Store = (*Store)(nil)

// Open prepares the database at path, creating and migrating it if necessary.
//
// The file is created with owner-only permissions before SQLite sees it, so there is no window in
// which another user could read it.
func Open(ctx context.Context, path string) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%w: the database path %q must be absolute", core.ErrInvalidArgument, path)
	}

	if err := createPrivateFile(path); err != nil {
		return nil, err
	}

	database, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	// SQLite serialises writers anyway, and LAC's statements are short. One connection removes
	// every chance of a busy error or a lost update, at no cost this workload can measure.
	database.SetMaxOpenConns(1)

	store := &Store{database: database, queries: database}

	if err := store.verifyPragmas(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	if err := migrate(ctx, database); err != nil {
		_ = database.Close()
		return nil, err
	}

	return store, nil
}

// dataSourceName builds the driver connection string.
//
// _txlock=immediate makes every BeginTx take the write lock at once rather than upgrading on the
// first write. Without it a read-then-write transaction can fail to upgrade and lose its snapshot,
// which is exactly the shape of the lease grant.
func dataSourceName(path string) string {
	pragmas := []string{
		"_pragma=busy_timeout(" + busyTimeout + ")",
		"_pragma=journal_mode(WAL)",
		"_pragma=foreign_keys(1)",
		"_pragma=synchronous(1)", // NORMAL: durable enough in WAL, and far fewer fsyncs
		"_txlock=immediate",
	}

	return "file:" + path + "?" + strings.Join(pragmas, "&")
}

// createPrivateFile makes sure the database file exists with owner-only permissions before the
// driver opens it, and tightens an existing file that is too permissive.
func createPrivateFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, fileMode) //nolint:gosec // operator-owned path
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}

	if err := os.Chmod(path, fileMode); err != nil {
		return fmt.Errorf("tightening permissions on %s: %w", path, err)
	}

	return nil
}

// verifyPragmas confirms the connection really has the settings the correctness of the store
// depends on. A driver that silently ignored them would leave foreign keys unenforced.
func (s *Store) verifyPragmas(ctx context.Context) error {
	var journalMode string
	if err := s.database.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return fmt.Errorf("reading journal_mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("journal_mode is %q, want wal", journalMode)
	}

	var foreignKeys int
	if err := s.database.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return fmt.Errorf("reading foreign_keys: %w", err)
	}
	if foreignKeys != 1 {
		return errors.New("foreign_keys is off; referential integrity would not be enforced")
	}

	return nil
}

// InTransaction runs fn inside one immediate write transaction, committing when it returns nil and
// rolling back on any error or panic. A Store that is already transaction-bound simply runs fn, so
// a service can call a helper that also wants a transaction without deadlocking on itself.
func (s *Store) InTransaction(ctx context.Context, fn func(tx core.Store) error) error {
	if s.inTransaction {
		return fn(s)
	}

	transaction, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}

	bound := &Store{database: s.database, queries: transaction, inTransaction: true}

	defer func() {
		if recovered := recover(); recovered != nil {
			_ = transaction.Rollback()
			panic(recovered)
		}
	}()

	if err := fn(bound); err != nil {
		if rollbackErr := transaction.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("rolling back: %w", rollbackErr))
		}
		return err
	}

	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("committing: %w", err)
	}

	return nil
}

// Close releases the database.
func (s *Store) Close() error {
	if err := s.database.Close(); err != nil {
		return fmt.Errorf("closing the database: %w", err)
	}
	return nil
}

// Agents returns the agent repository.
func (s *Store) Agents() core.AgentRepository { return agentRepository{s.queries} }

// Credentials returns the credential repository.
func (s *Store) Credentials() core.CredentialRepository { return credentialRepository{s.queries} }

// Messages returns the message repository.
func (s *Store) Messages() core.MessageRepository { return messageRepository{s.queries} }

// Resources returns the resource repository.
func (s *Store) Resources() core.ResourceRepository { return resourceRepository{s.queries} }

// Leases returns the queue and lease repository.
func (s *Store) Leases() core.LeaseRepository { return leaseRepository{s.queries} }

// Reports returns the report repository.
func (s *Store) Reports() core.ReportRepository { return reportRepository{s.queries} }

// Tasks returns the dispatched-work repository.
func (s *Store) Tasks() core.TaskRepository { return taskRepository{s.queries} }

// Audit returns the append-only audit log.
func (s *Store) Audit() core.AuditLog { return auditLog{s.queries} }
