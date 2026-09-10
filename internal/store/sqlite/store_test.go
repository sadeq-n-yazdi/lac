package sqlite_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"code.sadeq.uk/lac/internal/core"
	"code.sadeq.uk/lac/internal/id"
	"code.sadeq.uk/lac/internal/store/sqlite"
)

// baseTime is a fixed instant, so tests never depend on the wall clock.
var baseTime = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

// openStore returns a store backed by a real file rather than an in-memory database, so WAL mode,
// file permissions and the busy timeout are all genuinely exercised.
func openStore(t *testing.T) *sqlite.Store {
	t.Helper()

	path := filepath.Join(t.TempDir(), "lac.db")
	store, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open(%q) = %v, want nil", path, err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() = %v", err)
		}
	})

	return store
}

func newAgent(t *testing.T, store *sqlite.Store, name string) core.Agent {
	t.Helper()

	agent := core.Agent{
		ID:              id.New("agent"),
		Name:            name,
		Kind:            "claude",
		Workdir:         "/tmp/" + name,
		ProcessID:       4242,
		Capabilities:    core.Capabilities{Resources: []string{"test"}, CanBroadcast: true},
		State:           core.AgentActive,
		RegisteredAt:    baseTime,
		LastHeartbeatAt: baseTime,
	}

	if err := store.Agents().Create(t.Context(), agent); err != nil {
		t.Fatalf("Create(%q) = %v, want nil", name, err)
	}

	return agent
}

func defineResource(t *testing.T, store *sqlite.Store, name string, capacity int) core.Resource {
	t.Helper()

	resource := core.Resource{
		Name:            name,
		Capacity:        capacity,
		LeaseTimeToLive: 15 * time.Minute,
		Description:     "test fixture",
		CreatedAt:       baseTime,
	}

	if err := store.Resources().Define(t.Context(), resource); err != nil {
		t.Fatalf("Define(%q) = %v, want nil", name, err)
	}

	return resource
}

// The store's correctness rests on settings the driver could silently ignore, so Open verifies
// them rather than assuming.
func TestOpenAppliesPragmas(t *testing.T) {
	store := openStore(t)

	// Foreign keys being enforced is what stops a lease outliving its agent.
	err := store.Leases().Enqueue(t.Context(), core.QueueEntry{
		ID: id.New("queue"), ResourceName: "absent", AgentID: "agent_absent",
		State: core.QueueWaiting, RequestedAt: baseTime,
	})
	if !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("queueing against a missing resource = %v, want ErrInvalidArgument", err)
	}
}

// The database holds every message the agents exchanged; another user must not be able to read it.
func TestOpenCreatesAPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lac.db")

	store, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open() = %v, want nil", err)
	}
	defer func() { _ = store.Close() }()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		t.Errorf("the database is mode %#o, want 0600", permissions)
	}
}

func TestOpenRejectsRelativePaths(t *testing.T) {
	_, err := sqlite.Open(t.Context(), "lac.db")

	if !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("Open(%q) = %v, want ErrInvalidArgument", "lac.db", err)
	}
}

// Migrations must be safe to run against an existing database: the daemon applies them on every
// start.
func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lac.db")

	first, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("first Open() = %v, want nil", err)
	}
	agent := newAgent(t, first, "claude-a")
	if err := first.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}

	second, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("second Open() = %v, want nil", err)
	}
	defer func() { _ = second.Close() }()

	stored, err := second.Agents().ByID(t.Context(), agent.ID)
	if err != nil {
		t.Fatalf("ByID() after reopening = %v, want nil", err)
	}
	if stored.Name != agent.Name {
		t.Errorf("the reopened database lost data: name = %q, want %q", stored.Name, agent.Name)
	}
}

// An older daemon writing through a schema it does not understand is how data gets quietly
// corrupted, so it must refuse to start.
func TestOpenRefusesANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lac.db")

	store, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open() = %v, want nil", err)
	}
	if err := sqlite.RecordFutureMigrationForTest(t.Context(), store, 9999); err != nil {
		t.Fatalf("recording a future migration: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}

	_, err = sqlite.Open(t.Context(), path)
	if err == nil {
		t.Fatal("Open() against a newer schema = nil, want an error")
	}
}

// A rolled-back transaction must leave nothing behind: the lease grant depends on all-or-nothing.
func TestInTransactionRollsBack(t *testing.T) {
	store := openStore(t)
	sentinel := errors.New("deliberate failure")

	err := store.InTransaction(t.Context(), func(tx core.Store) error {
		if err := tx.Resources().Define(t.Context(), core.Resource{
			Name: "test", Capacity: 4, LeaseTimeToLive: time.Minute, CreatedAt: baseTime,
		}); err != nil {
			return err
		}
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("InTransaction() = %v, want the sentinel error", err)
	}

	if _, err := store.Resources().ByName(t.Context(), "test"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("the rolled back resource is still there: %v", err)
	}
}

func TestInTransactionCommits(t *testing.T) {
	store := openStore(t)

	err := store.InTransaction(t.Context(), func(tx core.Store) error {
		return tx.Resources().Define(t.Context(), core.Resource{
			Name: "test", Capacity: 4, LeaseTimeToLive: time.Minute, CreatedAt: baseTime,
		})
	})
	if err != nil {
		t.Fatalf("InTransaction() = %v, want nil", err)
	}

	if _, err := store.Resources().ByName(t.Context(), "test"); err != nil {
		t.Errorf("ByName() after commit = %v, want nil", err)
	}
}

// A panic inside a transaction must not commit half a change, and must still reach the caller.
func TestInTransactionRollsBackOnPanic(t *testing.T) {
	store := openStore(t)

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Error("the panic did not propagate to the caller")
			}
		}()

		_ = store.InTransaction(t.Context(), func(tx core.Store) error {
			if err := tx.Resources().Define(t.Context(), core.Resource{
				Name: "test", Capacity: 4, LeaseTimeToLive: time.Minute, CreatedAt: baseTime,
			}); err != nil {
				t.Errorf("Define() = %v, want nil", err)
			}
			panic("something went badly wrong")
		})
	}()

	if _, err := store.Resources().ByName(t.Context(), "test"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("the resource survived a panicking transaction: %v", err)
	}
}

// A service that already holds a transaction must be able to call a helper that wants one, without
// deadlocking on the single write connection.
func TestInTransactionNestsWithoutDeadlock(t *testing.T) {
	store := openStore(t)
	done := make(chan error, 1)

	go func() {
		done <- store.InTransaction(t.Context(), func(outer core.Store) error {
			return outer.InTransaction(t.Context(), func(inner core.Store) error {
				return inner.Resources().Define(t.Context(), core.Resource{
					Name: "test", Capacity: 1, LeaseTimeToLive: time.Minute, CreatedAt: baseTime,
				})
			})
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("nested InTransaction() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nested InTransaction() deadlocked")
	}

	if _, err := store.Resources().ByName(t.Context(), "test"); err != nil {
		t.Errorf("ByName() = %v, want nil", err)
	}
}

func TestContextCancellationIsHonoured(t *testing.T) {
	store := openStore(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := store.Agents().List(ctx, core.AgentFilter{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("List() with a cancelled context = %v, want context.Canceled", err)
	}
}
