package github

import (
	"strings"
	"time"
)

// The shapes GitHub's GraphQL answers come back in. They live here rather than in the domain types
// so that a change at GitHub's end is absorbed in one place.

type pullRequestNode struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	State     string `json:"state"`
	IsDraft   bool   `json:"isDraft"`
	Merged    bool   `json:"merged"`
	Mergeable string `json:"mergeable"`
	UpdatedAt string `json:"updatedAt"`
	Author    *actor `json:"author"`
	HeadSHA   string `json:"headRefOid"`

	Comments struct {
		Nodes []commentNode `json:"nodes"`
	} `json:"comments"`

	Reviews struct {
		Nodes []struct {
			ID          string `json:"id"`
			Author      *actor `json:"author"`
			State       string `json:"state"`
			SubmittedAt string `json:"submittedAt"`
		} `json:"nodes"`
	} `json:"reviews"`

	ReviewThreads struct {
		Nodes []struct {
			ID         string `json:"id"`
			IsResolved bool   `json:"isResolved"`
			IsOutdated bool   `json:"isOutdated"`
			Path       string `json:"path"`
			Line       *int   `json:"line"`
			Comments   struct {
				Nodes []commentNode `json:"nodes"`
			} `json:"comments"`
		} `json:"nodes"`
	} `json:"reviewThreads"`

	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State    string `json:"state"`
					Contexts struct {
						Nodes []checkNode `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

type actor struct {
	Login string `json:"login"`
}

func (a *actor) login() string {
	if a == nil {
		return "ghost" // GitHub's own name for a deleted account.
	}

	return a.Login
}

type commentNode struct {
	ID        string `json:"id"`
	Author    *actor `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

func (c commentNode) convert() Comment {
	return Comment{
		ID:        c.ID,
		Author:    c.Author.login(),
		Body:      c.Body,
		CreatedAt: parseTime(c.CreatedAt),
	}
}

// checkNode covers both shapes a check comes in: a CheckRun from GitHub Actions and the like, and a
// StatusContext from an older integration.
type checkNode struct {
	Typename string `json:"__typename"`

	// CheckRun
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"detailsUrl"`

	// StatusContext
	Context   string `json:"context"`
	State     string `json:"state"`
	TargetURL string `json:"targetUrl"`
}

func (c checkNode) convert() Check {
	if c.Typename == "StatusContext" {
		return Check{Name: c.Context, State: strings.ToLower(c.State), URL: c.TargetURL}
	}

	// A check run that has not finished has no conclusion; its status is what matters then.
	state := strings.ToLower(c.Conclusion)
	if state == "" {
		state = "pending"
		if strings.EqualFold(c.Status, "COMPLETED") {
			state = "unknown"
		}
	}

	return Check{Name: c.Name, State: state, URL: c.DetailsURL}
}

func (p *pullRequestNode) convert(owner, repository string) PullRequest {
	pullRequest := PullRequest{
		Owner:      owner,
		Repository: repository,
		Number:     p.Number,
		Title:      p.Title,
		URL:        p.URL,
		Author:     p.Author.login(),
		State:      stateOf(p.State, p.Merged),
		Draft:      p.IsDraft,
		Mergeable:  p.Mergeable,
		HeadSHA:    p.HeadSHA,
		UpdatedAt:  parseTime(p.UpdatedAt),
	}

	for _, comment := range p.Comments.Nodes {
		pullRequest.Comments = append(pullRequest.Comments, comment.convert())
	}

	for _, review := range p.Reviews.Nodes {
		pullRequest.Reviews = append(pullRequest.Reviews, Review{
			ID:          review.ID,
			Author:      review.Author.login(),
			State:       strings.ToLower(review.State),
			SubmittedAt: parseTime(review.SubmittedAt),
		})
	}

	for _, thread := range p.ReviewThreads.Nodes {
		converted := ReviewThread{
			ID:       thread.ID,
			Path:     thread.Path,
			Resolved: thread.IsResolved,
			Outdated: thread.IsOutdated,
		}
		if thread.Line != nil {
			converted.Line = *thread.Line
		}
		for _, comment := range thread.Comments.Nodes {
			converted.Comments = append(converted.Comments, comment.convert())
		}

		pullRequest.Threads = append(pullRequest.Threads, converted)
	}

	pullRequest.Checks = p.checks()

	return pullRequest
}

// checks summarises CI on the head commit.
func (p *pullRequestNode) checks() ChecksSummary {
	summary := ChecksSummary{State: "unknown"}

	if len(p.Commits.Nodes) == 0 || p.Commits.Nodes[0].Commit.StatusCheckRollup == nil {
		// No checks at all is not the same as checks that failed, and saying "unknown" keeps the
		// difference visible.
		return summary
	}

	rollup := p.Commits.Nodes[0].Commit.StatusCheckRollup
	summary.State = strings.ToLower(rollup.State)

	for _, node := range rollup.Contexts.Nodes {
		check := node.convert()
		summary.Checks = append(summary.Checks, check)
		summary.Total++

		switch {
		case check.Failed():
			summary.Failed++
		case check.State == "success", check.State == "neutral", check.State == "skipped":
			summary.Passed++
		default:
			summary.Pending++
		}
	}

	// GitHub's own rollup can lag the individual results; trust what the checks say.
	switch {
	case summary.Failed > 0:
		summary.State = "failure"
	case summary.Pending > 0:
		summary.State = "pending"
	case summary.Total > 0:
		summary.State = "success"
	}

	return summary
}

// stateOf folds GitHub's separate "merged" flag into the state, because "closed" alone hides the
// difference between a pull request that landed and one that was abandoned.
func stateOf(state string, merged bool) string {
	if merged {
		return "merged"
	}

	return strings.ToLower(state)
}

func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}

	return parsed.UTC()
}
