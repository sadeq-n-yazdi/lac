// Package prwatch keeps an eye on pull requests and tells the agents that care when something
// happens: CI finishing, a status changing, a comment arriving, a review conversation being
// resolved.
//
// It reads GitHub through the gh command line, so LAC holds no credential of its own. It polls,
// because the alternative is an inbound webhook and LAC has no listener by design.
//
// Two things make it worth relying on during a bad afternoon at GitHub. The last good observation
// is kept in full, so an agent asking what is happening gets the last thing that was true, marked
// as old, rather than an error. And repeated failures back off rather than hammering: a watcher
// that turns an outage into a rate limit has made things worse.
package prwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"sadeq.uk/lac/internal/auth"
	"sadeq.uk/lac/internal/core"
	"sadeq.uk/lac/internal/github"
	"sadeq.uk/lac/internal/id"
	"sadeq.uk/lac/internal/service/messaging"
)

// MessageKind is the kind given to the message a subscriber receives, so an agent can route on it.
const MessageKind = "pr-update"

// AgentName is how the watcher appears on the roster, so a change arrives from something with a
// name rather than from the agent to itself.
const AgentName = "pr-watcher"

// Default timings.
const (
	// DefaultInterval is how often an open pull request is looked at. Often enough that CI finishing
	// is noticed while somebody still cares, rare enough to be unremarkable against a rate limit.
	DefaultInterval = time.Minute
	// DefaultSettledInterval is how often a merged or closed pull request is looked at. Rarely:
	// it is finished, and the watch is kept only so the history stays readable.
	DefaultSettledInterval = 30 * time.Minute
	// DefaultFreshFor is how long an observation counts as current when reporting staleness.
	DefaultFreshFor = 5 * time.Minute
	// maxBackoff caps the wait after repeated failures. Long enough to sit out an outage, short
	// enough that recovery is noticed within the hour.
	maxBackoff = 15 * time.Minute
	// pollBatch is how many watches are refreshed in one sweep, so a long list does not become one
	// enormous burst of requests.
	pollBatch = 10
)

// Options configure the service.
type Options struct {
	// Interval is how often an open pull request is polled. Zero means a minute.
	Interval time.Duration
	// SettledInterval is how often a merged or closed one is polled. Zero means half an hour.
	SettledInterval time.Duration
	// FreshFor is how long an observation counts as current. Zero means five minutes.
	FreshFor time.Duration
	// Clock defaults to the wall clock in UTC.
	Clock auth.Clock
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Forge is the part of GitHub this service needs. It is an interface so the service can be tested
// without a network, and so a second forge could be added without touching the watching itself.
type Forge interface {
	// PullRequest reads one pull request as the given account, or as whichever is active when it is
	// empty.
	PullRequest(ctx context.Context, account, owner, repository string, number int) (github.PullRequest, error)
	// Accounts lists the logins available, the active one first.
	Accounts(ctx context.Context) ([]string, error)
	// Available reports whether the forge can be reached at all.
	Available(ctx context.Context) error
}

// Service watches pull requests.
type Service struct {
	store     core.Store
	forge     Forge
	messaging *messaging.Service
	registry  agentLookup
	options   Options
	now       auth.Clock
	logger    *slog.Logger

	// senderID is the watcher's own agent, set once it has registered.
	senderMutex sync.RWMutex
	senderID    string

	// refreshed wakes anybody waiting on a particular watch being polled.
	refreshed *signals
	// polling serialises the sweeps, so a manual refresh and the background loop cannot both be
	// reading and writing one watch at the same time.
	polling sync.Mutex
}

// agentLookup resolves an agent id to a name, for the messages subscribers receive.
type agentLookup interface {
	ByID(ctx context.Context, agentID string) (core.Agent, error)
}

// New returns a watcher.
func New(
	store core.Store, forge Forge, messagingService *messaging.Service, agents agentLookup, options Options,
) *Service {
	if options.Interval <= 0 {
		options.Interval = DefaultInterval
	}
	if options.SettledInterval <= 0 {
		options.SettledInterval = DefaultSettledInterval
	}
	if options.FreshFor <= 0 {
		options.FreshFor = DefaultFreshFor
	}
	if options.Clock == nil {
		options.Clock = func() time.Time { return time.Now().UTC() }
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}

	return &Service{
		store:     store,
		forge:     forge,
		messaging: messagingService,
		registry:  agents,
		options:   options,
		now:       options.Clock,
		logger:    options.Logger,
		refreshed: newSignals(),
	}
}

// Watch starts watching a pull request for an agent, or subscribes it to one already watched.
//
// The first poll happens before returning, so the caller gets a real answer rather than an empty
// record it has to come back for. If GitHub cannot be reached the watch is still created — the
// pull request exists whether or not GitHub is answering this minute — and the failure is reported.
func (s *Service) Watch(
	ctx context.Context, agentID, owner, repository string, number int,
) (core.Watch, error) {
	now := s.now()

	watch, err := s.store.Watches().ByReference(ctx, owner, repository, number)
	switch {
	case errors.Is(err, core.ErrNotFound):
		watch = core.Watch{
			ID:          id.New("watch"),
			Owner:       owner,
			Repository:  repository,
			Number:      number,
			State:       "unknown",
			ChecksState: "unknown",
			NextPollAt:  now,
			CreatedAt:   now,
		}
		if err := s.store.Watches().Create(ctx, watch); err != nil {
			return core.Watch{}, err
		}

	case err != nil:
		return core.Watch{}, err
	}

	if err := s.store.Watches().Subscribe(ctx, watch.ID, agentID, now); err != nil {
		return core.Watch{}, err
	}

	// Look now, so the caller learns straight away whether the pull request is even there.
	refreshed, err := s.Refresh(ctx, watch.ID)
	if err != nil {
		if errors.Is(err, github.ErrNotFound) {
			// A reference nobody can see is a mistake worth undoing rather than retrying forever.
			if removeErr := s.store.Watches().Delete(ctx, watch.ID); removeErr != nil {
				s.logger.Warn("could not remove a watch on a pull request that is not there",
					"watch", watch.ID, "error", removeErr)
			}

			return core.Watch{}, err
		}

		// Anything else: keep the watch and let the poller carry on trying.
		s.logger.Warn("could not read a newly watched pull request",
			"reference", watch.Reference(), "error", err)

		return s.store.Watches().ByID(ctx, watch.ID)
	}

	return refreshed, nil
}

// SetSender records the watcher's own agent id, so the messages it sends come from something with a
// name. Until it is set, a change is delivered in the subscriber's own name, which still reaches
// them but reads oddly.
func (s *Service) SetSender(agentID string) {
	s.senderMutex.Lock()
	defer s.senderMutex.Unlock()

	s.senderID = agentID
}

func (s *Service) sender(fallback string) string {
	s.senderMutex.RLock()
	defer s.senderMutex.RUnlock()

	if s.senderID == "" {
		return fallback
	}

	return s.senderID
}

// Unwatch takes an agent off a watch. The watch itself is kept when others are still listening, and
// when nobody is: its history is worth more than the row it occupies, and it costs one poll an hour.
func (s *Service) Unwatch(ctx context.Context, agentID, watchID string) error {
	if err := s.store.Watches().Unsubscribe(ctx, watchID, agentID); err != nil {
		return err
	}

	return nil
}

// Forget removes a watch and everything attached to it, whoever was listening.
func (s *Service) Forget(ctx context.Context, watchID string) error {
	return s.store.Watches().Delete(ctx, watchID)
}

// Watch returns one watch by id.
func (s *Service) ByID(ctx context.Context, watchID string) (core.Watch, error) {
	return s.store.Watches().ByID(ctx, watchID)
}

// ByReference returns the watch on a pull request.
func (s *Service) ByReference(
	ctx context.Context, owner, repository string, number int,
) (core.Watch, error) {
	return s.store.Watches().ByReference(ctx, owner, repository, number)
}

// Mine returns the watches an agent subscribed to.
func (s *Service) Mine(ctx context.Context, agentID string) ([]core.Watch, error) {
	return s.store.Watches().ListForAgent(ctx, agentID)
}

// All returns every watch, for an operator looking at the whole machine.
func (s *Service) All(ctx context.Context) ([]core.Watch, error) {
	return s.store.Watches().List(ctx)
}

// Events returns what has changed on a watch, newest first.
func (s *Service) Events(ctx context.Context, watchID string, limit int) ([]core.WatchEvent, error) {
	return s.store.Watches().Events(ctx, watchID, limit)
}

// Subscribers returns who is listening to a watch.
func (s *Service) Subscribers(ctx context.Context, watchID string) ([]string, error) {
	return s.store.Watches().Subscribers(ctx, watchID)
}

// Snapshot decodes the last good observation. It reports whether there is one: before the first
// successful poll there is nothing to show but the reference.
func (s *Service) Snapshot(watch core.Watch) (github.PullRequest, bool) {
	if len(watch.Snapshot) == 0 {
		return github.PullRequest{}, false
	}

	var pullRequest github.PullRequest
	if err := json.Unmarshal(watch.Snapshot, &pullRequest); err != nil {
		s.logger.Warn("a stored observation could not be read",
			"watch", watch.ID, "error", err)

		return github.PullRequest{}, false
	}

	return pullRequest, true
}

// Stale reports whether what is known about a watch is older than it should be.
func (s *Service) Stale(watch core.Watch) bool {
	return watch.Stale(s.now(), s.options.FreshFor)
}

// Refresh reads a pull request now and records what changed.
//
// This is what an agent calls when it wants to know immediately rather than at the next sweep —
// after pushing a fix, say, when waiting for CI to start.
func (s *Service) Refresh(ctx context.Context, watchID string) (core.Watch, error) {
	s.polling.Lock()
	defer s.polling.Unlock()

	watch, err := s.store.Watches().ByID(ctx, watchID)
	if err != nil {
		return core.Watch{}, err
	}

	return s.poll(ctx, watch)
}

// poll reads one pull request and records the outcome, successful or not.
func (s *Service) poll(ctx context.Context, watch core.Watch) (core.Watch, error) {
	observation, account, err := s.read(ctx, watch)
	if err != nil {
		return watch, s.recordFailure(ctx, watch, err)
	}

	now := s.now()
	previous, hadPrevious := s.Snapshot(watch)

	encoded, err := json.Marshal(observation)
	if err != nil {
		return watch, fmt.Errorf("storing the observation of %s: %w", watch.Reference(), err)
	}

	updated := watch
	updated.Account = account
	updated.Snapshot = encoded
	updated.Title = observation.Title
	updated.State = observation.State
	updated.Draft = observation.Draft
	updated.ChecksState = observation.Checks.State
	updated.Unresolved = observation.UnresolvedThreads()
	updated.ObservedAt = now
	updated.Failures = 0
	updated.LastError = ""
	updated.NextPollAt = now.Add(s.intervalFor(observation))

	if err := s.store.Watches().RecordObservation(ctx, updated); err != nil {
		return watch, err
	}

	// A run of failures ending is worth saying, so a silence in the events has an explanation
	// either side of it.
	if watch.Failures > 0 {
		s.record(ctx, updated, core.WatchEvent{
			Kind:    core.WatchRecovered,
			Summary: fmt.Sprintf("github is answering again (after %d failed attempts)", watch.Failures),
			At:      now,
		})
	}

	for _, change := range changesBetween(previous, observation, !hadPrevious) {
		change.At = now
		s.record(ctx, updated, change)
	}

	s.refreshed.signal(watch.ID)

	return updated, nil
}

// read fetches a pull request, finding the account that can see it if that is not yet known.
//
// A machine with a work login and a personal one has a repository each login cannot see, and gh
// does not choose by directory. So: try the account that worked last time; otherwise try each login
// in turn, active first, and remember which one answered. Only a "not found" is worth trying the
// next account for — an outage looks the same from every login.
func (s *Service) read(ctx context.Context, watch core.Watch) (github.PullRequest, string, error) {
	if watch.Account != "" {
		observation, err := s.forge.PullRequest(ctx, watch.Account, watch.Owner, watch.Repository, watch.Number)
		if err == nil {
			return observation, watch.Account, nil
		}
		if !errors.Is(err, github.ErrNotFound) {
			return github.PullRequest{}, "", err
		}

		// The login that used to see it no longer does — it was signed out, or lost access. Fall
		// through and look again rather than reporting a pull request as gone.
		s.logger.Info("the account that could see this pull request no longer can; looking again",
			"reference", watch.Reference(), "account", watch.Account)
	}

	// The active account first: on the usual single-login machine this is the only attempt.
	observation, err := s.forge.PullRequest(ctx, "", watch.Owner, watch.Repository, watch.Number)
	if err == nil {
		return observation, "", nil
	}
	if !errors.Is(err, github.ErrNotFound) {
		return github.PullRequest{}, "", err
	}

	accounts, accountsErr := s.forge.Accounts(ctx)
	if accountsErr != nil || len(accounts) <= 1 {
		// Nothing else to try: report the original answer, which is the useful one.
		return github.PullRequest{}, "", err
	}

	for _, account := range accounts {
		if account == watch.Account {
			continue // already tried, above
		}

		observation, attemptErr := s.forge.PullRequest(ctx, account, watch.Owner, watch.Repository, watch.Number)
		if attemptErr == nil {
			s.logger.Info("found the pull request under another github login",
				"reference", watch.Reference(), "account", account)

			return observation, account, nil
		}
		if !errors.Is(attemptErr, github.ErrNotFound) {
			return github.PullRequest{}, "", attemptErr
		}
	}

	return github.PullRequest{}, "", fmt.Errorf("%w (tried every github login this machine is signed in to: %s)",
		err, strings.Join(accounts, ", "))
}

// recordFailure notes that GitHub could not be reached and works out when to try again.
func (s *Service) recordFailure(ctx context.Context, watch core.Watch, cause error) error {
	now := s.now()
	nextPoll := now.Add(s.backoffFor(watch.Failures + 1))

	if err := s.store.Watches().RecordFailure(ctx, watch.ID, cause.Error(), now, nextPoll); err != nil {
		return err
	}

	// Say it once, when it starts, rather than on every attempt: an outage should not fill an
	// agent's inbox with the same sentence.
	if watch.Failures == 0 {
		s.record(ctx, watch, core.WatchEvent{
			Kind:    core.WatchUnreachable,
			Summary: "github could not be reached: " + firstLine(cause.Error()),
			At:      now,
		})
	}

	s.logger.Warn("could not read a watched pull request",
		"reference", watch.Reference(), "failures", watch.Failures+1,
		"next_attempt", nextPoll.Format(time.RFC3339), "error", cause)

	s.refreshed.signal(watch.ID)

	return cause
}

// intervalFor decides when to look again. A settled pull request is looked at rarely: it is
// finished, and polling it is a request somebody else could have used.
func (s *Service) intervalFor(observation github.PullRequest) time.Duration {
	switch observation.State {
	case "merged", "closed":
		return s.options.SettledInterval
	default:
		return s.options.Interval
	}
}

// backoffFor spaces out attempts after failures, with a little jitter so a daemon watching several
// pull requests does not retry them all in the same instant.
func (s *Service) backoffFor(failures int) time.Duration {
	wait := s.options.Interval
	for range failures - 1 {
		wait *= 2
		if wait >= maxBackoff {
			wait = maxBackoff

			break
		}
	}

	jitter := time.Duration(rand.Int64N(int64(wait / 4))) //nolint:gosec // scheduling, not secrecy

	return wait + jitter
}

// record stores a change and tells whoever is listening.
func (s *Service) record(ctx context.Context, watch core.Watch, event core.WatchEvent) {
	event.WatchID = watch.ID
	if event.At.IsZero() {
		event.At = s.now()
	}

	if err := s.store.Watches().AppendEvent(ctx, event); err != nil {
		s.logger.Error("could not record a change to a pull request",
			"reference", watch.Reference(), "error", err)

		return
	}

	s.logger.Info("pull request changed",
		"reference", watch.Reference(), "change", event.Summary)

	s.notify(ctx, watch, event)
}

// notify sends the change to each subscriber as an ordinary message, so an agent that was busy
// still finds out, and one connected over MCP is pushed a notification.
func (s *Service) notify(ctx context.Context, watch core.Watch, event core.WatchEvent) {
	if s.messaging == nil {
		return
	}

	subscribers, err := s.store.Watches().Subscribers(ctx, watch.ID)
	if err != nil {
		s.logger.Error("could not read the subscribers of a watch",
			"reference", watch.Reference(), "error", err)

		return
	}
	if len(subscribers) == 0 {
		return
	}

	body, err := json.Marshal(map[string]any{
		"watch_id":  watch.ID,
		"reference": watch.Reference(),
		"kind":      string(event.Kind),
		"text":      fmt.Sprintf("%s: %s", watch.Reference(), event.Summary),
		"state":     watch.State,
	})
	if err != nil {
		s.logger.Error("could not encode a pull request change", "error", err)
		return
	}

	for _, subscriber := range subscribers {
		agent, err := s.registry.ByID(ctx, subscriber)
		if err != nil {
			// A subscriber that has gone away is not an error worth shouting about; the watch
			// outlives the agents watching it.
			continue
		}

		if _, err := s.messaging.Send(ctx, messaging.SendRequest{
			FromAgentID: s.sender(subscriber),
			ToAgentName: agent.Name,
			Kind:        MessageKind,
			Body:        body,
		}); err != nil {
			s.logger.Warn("could not deliver a pull request change",
				"agent", agent.Name, "reference", watch.Reference(), "error", err)
		}
	}
}

// Sweep polls everything that is due, and reports how many it looked at. The daemon calls it on a
// timer; it is exported so a test can drive it without waiting.
func (s *Service) Sweep(ctx context.Context) int {
	s.polling.Lock()
	defer s.polling.Unlock()

	due, err := s.store.Watches().Due(ctx, s.now(), pollBatch)
	if err != nil {
		s.logger.Error("could not find pull requests to look at", "error", err)

		return 0
	}

	polled := 0
	for index := range due {
		if ctx.Err() != nil {
			return polled
		}

		// A failure is already recorded and logged inside poll; there is nothing useful to do with
		// the error here beyond carrying on to the next one.
		_, _ = s.poll(ctx, due[index])
		polled++
	}

	return polled
}

// Run sweeps until the context ends. The ticker is short relative to the poll interval, because
// what it is really doing is noticing which watches have come due.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(s.tickInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Sweep(ctx)
		}
	}
}

func (s *Service) tickInterval() time.Duration {
	const shortest = time.Second

	tick := s.options.Interval / 4
	if tick < shortest {
		return shortest
	}

	return tick
}

// signals wakes anybody waiting on a watch being polled.
type signals struct {
	mutex    sync.Mutex
	channels map[string]chan struct{}
}

func newSignals() *signals {
	return &signals{channels: make(map[string]chan struct{})}
}

func (s *signals) watch(key string) <-chan struct{} {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	channel, found := s.channels[key]
	if !found {
		channel = make(chan struct{})
		s.channels[key] = channel
	}

	return channel
}

func (s *signals) signal(key string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if channel, found := s.channels[key]; found {
		close(channel)
		delete(s.channels, key)
	}
}

// Await blocks until the watch is next polled, or the context ends. An agent that has just pushed a
// commit uses it to wait for the next observation rather than asking repeatedly.
func (s *Service) Await(ctx context.Context, watchID string) (core.Watch, error) {
	polled := s.refreshed.watch(watchID)

	select {
	case <-polled:
		return s.store.Watches().ByID(ctx, watchID)
	case <-ctx.Done():
		return core.Watch{}, ctx.Err() //nolint:wrapcheck // the caller compares with context errors
	}
}
