// Package github reads pull requests through the gh command-line tool.
//
// It shells out to gh rather than talking to api.github.com directly, so LAC never holds a GitHub
// credential: the operator is already logged in, and whatever that login can see is exactly what
// LAC can see. There is nothing extra to store, rotate or leak.
package github

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PullRequest is everything the watcher tracks about one pull request.
type PullRequest struct {
	Owner      string
	Repository string
	Number     int

	Title  string
	URL    string
	Author string
	// State is open, closed or merged. GitHub reports merged separately from closed; this does not.
	State string
	Draft bool
	// Mergeable is GitHub's view: MERGEABLE, CONFLICTING or UNKNOWN.
	Mergeable string
	HeadSHA   string
	UpdatedAt time.Time

	// Checks is the state of CI on the head commit.
	Checks ChecksSummary
	// Comments are the conversation on the pull request itself, newest last.
	Comments []Comment
	// Reviews are the submitted reviews, newest last.
	Reviews []Review
	// Threads are the review conversations on the diff, resolved or not.
	Threads []ReviewThread
}

// Reference is how a pull request is named: owner/repository#number.
func (p PullRequest) Reference() string {
	return fmt.Sprintf("%s/%s#%d", p.Owner, p.Repository, p.Number)
}

// UnresolvedThreads counts the review conversations still waiting on somebody. It is the number an
// author actually wants: how much is left to deal with.
func (p PullRequest) UnresolvedThreads() int {
	unresolved := 0
	for _, thread := range p.Threads {
		if !thread.Resolved && !thread.Outdated {
			unresolved++
		}
	}

	return unresolved
}

// ChecksSummary is the state of CI on the head commit.
type ChecksSummary struct {
	// State is success, failure, pending, or unknown when GitHub reports nothing at all.
	State string
	// Total, Passed, Failed and Pending count the individual checks.
	Total   int
	Passed  int
	Failed  int
	Pending int
	// Checks are the individual results, so a failure can be named rather than merely counted.
	Checks []Check
}

// Failing returns the checks that did not pass, which is the part anybody reads first.
func (s ChecksSummary) Failing() []Check {
	failing := make([]Check, 0, s.Failed)
	for _, check := range s.Checks {
		if check.Failed() {
			failing = append(failing, check)
		}
	}

	return failing
}

// Check is one CI result.
type Check struct {
	Name string
	// State is success, failure, pending, skipped or cancelled.
	State string
	URL   string
}

// Failed reports whether this check is one somebody has to do something about.
func (c Check) Failed() bool {
	switch c.State {
	case "failure", "cancelled", "timed_out", "action_required", "startup_failure", "stale":
		return true
	default:
		return false
	}
}

// Comment is one message on the pull request.
type Comment struct {
	ID        string
	Author    string
	Body      string
	CreatedAt time.Time
}

// Review is one submitted review.
type Review struct {
	ID          string
	Author      string
	State       string
	SubmittedAt time.Time
}

// ReviewThread is one conversation on the diff — a review comment and the replies to it.
type ReviewThread struct {
	ID   string
	Path string
	Line int
	// Resolved is true once somebody marked the conversation resolved on GitHub.
	Resolved bool
	// Outdated is true when the code it refers to has changed since.
	Outdated bool
	Comments []Comment
}

// Summary is the first line of the thread, which is what identifies it to a person.
func (t ReviewThread) Summary() string {
	if len(t.Comments) == 0 {
		return ""
	}

	body := strings.TrimSpace(t.Comments[0].Body)
	if line, _, found := strings.Cut(body, "\n"); found {
		return line
	}

	return body
}

// ParseReference reads owner/repository#number, the way a person writes it.
//
// It also accepts a full URL, because that is what somebody pastes from a browser.
func ParseReference(reference string) (owner, repository string, number int, err error) {
	value := strings.TrimSpace(reference)

	// A pasted URL: https://github.com/owner/repo/pull/123
	if index := strings.Index(value, "github.com/"); index >= 0 {
		value = value[index+len("github.com/"):]
		value = strings.ReplaceAll(value, "/pull/", "#")
		value = strings.TrimSuffix(value, "/")
		if cut := strings.Index(value, "#"); cut >= 0 {
			if end := strings.IndexAny(value[cut:], "/?"); end > 0 {
				value = value[:cut+end]
			}
		}
	}

	repository, digits, found := strings.Cut(value, "#")
	if !found {
		return "", "", 0, fmt.Errorf("%q is not a pull request; write it as owner/repository#number", reference)
	}

	owner, repository, found = strings.Cut(repository, "/")
	if !found || owner == "" || repository == "" {
		return "", "", 0, fmt.Errorf("%q is missing the owner; write it as owner/repository#number", reference)
	}

	number, convertErr := strconv.Atoi(digits)
	if convertErr != nil || number <= 0 {
		return "", "", 0, fmt.Errorf("%q has no pull request number; write it as owner/repository#number", reference)
	}

	return owner, repository, number, nil
}
