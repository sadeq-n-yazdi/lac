package prwatch_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sadeq.uk/lac/internal/auth"
	"sadeq.uk/lac/internal/core"
	"sadeq.uk/lac/internal/github"
	"sadeq.uk/lac/internal/service/messaging"
	"sadeq.uk/lac/internal/service/prwatch"
	"sadeq.uk/lac/internal/service/registry"
	"sadeq.uk/lac/internal/store/sqlite"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// forge stands in for GitHub: a test sets what the next read returns, including a failure.
type forge struct {
	mutex   sync.Mutex
	current github.PullRequest
	failure error
	reads   int
	// visibleTo, when set, is the only account that can see the pull request. It stands in for a
	// repository one github login can see and another cannot.
	visibleTo string
	accounts  []string
	// readsAs records which account each read was made as.
	readsAs []string
}

func (f *forge) PullRequest(
	_ context.Context, account, _, _ string, _ int,
) (github.PullRequest, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.reads++
	f.readsAs = append(f.readsAs, account)

	if f.failure != nil {
		return github.PullRequest{}, f.failure
	}
	if f.visibleTo != "" && account != f.visibleTo {
		return github.PullRequest{}, github.ErrNotFound
	}

	return f.current, nil
}

func (f *forge) Accounts(context.Context) ([]string, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	if f.accounts == nil {
		return []string{"active-login"}, nil
	}

	return f.accounts, nil
}

func (f *forge) Available(context.Context) error { return nil }

func (f *forge) onlyVisibleTo(account string, accounts ...string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.visibleTo = account
	f.accounts = accounts
}

func (f *forge) accountsUsed() []string {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	return append([]string(nil), f.readsAs...)
}

func (f *forge) set(pullRequest github.PullRequest) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.current = pullRequest
	f.failure = nil
}

func (f *forge) fail(err error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.failure = err
}

func (f *forge) readCount() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	return f.reads
}

type harness struct {
	service   *prwatch.Service
	forge     *forge
	messaging *messaging.Service
	registry  *registry.Service
	store     *sqlite.Store
	now       time.Time
	nowMutex  sync.Mutex
}

var baseTime = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

func newHarness(t *testing.T) *harness {
	t.Helper()

	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "lac.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	secret, err := auth.LoadOrCreateSecret(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatalf("creating the secret: %v", err)
	}
	authenticator, err := auth.New(store, secret, auth.Options{Logger: quietLogger()})
	if err != nil {
		t.Fatalf("creating the authenticator: %v", err)
	}

	subject := &harness{forge: &forge{}, store: store, now: baseTime}

	subject.registry = registry.New(store, authenticator, registry.Options{
		TimeToLive: time.Hour,
		CapabilitiesFor: func(string) core.Capabilities {
			return core.Capabilities{Resources: []string{core.WildcardResource}, CanBroadcast: true}
		},
		Logger: quietLogger(),
	})
	subject.messaging = messaging.New(store, messaging.Options{Logger: quietLogger()})

	subject.service = prwatch.New(store, subject.forge, subject.messaging, subject.registry,
		prwatch.Options{
			Interval: time.Minute,
			Clock:    subject.clock,
			Logger:   quietLogger(),
		})

	return subject
}

func (h *harness) clock() time.Time {
	h.nowMutex.Lock()
	defer h.nowMutex.Unlock()

	return h.now
}

func (h *harness) advance(by time.Duration) {
	h.nowMutex.Lock()
	defer h.nowMutex.Unlock()

	h.now = h.now.Add(by)
}

func (h *harness) agent(t *testing.T, name string, processID int) core.Agent {
	t.Helper()

	registration, err := h.registry.Register(t.Context(), registry.RegisterRequest{
		Name: name, Kind: "claude", Workdir: "/tmp/" + name, ProcessID: processID,
	})
	if err != nil {
		t.Fatalf("registering %q: %v", name, err)
	}

	return registration.Agent
}

// openPullRequest is a reasonable starting state to vary from.
func openPullRequest() github.PullRequest {
	return github.PullRequest{
		Owner: "sadeq-n-yazdi", Repository: "lac", Number: 31,
		Title: "feat: something", State: "open", Author: "sadeq-n-yazdi",
		HeadSHA: "aaaaaaa", Mergeable: "MERGEABLE",
		Checks: github.ChecksSummary{State: "pending", Total: 1, Pending: 1,
			Checks: []github.Check{{Name: "Test", State: "pending"}}},
	}
}

// inbox reads what an agent was told, so a test asserts on what actually reaches somebody rather
// than on an internal event list.
func (h *harness) inbox(t *testing.T, agent core.Agent) []string {
	t.Helper()

	messages, err := h.messaging.Inbox(t.Context(), agent.ID, 0)
	if err != nil {
		t.Fatalf("Inbox() = %v, want nil", err)
	}

	lines := make([]string, 0, len(messages))
	for _, message := range messages {
		lines = append(lines, string(message.Body))
	}

	return lines
}

// Watching reads the pull request at once, so the caller gets a real answer rather than an empty
// record it has to come back for.
func TestWatchReadsImmediately(t *testing.T) {
	subject := newHarness(t)
	agent := subject.agent(t, "claude-a", 1)
	subject.forge.set(openPullRequest())

	watch, err := subject.service.Watch(t.Context(), agent.ID, "sadeq-n-yazdi", "lac", 31)
	if err != nil {
		t.Fatalf("Watch() = %v, want nil", err)
	}

	if watch.State != "open" || watch.Title != "feat: something" {
		t.Errorf("watch = %+v, want the pull request as it is", watch)
	}
	if !watch.Observed() {
		t.Error("the watch was never observed, though the read succeeded")
	}

	// And the first observation is not reported as a change: nothing happened, somebody just
	// started watching.
	if told := subject.inbox(t, agent); len(told) != 0 {
		t.Errorf("the agent was told %v on the first read, want nothing", told)
	}
}

// The whole point: when something happens, whoever is watching is told, in words.
func TestSubscribersAreToldWhatChanged(t *testing.T) {
	subject := newHarness(t)
	agent := subject.agent(t, "claude-a", 1)
	subject.forge.set(openPullRequest())

	watch, err := subject.service.Watch(t.Context(), agent.ID, "sadeq-n-yazdi", "lac", 31)
	if err != nil {
		t.Fatalf("Watch() = %v, want nil", err)
	}

	// CI finishes, badly.
	failing := openPullRequest()
	failing.Checks = github.ChecksSummary{State: "failure", Total: 2, Passed: 1, Failed: 1,
		Checks: []github.Check{
			{Name: "Lint", State: "success"},
			{Name: "Test", State: "failure"},
		}}
	subject.forge.set(failing)
	subject.advance(time.Minute)

	if _, err := subject.service.Refresh(t.Context(), watch.ID); err != nil {
		t.Fatalf("Refresh() = %v, want nil", err)
	}

	told := subject.inbox(t, agent)
	if len(told) != 1 {
		t.Fatalf("the agent was told %d things, want 1: %v", len(told), told)
	}
	// The failing check is named, because "CI failed" alone sends somebody to go and look.
	if !strings.Contains(told[0], "CI failed: Test") {
		t.Errorf("the agent was told %q, want the failing check named", told[0])
	}
	if !strings.Contains(told[0], "sadeq-n-yazdi/lac#31") {
		t.Errorf("the agent was told %q, want it to say which pull request", told[0])
	}
}

// Every kind of change the watcher exists to notice.
func TestChangesThatAreReported(t *testing.T) {
	tests := map[string]struct {
		change func(github.PullRequest) github.PullRequest
		want   string
	}{
		"CI passes": {
			change: func(p github.PullRequest) github.PullRequest {
				p.Checks = github.ChecksSummary{State: "success", Total: 2, Passed: 2}
				return p
			},
			want: "CI passed (2 checks)",
		},
		"it is merged": {
			change: func(p github.PullRequest) github.PullRequest {
				p.State = "merged"
				return p
			},
			want: "merged",
		},
		"it is closed without merging": {
			change: func(p github.PullRequest) github.PullRequest {
				p.State = "closed"
				return p
			},
			want: "closed without merging",
		},
		"it becomes a draft": {
			change: func(p github.PullRequest) github.PullRequest {
				p.Draft = true
				return p
			},
			want: "marked as a draft",
		},
		"it is ready for review": {
			change: func(p github.PullRequest) github.PullRequest {
				p.Draft = false
				return p
			},
			want: "",
		},
		"somebody comments": {
			change: func(p github.PullRequest) github.PullRequest {
				p.Comments = append(p.Comments, github.Comment{
					ID: "c1", Author: "reviewer", Body: "this needs a test\nand a comment",
				})
				return p
			},
			want: "reviewer commented: this needs a test",
		},
		"somebody approves": {
			change: func(p github.PullRequest) github.PullRequest {
				p.Reviews = append(p.Reviews, github.Review{
					ID: "r1", Author: "reviewer", State: "approved",
				})
				return p
			},
			want: "reviewer approved the pull request",
		},
		"somebody requests changes": {
			change: func(p github.PullRequest) github.PullRequest {
				p.Reviews = append(p.Reviews, github.Review{
					ID: "r1", Author: "reviewer", State: "changes_requested",
				})
				return p
			},
			want: "reviewer requested changes on the pull request",
		},
		"a review comment appears": {
			change: func(p github.PullRequest) github.PullRequest {
				p.Threads = append(p.Threads, github.ReviewThread{
					ID: "t1", Path: "main.go", Line: 12,
					Comments: []github.Comment{{Author: "reviewer", Body: "this leaks"}},
				})
				return p
			},
			want: "new review comment on main.go:12: this leaks",
		},
		"the branch is pushed to": {
			change: func(p github.PullRequest) github.PullRequest {
				p.HeadSHA = "bbbbbbbccccccc"
				return p
			},
			want: "pushed: the branch is now at bbbbbbb",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			subject := newHarness(t)
			agent := subject.agent(t, "claude-a", 1)

			start := openPullRequest()
			if name == "it is ready for review" {
				start.Draft = true
			}
			subject.forge.set(start)

			watch, err := subject.service.Watch(t.Context(), agent.ID, "sadeq-n-yazdi", "lac", 31)
			if err != nil {
				t.Fatalf("Watch() = %v, want nil", err)
			}

			subject.forge.set(test.change(start))
			subject.advance(time.Minute)

			if _, err := subject.service.Refresh(t.Context(), watch.ID); err != nil {
				t.Fatalf("Refresh() = %v, want nil", err)
			}

			want := test.want
			if name == "it is ready for review" {
				want = "marked ready for review"
			}

			told := strings.Join(subject.inbox(t, agent), " ")
			if !strings.Contains(told, want) {
				t.Errorf("the agent was told %q, want it to mention %q", told, want)
			}
		})
	}
}

// The one an author waits for: a review conversation being resolved is how they know a round of
// review is finished.
func TestResolvingAReviewThreadIsReported(t *testing.T) {
	subject := newHarness(t)
	agent := subject.agent(t, "claude-a", 1)

	start := openPullRequest()
	start.Threads = []github.ReviewThread{{
		ID: "t1", Path: "main.go", Line: 12,
		Comments: []github.Comment{{Author: "reviewer", Body: "this leaks a file handle"}},
	}}
	subject.forge.set(start)

	watch, err := subject.service.Watch(t.Context(), agent.ID, "sadeq-n-yazdi", "lac", 31)
	if err != nil {
		t.Fatalf("Watch() = %v, want nil", err)
	}
	if watch.Unresolved != 1 {
		t.Errorf("Unresolved = %d, want 1", watch.Unresolved)
	}

	resolved := start
	resolved.Threads = []github.ReviewThread{{
		ID: "t1", Path: "main.go", Line: 12, Resolved: true,
		Comments: start.Threads[0].Comments,
	}}
	subject.forge.set(resolved)
	subject.advance(time.Minute)

	updated, err := subject.service.Refresh(t.Context(), watch.ID)
	if err != nil {
		t.Fatalf("Refresh() = %v, want nil", err)
	}
	if updated.Unresolved != 0 {
		t.Errorf("Unresolved = %d, want 0 once the thread is resolved", updated.Unresolved)
	}

	told := strings.Join(subject.inbox(t, agent), " ")
	if !strings.Contains(told, "review comment resolved on main.go:12") {
		t.Errorf("the agent was told %q, want the resolution reported", told)
	}
}

// A reply on an existing conversation is a change too — that is how a discussion carries on.
func TestARepliedThreadIsReported(t *testing.T) {
	subject := newHarness(t)
	agent := subject.agent(t, "claude-a", 1)

	start := openPullRequest()
	start.Threads = []github.ReviewThread{{
		ID: "t1", Path: "main.go", Line: 12,
		Comments: []github.Comment{{Author: "reviewer", Body: "this leaks"}},
	}}
	subject.forge.set(start)

	watch, err := subject.service.Watch(t.Context(), agent.ID, "sadeq-n-yazdi", "lac", 31)
	if err != nil {
		t.Fatalf("Watch() = %v, want nil", err)
	}

	replied := start
	replied.Threads = []github.ReviewThread{{
		ID: "t1", Path: "main.go", Line: 12,
		Comments: []github.Comment{
			{Author: "reviewer", Body: "this leaks"},
			{Author: "claude-a", Body: "fixed in the next commit"},
		},
	}}
	subject.forge.set(replied)
	subject.advance(time.Minute)

	if _, err := subject.service.Refresh(t.Context(), watch.ID); err != nil {
		t.Fatalf("Refresh() = %v, want nil", err)
	}

	told := strings.Join(subject.inbox(t, agent), " ")
	if !strings.Contains(told, "claude-a replied on main.go:12: fixed in the next commit") {
		t.Errorf("the agent was told %q, want the reply reported", told)
	}
}

// Nothing changing must tell nobody anything. A watcher that reports every poll is a watcher
// everybody turns off.
func TestNoChangeTellsNobody(t *testing.T) {
	subject := newHarness(t)
	agent := subject.agent(t, "claude-a", 1)
	subject.forge.set(openPullRequest())

	watch, err := subject.service.Watch(t.Context(), agent.ID, "sadeq-n-yazdi", "lac", 31)
	if err != nil {
		t.Fatalf("Watch() = %v, want nil", err)
	}

	for range 3 {
		subject.advance(time.Minute)
		if _, err := subject.service.Refresh(t.Context(), watch.ID); err != nil {
			t.Fatalf("Refresh() = %v, want nil", err)
		}
	}

	if told := subject.inbox(t, agent); len(told) != 0 {
		t.Errorf("the agent was told %v, want nothing when nothing changed", told)
	}
}

// The resilience that matters: during an outage the last good answer is still there, marked as old,
// rather than an error.
func TestTheLastGoodAnswerSurvivesAnOutage(t *testing.T) {
	subject := newHarness(t)
	agent := subject.agent(t, "claude-a", 1)
	subject.forge.set(openPullRequest())

	watch, err := subject.service.Watch(t.Context(), agent.ID, "sadeq-n-yazdi", "lac", 31)
	if err != nil {
		t.Fatalf("Watch() = %v, want nil", err)
	}

	subject.forge.fail(github.ErrUnavailable)
	subject.advance(time.Minute)

	if _, err := subject.service.Refresh(t.Context(), watch.ID); !errors.Is(err, github.ErrUnavailable) {
		t.Fatalf("Refresh() during an outage = %v, want the failure reported", err)
	}

	stored, err := subject.service.ByID(t.Context(), watch.ID)
	if err != nil {
		t.Fatalf("ByID() = %v, want nil", err)
	}

	// What was true is still there.
	observation, known := subject.service.Snapshot(stored)
	if !known || observation.Title != "feat: something" {
		t.Errorf("the last good observation was lost during the outage: %+v", observation)
	}
	// And it is honestly marked as old.
	if !subject.service.Stale(stored) {
		t.Error("the watch is not marked stale after a failure")
	}
	if stored.Failures != 1 || !strings.Contains(stored.LastError, "not reachable") {
		t.Errorf("the failure was not recorded: %+v", stored)
	}
}

// An outage should be said once, not on every attempt: it must not fill an agent's inbox with the
// same sentence.
func TestAnOutageIsReportedOnceAndRecoveryIsToo(t *testing.T) {
	subject := newHarness(t)
	agent := subject.agent(t, "claude-a", 1)
	subject.forge.set(openPullRequest())

	watch, err := subject.service.Watch(t.Context(), agent.ID, "sadeq-n-yazdi", "lac", 31)
	if err != nil {
		t.Fatalf("Watch() = %v, want nil", err)
	}

	subject.forge.fail(github.ErrUnavailable)
	for range 4 {
		subject.advance(time.Minute)
		_, _ = subject.service.Refresh(t.Context(), watch.ID)
	}

	told := subject.inbox(t, agent)
	if len(told) != 1 {
		t.Fatalf("the agent was told %d things about one outage, want 1: %v", len(told), told)
	}
	if !strings.Contains(told[0], "could not be reached") {
		t.Errorf("the agent was told %q, want the outage explained", told[0])
	}
	if _, err := subject.messaging.Acknowledge(t.Context(), agent.ID, messageIDs(t, subject, agent)); err != nil {
		t.Fatalf("Acknowledge() = %v, want nil", err)
	}

	// GitHub comes back.
	subject.forge.set(openPullRequest())
	subject.advance(time.Minute)

	if _, err := subject.service.Refresh(t.Context(), watch.ID); err != nil {
		t.Fatalf("Refresh() after the outage = %v, want nil", err)
	}

	recovered := strings.Join(subject.inbox(t, agent), " ")
	if !strings.Contains(recovered, "answering again") {
		t.Errorf("the agent was told %q, want the recovery reported", recovered)
	}

	stored, err := subject.service.ByID(t.Context(), watch.ID)
	if err != nil {
		t.Fatalf("ByID() = %v, want nil", err)
	}
	if stored.Failures != 0 || subject.service.Stale(stored) {
		t.Errorf("the watch is still marked as failing after recovery: %+v", stored)
	}
}

// Repeated failures must back off. A watcher that turns an outage into a rate limit has made
// things worse.
func TestFailuresBackOff(t *testing.T) {
	subject := newHarness(t)
	agent := subject.agent(t, "claude-a", 1)
	subject.forge.set(openPullRequest())

	watch, err := subject.service.Watch(t.Context(), agent.ID, "sadeq-n-yazdi", "lac", 31)
	if err != nil {
		t.Fatalf("Watch() = %v, want nil", err)
	}

	subject.forge.fail(github.ErrUnavailable)

	waits := make([]time.Duration, 0, 4)
	for range 4 {
		subject.advance(time.Minute)
		_, _ = subject.service.Refresh(t.Context(), watch.ID)

		stored, err := subject.service.ByID(t.Context(), watch.ID)
		if err != nil {
			t.Fatalf("ByID() = %v, want nil", err)
		}
		waits = append(waits, stored.NextPollAt.Sub(subject.clock()))
	}

	for index := 1; index < len(waits); index++ {
		if waits[index] <= waits[index-1] {
			t.Errorf("the wait did not grow: %v", waits)

			break
		}
	}
}

// A sweep only touches what is due, so a watch polled a moment ago is left alone.
func TestSweepOnlyPollsWhatIsDue(t *testing.T) {
	subject := newHarness(t)
	agent := subject.agent(t, "claude-a", 1)
	subject.forge.set(openPullRequest())

	if _, err := subject.service.Watch(t.Context(), agent.ID, "sadeq-n-yazdi", "lac", 31); err != nil {
		t.Fatalf("Watch() = %v, want nil", err)
	}

	before := subject.forge.readCount()

	if polled := subject.service.Sweep(t.Context()); polled != 0 {
		t.Errorf("Sweep() polled %d watches straight after a read, want 0", polled)
	}
	if subject.forge.readCount() != before {
		t.Error("Sweep() read github when nothing was due")
	}

	subject.advance(2 * time.Minute)

	if polled := subject.service.Sweep(t.Context()); polled != 1 {
		t.Errorf("Sweep() polled %d watches once one was due, want 1", polled)
	}
}

// A settled pull request is looked at far less often: it is finished, and every poll is a request
// somebody else could have used.
func TestASettledPullRequestIsPolledRarely(t *testing.T) {
	subject := newHarness(t)
	agent := subject.agent(t, "claude-a", 1)

	merged := openPullRequest()
	merged.State = "merged"
	subject.forge.set(merged)

	watch, err := subject.service.Watch(t.Context(), agent.ID, "sadeq-n-yazdi", "lac", 31)
	if err != nil {
		t.Fatalf("Watch() = %v, want nil", err)
	}

	if wait := watch.NextPollAt.Sub(subject.clock()); wait < 10*time.Minute {
		t.Errorf("a merged pull request is polled again in %s, want much less often", wait)
	}
}

// A reference nobody can see is a mistake, not an outage: it must not be retried forever.
func TestWatchingSomethingThatIsNotThereIsRefused(t *testing.T) {
	subject := newHarness(t)
	agent := subject.agent(t, "claude-a", 1)
	subject.forge.fail(github.ErrNotFound)

	_, err := subject.service.Watch(t.Context(), agent.ID, "nobody", "nothing", 1)
	if !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("Watch() = %v, want ErrNotFound", err)
	}

	watches, err := subject.service.All(t.Context())
	if err != nil {
		t.Fatalf("All() = %v, want nil", err)
	}
	if len(watches) != 0 {
		t.Errorf("a watch was left behind for a pull request that does not exist: %+v", watches)
	}
}

// Two agents watching one pull request share the work: it is read once and both are told.
func TestTwoAgentsShareOneWatch(t *testing.T) {
	subject := newHarness(t)
	first := subject.agent(t, "claude-a", 1)
	second := subject.agent(t, "claude-b", 2)
	subject.forge.set(openPullRequest())

	watch, err := subject.service.Watch(t.Context(), first.ID, "sadeq-n-yazdi", "lac", 31)
	if err != nil {
		t.Fatalf("Watch() = %v, want nil", err)
	}

	reads := subject.forge.readCount()

	again, err := subject.service.Watch(t.Context(), second.ID, "sadeq-n-yazdi", "lac", 31)
	if err != nil {
		t.Fatalf("a second Watch() = %v, want nil", err)
	}
	if again.ID != watch.ID {
		t.Errorf("the second agent got watch %q, want the same one %q", again.ID, watch.ID)
	}
	if subject.forge.readCount() != reads+1 {
		t.Error("subscribing read github more than once")
	}

	merged := openPullRequest()
	merged.State = "merged"
	subject.forge.set(merged)
	subject.advance(time.Minute)

	if _, err := subject.service.Refresh(t.Context(), watch.ID); err != nil {
		t.Fatalf("Refresh() = %v, want nil", err)
	}

	for _, agent := range []core.Agent{first, second} {
		told := strings.Join(subject.inbox(t, agent), " ")
		if !strings.Contains(told, "merged") {
			t.Errorf("%s was told %q, want the merge", agent.Name, told)
		}
	}
}

// Unsubscribing stops the messages without throwing the history away.
func TestUnwatchStopsTheMessages(t *testing.T) {
	subject := newHarness(t)
	agent := subject.agent(t, "claude-a", 1)
	subject.forge.set(openPullRequest())

	watch, err := subject.service.Watch(t.Context(), agent.ID, "sadeq-n-yazdi", "lac", 31)
	if err != nil {
		t.Fatalf("Watch() = %v, want nil", err)
	}

	if err := subject.service.Unwatch(t.Context(), agent.ID, watch.ID); err != nil {
		t.Fatalf("Unwatch() = %v, want nil", err)
	}

	merged := openPullRequest()
	merged.State = "merged"
	subject.forge.set(merged)
	subject.advance(time.Minute)

	if _, err := subject.service.Refresh(t.Context(), watch.ID); err != nil {
		t.Fatalf("Refresh() = %v, want nil", err)
	}

	if told := subject.inbox(t, agent); len(told) != 0 {
		t.Errorf("an agent that stopped watching was still told %v", told)
	}

	// The change is still recorded, for anybody who looks later.
	events, err := subject.service.Events(t.Context(), watch.ID, 0)
	if err != nil {
		t.Fatalf("Events() = %v, want nil", err)
	}
	if len(events) == 0 {
		t.Error("the change was not recorded after everybody unsubscribed")
	}
}

// Await is how an agent waits for the next observation rather than asking over and over.
func TestAwaitReturnsOnTheNextPoll(t *testing.T) {
	subject := newHarness(t)
	agent := subject.agent(t, "claude-a", 1)
	subject.forge.set(openPullRequest())

	watch, err := subject.service.Watch(t.Context(), agent.ID, "sadeq-n-yazdi", "lac", 31)
	if err != nil {
		t.Fatalf("Watch() = %v, want nil", err)
	}

	awaited := make(chan core.Watch, 1)
	go func() {
		updated, err := subject.service.Await(t.Context(), watch.ID)
		if err != nil {
			t.Errorf("Await() = %v, want nil", err)
			return
		}
		awaited <- updated
	}()

	time.Sleep(50 * time.Millisecond)

	merged := openPullRequest()
	merged.State = "merged"
	subject.forge.set(merged)
	subject.advance(time.Minute)

	if _, err := subject.service.Refresh(t.Context(), watch.ID); err != nil {
		t.Fatalf("Refresh() = %v, want nil", err)
	}

	select {
	case updated := <-awaited:
		if updated.State != "merged" {
			t.Errorf("Await() returned state %q, want merged", updated.State)
		}
	case <-time.After(5 * time.Second):
		t.Error("Await() did not return when the watch was polled")
	}
}

func messageIDs(t *testing.T, subject *harness, agent core.Agent) []string {
	t.Helper()

	messages, err := subject.messaging.Inbox(t.Context(), agent.ID, 0)
	if err != nil {
		t.Fatalf("Inbox() = %v, want nil", err)
	}

	identifiers := make([]string, 0, len(messages))
	for _, message := range messages {
		identifiers = append(identifiers, message.ID)
	}

	return identifiers
}

// A machine with a work login and a personal one has repositories each login cannot see, and gh
// does not choose by directory. The watcher has to find the login that can see a pull request, and
// then keep using it.
func TestARepositoryVisibleToAnotherLoginIsFound(t *testing.T) {
	subject := newHarness(t)
	agent := subject.agent(t, "claude-a", 1)

	subject.forge.set(openPullRequest())
	subject.forge.onlyVisibleTo("personal-login", "work-login", "personal-login")

	watch, err := subject.service.Watch(t.Context(), agent.ID, "sadeq-n-yazdi", "personal-website", 5)
	if err != nil {
		t.Fatalf("Watch() = %v, want it to find the login that can see it", err)
	}
	if watch.Account != "personal-login" {
		t.Errorf("Account = %q, want the login that could see it", watch.Account)
	}

	// The active login is tried first, because on the usual single-login machine it is the only
	// attempt worth making.
	used := subject.forge.accountsUsed()
	if len(used) < 2 || used[0] != "" {
		t.Errorf("the reads were made as %v, want the active login tried first", used)
	}

	// And the next read goes straight to the login that worked, rather than searching again.
	subject.advance(time.Minute)
	before := len(subject.forge.accountsUsed())

	if _, err := subject.service.Refresh(t.Context(), watch.ID); err != nil {
		t.Fatalf("Refresh() = %v, want nil", err)
	}

	used = subject.forge.accountsUsed()
	if len(used) != before+1 || used[len(used)-1] != "personal-login" {
		t.Errorf("the refresh read as %v, want one read as the remembered login", used[before:])
	}
}

// A pull request no login can see is refused, and says that every login was tried — otherwise the
// operator is left wondering whether the right account was even considered.
func TestAPullRequestNoLoginCanSeeSaysSo(t *testing.T) {
	subject := newHarness(t)
	agent := subject.agent(t, "claude-a", 1)

	subject.forge.onlyVisibleTo("nobody-has-this-login", "work-login", "personal-login")

	_, err := subject.service.Watch(t.Context(), agent.ID, "someone", "private", 1)
	if !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("Watch() = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "work-login") || !strings.Contains(err.Error(), "personal-login") {
		t.Errorf("the error does not say which logins were tried: %v", err)
	}
}

// An outage looks the same from every login, so it must not send the watcher round all of them.
func TestAnOutageIsNotMistakenForAVisibilityProblem(t *testing.T) {
	subject := newHarness(t)
	agent := subject.agent(t, "claude-a", 1)

	subject.forge.set(openPullRequest())
	watch, err := subject.service.Watch(t.Context(), agent.ID, "sadeq-n-yazdi", "lac", 31)
	if err != nil {
		t.Fatalf("Watch() = %v, want nil", err)
	}

	subject.forge.fail(github.ErrUnavailable)
	before := len(subject.forge.accountsUsed())
	subject.advance(time.Minute)

	_, _ = subject.service.Refresh(t.Context(), watch.ID)

	if attempts := len(subject.forge.accountsUsed()) - before; attempts != 1 {
		t.Errorf("an outage caused %d reads, want 1: every login would fail the same way", attempts)
	}
}
