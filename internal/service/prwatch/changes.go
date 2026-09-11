package prwatch

import (
	"fmt"
	"strings"

	"sadeq.uk/lac/internal/core"
	"sadeq.uk/lac/internal/github"
)

// changesBetween works out what an agent would want to be told about.
//
// It compares two observations rather than trusting GitHub's updatedAt, which moves for things
// nobody cares about and sometimes does not move for things they do. Each change is one line,
// written for a person: "CI failed: Test", not a diff.
func changesBetween(before, after github.PullRequest, first bool) []core.WatchEvent {
	if first {
		// The first observation is not a change. Telling somebody "CI is passing" the moment they
		// start watching is noise; they asked to hear when something happens.
		return nil
	}

	changes := make([]core.WatchEvent, 0, 4)

	if event, changed := stateChange(before, after); changed {
		changes = append(changes, event)
	}
	if event, changed := checksChange(before, after); changed {
		changes = append(changes, event)
	}
	if before.HeadSHA != after.HeadSHA && after.HeadSHA != "" {
		changes = append(changes, core.WatchEvent{
			Kind:    core.WatchCommits,
			Summary: fmt.Sprintf("pushed: the branch is now at %s", shorten(after.HeadSHA)),
		})
	}

	changes = append(changes, commentChanges(before, after)...)
	changes = append(changes, reviewChanges(before, after)...)
	changes = append(changes, threadChanges(before, after)...)

	return changes
}

func stateChange(before, after github.PullRequest) (core.WatchEvent, bool) {
	switch {
	case before.State != after.State:
		return core.WatchEvent{
			Kind:    core.WatchState,
			Summary: fmt.Sprintf("%s (was %s)", describeState(after), before.State),
		}, true

	case before.Draft != after.Draft:
		if after.Draft {
			return core.WatchEvent{Kind: core.WatchState, Summary: "marked as a draft"}, true
		}

		return core.WatchEvent{Kind: core.WatchState, Summary: "marked ready for review"}, true
	}

	return core.WatchEvent{}, false
}

func describeState(pullRequest github.PullRequest) string {
	switch pullRequest.State {
	case "merged":
		return "merged"
	case "closed":
		return "closed without merging"
	case "open":
		if pullRequest.Draft {
			return "reopened as a draft"
		}

		return "reopened"
	default:
		return pullRequest.State
	}
}

// checksChange reports CI moving between states, and names what failed — the name is the whole
// point, since "CI failed" sends somebody to look it up.
func checksChange(before, after github.PullRequest) (core.WatchEvent, bool) {
	if before.Checks.State == after.Checks.State {
		return core.WatchEvent{}, false
	}

	switch after.Checks.State {
	case "failure":
		names := make([]string, 0, len(after.Checks.Failing()))
		for _, check := range after.Checks.Failing() {
			names = append(names, check.Name)
		}

		return core.WatchEvent{
			Kind:    core.WatchChecks,
			Summary: "CI failed: " + strings.Join(names, ", "),
		}, true

	case "success":
		return core.WatchEvent{
			Kind:    core.WatchChecks,
			Summary: fmt.Sprintf("CI passed (%d checks)", after.Checks.Total),
		}, true

	case "pending":
		return core.WatchEvent{Kind: core.WatchChecks, Summary: "CI is running"}, true

	default:
		return core.WatchEvent{
			Kind:    core.WatchChecks,
			Summary: "CI state is now " + after.Checks.State,
		}, true
	}
}

func commentChanges(before, after github.PullRequest) []core.WatchEvent {
	seen := make(map[string]struct{}, len(before.Comments))
	for _, comment := range before.Comments {
		seen[comment.ID] = struct{}{}
	}

	changes := make([]core.WatchEvent, 0, 2)
	for _, comment := range after.Comments {
		if _, known := seen[comment.ID]; known {
			continue
		}

		changes = append(changes, core.WatchEvent{
			Kind:    core.WatchComment,
			Summary: fmt.Sprintf("%s commented: %s", comment.Author, firstLine(comment.Body)),
		})
	}

	return changes
}

func reviewChanges(before, after github.PullRequest) []core.WatchEvent {
	seen := make(map[string]struct{}, len(before.Reviews))
	for _, review := range before.Reviews {
		seen[review.ID] = struct{}{}
	}

	changes := make([]core.WatchEvent, 0, 2)
	for _, review := range after.Reviews {
		if _, known := seen[review.ID]; known {
			continue
		}
		// A review with no state of its own is just the wrapper around inline comments; the
		// comments themselves are reported as threads.
		if review.State == "commented" || review.State == "" {
			continue
		}

		changes = append(changes, core.WatchEvent{
			Kind:    core.WatchReview,
			Summary: fmt.Sprintf("%s %s the pull request", review.Author, describeReview(review.State)),
		})
	}

	return changes
}

func describeReview(state string) string {
	switch state {
	case "approved":
		return "approved"
	case "changes_requested":
		return "requested changes on"
	case "dismissed":
		return "had a review dismissed on"
	default:
		return "reviewed"
	}
}

// threadChanges reports review conversations appearing, gaining replies, and being resolved.
//
// Resolution is the one an author waits for: it is how they know a round of review is finished.
func threadChanges(before, after github.PullRequest) []core.WatchEvent {
	previous := make(map[string]github.ReviewThread, len(before.Threads))
	for _, thread := range before.Threads {
		previous[thread.ID] = thread
	}

	changes := make([]core.WatchEvent, 0, 2)

	for _, thread := range after.Threads {
		known, existed := previous[thread.ID]

		switch {
		case !existed:
			changes = append(changes, core.WatchEvent{
				Kind:    core.WatchThread,
				Summary: fmt.Sprintf("new review comment on %s: %s", location(thread), thread.Summary()),
			})

		case !known.Resolved && thread.Resolved:
			changes = append(changes, core.WatchEvent{
				Kind:    core.WatchThread,
				Summary: fmt.Sprintf("review comment resolved on %s: %s", location(thread), thread.Summary()),
			})

		case known.Resolved && !thread.Resolved:
			changes = append(changes, core.WatchEvent{
				Kind:    core.WatchThread,
				Summary: fmt.Sprintf("review comment reopened on %s: %s", location(thread), thread.Summary()),
			})

		case len(thread.Comments) > len(known.Comments):
			latest := thread.Comments[len(thread.Comments)-1]
			changes = append(changes, core.WatchEvent{
				Kind: core.WatchThread,
				Summary: fmt.Sprintf("%s replied on %s: %s",
					latest.Author, location(thread), firstLine(latest.Body)),
			})
		}
	}

	return changes
}

func location(thread github.ReviewThread) string {
	if thread.Path == "" {
		return "the pull request"
	}
	if thread.Line > 0 {
		return fmt.Sprintf("%s:%d", thread.Path, thread.Line)
	}

	return thread.Path
}

// firstLine keeps a summary to one line and a sensible length: these end up in a message an agent
// reads, not in a log nobody opens.
func firstLine(body string) string {
	const limit = 140

	text := strings.TrimSpace(body)
	if line, _, found := strings.Cut(text, "\n"); found {
		text = strings.TrimSpace(line)
	}

	if len(text) > limit {
		return text[:limit] + "…"
	}

	return text
}

func shorten(sha string) string {
	const short = 7

	if len(sha) <= short {
		return sha
	}

	return sha[:short]
}
