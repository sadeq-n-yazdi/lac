package id

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// Identifiers become database keys and appear in logs, so a collision would silently attach one
// agent's lease to another's record.
func TestNewIsUnique(t *testing.T) {
	const count = 10_000

	seen := make(map[string]struct{}, count)

	for range count {
		value := New("agent")

		if _, duplicate := seen[value]; duplicate {
			t.Fatalf("New produced the duplicate identifier %q", value)
		}
		seen[value] = struct{}{}

		if !strings.HasPrefix(value, "agent_") {
			t.Fatalf("New(%q) = %q, want the kind as a prefix", "agent", value)
		}
	}
}

// Queue displays and log searches read better when identifiers sort by creation time, and the
// database index stays compact because inserts append rather than scatter.
func TestNewSortsByCreationTime(t *testing.T) {
	instant := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	now = func() time.Time { return instant }
	t.Cleanup(func() { now = time.Now })

	identifiers := make([]string, 0, 100)
	for range 100 {
		identifiers = append(identifiers, New("lease"))
		instant = instant.Add(time.Millisecond)
	}

	if !sort.StringsAreSorted(identifiers) {
		t.Error("identifiers created in ascending time order do not sort lexicographically")
	}
}

func TestNewLengthIsStable(t *testing.T) {
	first, second := New("lease"), New("lease")

	if len(first) != len(second) {
		t.Errorf("identifier lengths differ: %d and %d", len(first), len(second))
	}
}
