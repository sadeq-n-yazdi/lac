package core

import (
	"fmt"
	"time"
)

// Watch is a pull request LAC is keeping an eye on.
//
// Snapshot holds the last thing GitHub actually said, in full. Everything beside it is copied out
// for listing and for noticing changes, and Stale says whether any of it can still be trusted.
type Watch struct {
	// ID is assigned by the daemon.
	ID string
	// Owner, Repository and Number identify the pull request.
	Owner      string
	Repository string
	Number     int

	// Account is the GitHub login that can see this repository, remembered from the first read that
	// worked. On a machine with more than one login, gh does not choose by directory, so the one
	// that can see a given repository has to be found and kept.
	Account string

	// Snapshot is the last good observation, as JSON. Empty before the first successful poll.
	Snapshot []byte

	// Title, State, Draft, ChecksState and Unresolved are the parts worth showing in a list.
	Title       string
	State       string
	Draft       bool
	ChecksState string
	Unresolved  int

	// ObservedAt is when GitHub last answered. Zero means it never has.
	ObservedAt time.Time
	// Failures counts consecutive failures, which is what the backoff is worked out from.
	Failures int
	// LastError is why the most recent attempt failed, for an operator to read.
	LastError   string
	LastErrorAt time.Time
	// NextPollAt is when to try again.
	NextPollAt time.Time
	CreatedAt  time.Time
}

// Reference is how a person names the pull request: owner/repository#number.
func (w Watch) Reference() string {
	return fmt.Sprintf("%s/%s#%d", w.Owner, w.Repository, w.Number)
}

// Observed reports whether GitHub has ever answered about this pull request. Until it has, there is
// nothing to show but the reference.
func (w Watch) Observed() bool { return !w.ObservedAt.IsZero() }

// Stale reports whether what we know is older than it should be — the last attempt failed, or the
// last success is older than the given age. An agent acting on stale information should know it is
// doing so.
func (w Watch) Stale(now time.Time, freshFor time.Duration) bool {
	if !w.Observed() {
		return true
	}

	return w.Failures > 0 || now.Sub(w.ObservedAt) > freshFor
}

// Validate reports whether the watch is well formed enough to store.
func (w Watch) Validate() error {
	if w.ID == "" {
		return fmt.Errorf("%w: watch id is required", ErrInvalidArgument)
	}
	if w.Owner == "" || w.Repository == "" {
		return fmt.Errorf("%w: a watch needs an owner and a repository", ErrInvalidArgument)
	}
	if w.Number <= 0 {
		return fmt.Errorf("%w: a pull request number must be positive, got %d", ErrInvalidArgument, w.Number)
	}

	return nil
}

// WatchEventKind says what sort of change happened, so an agent can route on it rather than reading
// English.
type WatchEventKind string

// The changes worth telling somebody about.
const (
	// WatchChecks means CI finished, started, or changed its mind.
	WatchChecks WatchEventKind = "checks"
	// WatchState means the pull request opened, closed, merged, or became a draft.
	WatchState WatchEventKind = "state"
	// WatchComment means somebody said something on the pull request.
	WatchComment WatchEventKind = "comment"
	// WatchReview means a review was submitted.
	WatchReview WatchEventKind = "review"
	// WatchThread means a review conversation appeared, gained a reply, or was resolved.
	WatchThread WatchEventKind = "thread"
	// WatchCommits means the branch was pushed to.
	WatchCommits WatchEventKind = "commits"
	// WatchUnreachable means GitHub could not be reached. It is recorded so that a long silence is
	// explicable afterwards.
	WatchUnreachable WatchEventKind = "unreachable"
	// WatchRecovered means GitHub answered again after a run of failures.
	WatchRecovered WatchEventKind = "recovered"
)

// WatchEvent is one change to a watched pull request.
type WatchEvent struct {
	// ID increases monotonically, so an agent can ask for everything after the last one it saw.
	ID      int64
	WatchID string
	Kind    WatchEventKind
	// Summary is one line, written for a person.
	Summary string
	At      time.Time
}
