package github_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sadeq.uk/lac/internal/github"
)

// stubGH writes a shell script standing in for the gh command, so the failure paths are exercised
// through a real subprocess rather than a mock: an exit status and a line on stderr are exactly
// what the client has to make sense of in the field.
func stubGH(t *testing.T, script string) *github.Client {
	t.Helper()

	path := filepath.Join(t.TempDir(), "gh")
	contents := "#!/bin/sh\n" + script + "\n"

	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatalf("writing the gh stub: %v", err)
	}

	return github.New(github.Options{Executable: path})
}

// answering returns a stub that prints the given GraphQL answer.
func answering(t *testing.T, body string) *github.Client {
	t.Helper()

	quoted := strings.ReplaceAll(body, "'", "'\\''")

	return stubGH(t, "cat <<'ANSWER'\n"+quoted+"\nANSWER")
}

const fullAnswer = `{
  "data": {"repository": {"pullRequest": {
    "number": 31,
    "title": "feat: lac reload",
    "url": "https://github.com/sadeq-n-yazdi/lac/pull/31",
    "state": "OPEN",
    "isDraft": false,
    "merged": false,
    "mergeable": "MERGEABLE",
    "updatedAt": "2026-09-11T07:20:00Z",
    "author": {"login": "sadeq-n-yazdi"},
    "headRefOid": "abc123",
    "comments": {"nodes": [
      {"id": "c1", "author": {"login": "reviewer"}, "body": "looks good", "createdAt": "2026-09-11T07:10:00Z"}
    ]},
    "reviews": {"nodes": [
      {"id": "r1", "author": {"login": "reviewer"}, "state": "APPROVED", "submittedAt": "2026-09-11T07:11:00Z"}
    ]},
    "reviewThreads": {"nodes": [
      {"id": "t1", "isResolved": false, "isOutdated": false, "path": "main.go", "line": 12,
       "comments": {"nodes": [
         {"id": "tc1", "author": {"login": "reviewer"}, "body": "this leaks a file handle\nsecond line", "createdAt": "2026-09-11T07:12:00Z"}
       ]}},
      {"id": "t2", "isResolved": true, "isOutdated": false, "path": "other.go", "line": 3,
       "comments": {"nodes": [
         {"id": "tc2", "author": {"login": "reviewer"}, "body": "nit", "createdAt": "2026-09-11T07:13:00Z"}
       ]}},
      {"id": "t3", "isResolved": false, "isOutdated": true, "path": "old.go", "line": null,
       "comments": {"nodes": []}}
    ]},
    "commits": {"nodes": [{"commit": {"statusCheckRollup": {
      "state": "FAILURE",
      "contexts": {"nodes": [
        {"__typename": "CheckRun", "name": "Lint", "status": "COMPLETED", "conclusion": "SUCCESS", "detailsUrl": "https://ci/lint"},
        {"__typename": "CheckRun", "name": "Test", "status": "COMPLETED", "conclusion": "FAILURE", "detailsUrl": "https://ci/test"},
        {"__typename": "CheckRun", "name": "Slow", "status": "IN_PROGRESS", "conclusion": "", "detailsUrl": "https://ci/slow"},
        {"__typename": "StatusContext", "context": "legacy", "state": "SUCCESS", "targetUrl": "https://ci/legacy"}
      ]}
    }}}]}
  }}}
}`

// The whole point of the query: one call brings back state, checks, conversation and threads.
func TestPullRequest(t *testing.T) {
	client := answering(t, fullAnswer)

	pullRequest, err := client.PullRequest(t.Context(), "sadeq-n-yazdi", "lac", 31)
	if err != nil {
		t.Fatalf("PullRequest() = %v, want nil", err)
	}

	if pullRequest.Reference() != "sadeq-n-yazdi/lac#31" {
		t.Errorf("Reference() = %q", pullRequest.Reference())
	}
	if pullRequest.State != "open" || pullRequest.Draft {
		t.Errorf("state = %q draft = %v, want an open pull request", pullRequest.State, pullRequest.Draft)
	}
	if pullRequest.Author != "sadeq-n-yazdi" || pullRequest.HeadSHA != "abc123" {
		t.Errorf("author = %q head = %q", pullRequest.Author, pullRequest.HeadSHA)
	}
	if len(pullRequest.Comments) != 1 || pullRequest.Comments[0].Body != "looks good" {
		t.Errorf("comments = %+v", pullRequest.Comments)
	}
	if len(pullRequest.Reviews) != 1 || pullRequest.Reviews[0].State != "approved" {
		t.Errorf("reviews = %+v", pullRequest.Reviews)
	}
}

// A failing check is the thing somebody has to act on, so it must be named rather than folded into
// a count, and one pending check must not read as success.
func TestChecksAreSummarisedAndNamed(t *testing.T) {
	client := answering(t, fullAnswer)

	pullRequest, err := client.PullRequest(t.Context(), "sadeq-n-yazdi", "lac", 31)
	if err != nil {
		t.Fatalf("PullRequest() = %v, want nil", err)
	}

	checks := pullRequest.Checks
	if checks.Total != 4 {
		t.Errorf("Total = %d, want 4", checks.Total)
	}
	if checks.Failed != 1 || checks.Pending != 1 || checks.Passed != 2 {
		t.Errorf("passed/failed/pending = %d/%d/%d, want 2/1/1", checks.Passed, checks.Failed, checks.Pending)
	}
	if checks.State != "failure" {
		t.Errorf("State = %q, want failure", checks.State)
	}

	failing := checks.Failing()
	if len(failing) != 1 || failing[0].Name != "Test" {
		t.Errorf("Failing() = %+v, want the Test check", failing)
	}
}

// "How much is left to deal with" is the number an author actually wants: resolved threads are
// done, and outdated ones refer to code that has since changed.
func TestUnresolvedThreadsExcludesResolvedAndOutdated(t *testing.T) {
	client := answering(t, fullAnswer)

	pullRequest, err := client.PullRequest(t.Context(), "sadeq-n-yazdi", "lac", 31)
	if err != nil {
		t.Fatalf("PullRequest() = %v, want nil", err)
	}

	if len(pullRequest.Threads) != 3 {
		t.Fatalf("got %d threads, want 3", len(pullRequest.Threads))
	}
	if unresolved := pullRequest.UnresolvedThreads(); unresolved != 1 {
		t.Errorf("UnresolvedThreads() = %d, want 1", unresolved)
	}

	// The summary is what identifies a thread to a person: the first line, not the whole comment.
	if summary := pullRequest.Threads[0].Summary(); summary != "this leaks a file handle" {
		t.Errorf("Summary() = %q, want the first line only", summary)
	}
}

// A merged pull request is not the same as an abandoned one, and "closed" alone hides that.
func TestMergedIsItsOwnState(t *testing.T) {
	client := answering(t, `{"data":{"repository":{"pullRequest":{
		"number":1,"state":"CLOSED","merged":true,"author":{"login":"a"},
		"comments":{"nodes":[]},"reviews":{"nodes":[]},"reviewThreads":{"nodes":[]},
		"commits":{"nodes":[]}}}}}`)

	pullRequest, err := client.PullRequest(t.Context(), "owner", "repo", 1)
	if err != nil {
		t.Fatalf("PullRequest() = %v, want nil", err)
	}
	if pullRequest.State != "merged" {
		t.Errorf("State = %q, want merged", pullRequest.State)
	}
}

// A pull request with no CI is not a pull request whose CI failed.
func TestNoChecksIsUnknownRatherThanFailure(t *testing.T) {
	client := answering(t, `{"data":{"repository":{"pullRequest":{
		"number":1,"state":"OPEN","author":{"login":"a"},
		"comments":{"nodes":[]},"reviews":{"nodes":[]},"reviewThreads":{"nodes":[]},
		"commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}}}}}`)

	pullRequest, err := client.PullRequest(t.Context(), "owner", "repo", 1)
	if err != nil {
		t.Fatalf("PullRequest() = %v, want nil", err)
	}
	if pullRequest.Checks.State != "unknown" {
		t.Errorf("Checks.State = %q, want unknown", pullRequest.Checks.State)
	}
}

// A deleted account comes back as null, and must not crash the watcher.
func TestADeletedAuthorIsHandled(t *testing.T) {
	client := answering(t, `{"data":{"repository":{"pullRequest":{
		"number":1,"state":"OPEN","author":null,
		"comments":{"nodes":[{"id":"c","author":null,"body":"hello","createdAt":""}]},
		"reviews":{"nodes":[]},"reviewThreads":{"nodes":[]},"commits":{"nodes":[]}}}}}`)

	pullRequest, err := client.PullRequest(t.Context(), "owner", "repo", 1)
	if err != nil {
		t.Fatalf("PullRequest() = %v, want nil", err)
	}
	if pullRequest.Author != "ghost" || pullRequest.Comments[0].Author != "ghost" {
		t.Errorf("a deleted account was not handled: %+v", pullRequest)
	}
}

// Telling a temporary outage from a permanent mistake is the difference between retrying forever
// and giving up on a pull request that is fine.
func TestFailuresAreClassified(t *testing.T) {
	tests := map[string]struct {
		script string
		want   error
	}{
		"rate limited": {
			script: `echo "API rate limit exceeded for user" >&2; exit 1`,
			want:   github.ErrUnavailable,
		},
		"github is down": {
			script: `echo "HTTP 503: Service Unavailable" >&2; exit 1`,
			want:   github.ErrUnavailable,
		},
		"no network": {
			script: `echo "dial tcp: lookup api.github.com: no such host" >&2; exit 1`,
			want:   github.ErrUnavailable,
		},
		"no such pull request": {
			script: `echo "GraphQL: Could not resolve to a Repository with the name 'x/y'." >&2; exit 1`,
			want:   github.ErrNotFound,
		},
		"not logged in": {
			script: `echo "gh: To use GitHub CLI in a GitHub Actions workflow, set the GH_TOKEN environment variable. authentication required" >&2; exit 4`,
			want:   github.ErrNoCLI,
		},
		"something nobody anticipated": {
			script: `echo "the flux capacitor is misaligned" >&2; exit 1`,
			// Unrecognised failures are treated as transient: a watcher that gives up permanently
			// on something unexpected is worse than one that keeps trying quietly.
			want: github.ErrUnavailable,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			client := stubGH(t, test.script)

			_, err := client.PullRequest(t.Context(), "owner", "repo", 1)
			if !errors.Is(err, test.want) {
				t.Errorf("PullRequest() = %v, want %v", err, test.want)
			}
		})
	}
}

// GitHub answers a missing pull request with a GraphQL error and exit status zero, which is easy to
// mistake for success.
func TestAGraphQLNotFoundIsNotFound(t *testing.T) {
	client := answering(t, `{"data":{"repository":null},
		"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository"}]}`)

	_, err := client.PullRequest(t.Context(), "owner", "repo", 1)
	if !errors.Is(err, github.ErrNotFound) {
		t.Errorf("PullRequest() = %v, want ErrNotFound", err)
	}
}

// A missing gh must say what to install, not fail obscurely.
func TestAMissingCLIExplainsItself(t *testing.T) {
	client := github.New(github.Options{Executable: "gh-that-is-not-installed"})

	err := client.Available(t.Context())
	if !errors.Is(err, github.ErrNoCLI) {
		t.Fatalf("Available() = %v, want ErrNoCLI", err)
	}
	if !strings.Contains(err.Error(), "cli.github.com") {
		t.Errorf("the error does not say where to get it: %v", err)
	}
}

func TestAvailableChecksTheLogin(t *testing.T) {
	loggedIn := stubGH(t, `exit 0`)
	if err := loggedIn.Available(t.Context()); err != nil {
		t.Errorf("Available() with a working gh = %v, want nil", err)
	}

	loggedOut := stubGH(t, `echo "You are not logged into any GitHub hosts. To log in, run: gh auth login" >&2; exit 1`)

	err := loggedOut.Available(t.Context())
	if !errors.Is(err, github.ErrNoCLI) {
		t.Fatalf("Available() when logged out = %v, want ErrNoCLI", err)
	}
	if !strings.Contains(err.Error(), "gh auth login") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

// A person writes a reference in whichever form is to hand: the short one, or a pasted URL.
func TestParseReference(t *testing.T) {
	tests := map[string]struct {
		owner      string
		repository string
		number     int
		wantErr    bool
	}{
		"sadeq-n-yazdi/lac#31":                               {"sadeq-n-yazdi", "lac", 31, false},
		"  sadeq-n-yazdi/lac#31  ":                           {"sadeq-n-yazdi", "lac", 31, false},
		"https://github.com/sadeq-n-yazdi/lac/pull/31":       {"sadeq-n-yazdi", "lac", 31, false},
		"https://github.com/sadeq-n-yazdi/lac/pull/31/":      {"sadeq-n-yazdi", "lac", 31, false},
		"https://github.com/sadeq-n-yazdi/lac/pull/31/files": {"sadeq-n-yazdi", "lac", 31, false},
		"lac#31":         {wantErr: true},
		"sadeq/lac":      {wantErr: true},
		"sadeq/lac#":     {wantErr: true},
		"sadeq/lac#zero": {wantErr: true},
		"sadeq/lac#-1":   {wantErr: true},
		"":               {wantErr: true},
	}

	for reference, want := range tests {
		t.Run(reference, func(t *testing.T) {
			owner, repository, number, err := github.ParseReference(reference)

			if want.wantErr {
				if err == nil {
					t.Fatalf("ParseReference(%q) = %s/%s#%d, want an error", reference, owner, repository, number)
				}
				if !strings.Contains(err.Error(), "owner/repository#number") {
					t.Errorf("the error does not say the expected form: %v", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseReference(%q) = %v, want nil", reference, err)
			}
			if owner != want.owner || repository != want.repository || number != want.number {
				t.Errorf("ParseReference(%q) = %s/%s#%d, want %s/%s#%d",
					reference, owner, repository, number, want.owner, want.repository, want.number)
			}
		})
	}
}

// The query has to ask for everything the watcher tracks, or the rest of it is guesswork.
func TestTheQueryAsksForWhatIsTracked(t *testing.T) {
	recorder := filepath.Join(t.TempDir(), "arguments")
	client := stubGH(t, fmt.Sprintf(`printf '%%s\n' "$@" > %s; echo '{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","message":"x"}]}'`, recorder))

	_, _ = client.PullRequest(t.Context(), "owner", "repo", 1)

	recorded, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatalf("reading the recorded arguments: %v", err)
	}
	query := string(recorded)

	for _, wanted := range []string{
		"statusCheckRollup", "reviewThreads", "isResolved", "isOutdated",
		"comments", "reviews", "isDraft", "merged", "headRefOid",
	} {
		if !strings.Contains(query, wanted) {
			t.Errorf("the query does not ask for %q", wanted)
		}
	}
}
