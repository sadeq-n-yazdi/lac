package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// defaultTimeout bounds one call to GitHub. Long enough for a slow network, short enough that a
// hung request does not hold a poll cycle open.
const defaultTimeout = 30 * time.Second

// maxOutput bounds what is read from gh. A pull request with an enormous conversation should be
// truncated rather than allowed to exhaust memory.
const maxOutput = 16 << 20

// The errors callers act on. A transient failure is worth retrying with a longer wait; the others
// mean somebody has to do something.
var (
	// ErrUnavailable means GitHub could not be reached, or answered with something that will
	// probably work later: a rate limit, a timeout, a server error, an outage.
	ErrUnavailable = errors.New("github is not reachable right now")
	// ErrNotFound means the pull request does not exist, or this login cannot see it.
	ErrNotFound = errors.New("no such pull request, or it is not visible to this github login")
	// ErrNoCLI means the gh command is missing or not logged in.
	ErrNoCLI = errors.New("the gh command line is not available")
)

// Client reads pull requests through gh.
type Client struct {
	// executable is the gh command. Tests point it at a stub.
	executable string
	timeout    time.Duration
}

// Options configure a Client.
type Options struct {
	// Executable overrides the gh command, for tests. Empty means "gh", found on PATH.
	Executable string
	// Timeout bounds one call. Zero means thirty seconds.
	Timeout time.Duration
}

// New returns a client. It does not check that gh works until it is used, so a daemon starts
// whether or not the operator has gh installed — the failure arrives when somebody asks for a pull
// request, where it can be explained.
func New(options Options) *Client {
	executable := options.Executable
	if executable == "" {
		executable = "gh"
	}

	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	return &Client{executable: executable, timeout: timeout}
}

// Available reports whether gh is there and logged in, so the daemon can say which of the two is
// wrong rather than leaving somebody to guess.
func (c *Client) Available(ctx context.Context) error {
	if _, err := exec.LookPath(c.executable); err != nil {
		return fmt.Errorf("%w: install it from https://cli.github.com and run `gh auth login`", ErrNoCLI)
	}

	if _, err := c.run(ctx, "auth", "status"); err != nil {
		if errors.Is(err, ErrUnavailable) {
			return err
		}

		return fmt.Errorf("%w: run `gh auth login`", ErrNoCLI)
	}

	return nil
}

// pullRequestQuery asks for everything the watcher tracks in one round trip: the state, the checks
// on the head commit, the conversation, and the review threads with whether they are resolved.
//
// One query rather than several because every extra call is another chance to see a half-changed
// pull request, and another request against a rate limit.
const pullRequestQuery = `
query($owner:String!,$repo:String!,$number:Int!){
  repository(owner:$owner,name:$repo){
    pullRequest(number:$number){
      number title url state isDraft merged mergeable updatedAt
      author{login}
      headRefOid
      comments(last:30){nodes{id author{login} body createdAt}}
      reviews(last:20){nodes{id author{login} state submittedAt}}
      reviewThreads(last:50){nodes{
        id isResolved isOutdated path line
        comments(first:20){nodes{id author{login} body createdAt}}
      }}
      commits(last:1){nodes{commit{statusCheckRollup{
        state
        contexts(last:100){nodes{
          __typename
          ... on CheckRun{name status conclusion detailsUrl}
          ... on StatusContext{context state targetUrl}
        }}
      }}}}
    }
  }
}`

// PullRequest reads one pull request.
func (c *Client) PullRequest(ctx context.Context, owner, repository string, number int) (PullRequest, error) {
	output, err := c.run(ctx, "api", "graphql",
		"-f", "query="+pullRequestQuery,
		"-F", "owner="+owner,
		"-F", "repo="+repository,
		"-F", "number="+strconv.Itoa(number),
	)
	if err != nil {
		return PullRequest{}, err
	}

	var response struct {
		Data struct {
			Repository *struct {
				PullRequest *pullRequestNode `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.Unmarshal(output, &response); err != nil {
		return PullRequest{}, fmt.Errorf("reading github's answer: %w", err)
	}

	if len(response.Errors) > 0 {
		first := response.Errors[0]
		if strings.EqualFold(first.Type, "NOT_FOUND") {
			return PullRequest{}, fmt.Errorf("%w: %s", ErrNotFound, first.Message)
		}

		return PullRequest{}, fmt.Errorf("github refused the query: %s", first.Message)
	}

	if response.Data.Repository == nil || response.Data.Repository.PullRequest == nil {
		return PullRequest{}, ErrNotFound
	}

	return response.Data.Repository.PullRequest.convert(owner, repository), nil
}

// run executes gh and returns its output.
func (c *Client) run(ctx context.Context, arguments ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	command := exec.CommandContext(ctx, c.executable, arguments...) //nolint:gosec // gh, or a path the operator configured

	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	err := command.Run()
	if stdout.Len() > maxOutput {
		return nil, fmt.Errorf("github's answer is %d bytes, which is more than this can handle", stdout.Len())
	}
	if err == nil {
		return stdout.Bytes(), nil
	}

	// A cancelled context is the caller giving up, not GitHub failing.
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w: gh did not answer within %s", ErrUnavailable, c.timeout)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err() //nolint:wrapcheck // the caller compares with context errors
	}

	var missing *exec.Error
	if errors.As(err, &missing) {
		return nil, fmt.Errorf("%w: install it from https://cli.github.com and run `gh auth login`", ErrNoCLI)
	}

	return nil, classify(stdout.Bytes(), stderr.String(), err)
}

// classify decides whether a failure is worth retrying. Getting this wrong in either direction is
// expensive: retrying a bad reference forever, or giving up on a passing outage.
func classify(stdout []byte, stderr string, cause error) error {
	message := strings.ToLower(stderr)
	if message == "" {
		message = strings.ToLower(string(stdout))
	}

	switch {
	case strings.Contains(message, "not found"), strings.Contains(message, "could not resolve to"):
		return fmt.Errorf("%w: %s", ErrNotFound, firstLine(stderr))

	case strings.Contains(message, "authentication"), strings.Contains(message, "not logged in"),
		strings.Contains(message, "gh auth login"), strings.Contains(message, "bad credentials"):
		return fmt.Errorf("%w: %s", ErrNoCLI, firstLine(stderr))

	case strings.Contains(message, "rate limit"), strings.Contains(message, "secondary rate"),
		strings.Contains(message, "abuse detection"):
		return fmt.Errorf("%w: %s", ErrUnavailable, firstLine(stderr))

	case strings.Contains(message, "timeout"), strings.Contains(message, "timed out"),
		strings.Contains(message, "connection refused"), strings.Contains(message, "no such host"),
		strings.Contains(message, "network is unreachable"), strings.Contains(message, "eof"),
		strings.Contains(message, "tls"), strings.Contains(message, "502"),
		strings.Contains(message, "503"), strings.Contains(message, "504"),
		strings.Contains(message, "server error"), strings.Contains(message, "try again"):
		return fmt.Errorf("%w: %s", ErrUnavailable, firstLine(stderr))
	}

	// Anything unrecognised is treated as transient. A watcher that gives up permanently on a
	// failure nobody anticipated is worse than one that keeps trying quietly. ErrUnavailable is
	// what callers match on, so it is the wrapped one; gh's own exit status rides along as text.
	return fmt.Errorf("%w: %s (%s)", ErrUnavailable, firstLine(stderr), cause.Error())
}

func firstLine(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "gh gave no explanation"
	}

	if line, _, found := strings.Cut(trimmed, "\n"); found {
		return line
	}

	return trimmed
}
