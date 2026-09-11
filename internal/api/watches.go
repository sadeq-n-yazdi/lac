package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"sadeq.uk/lac/internal/core"
	"sadeq.uk/lac/internal/github"
	"sadeq.uk/lac/internal/transport/jsonrpc"
)

// WatchView is a watched pull request as clients see it.
type WatchView struct {
	ID         string `json:"id"`
	Reference  string `json:"reference"`
	Title      string `json:"title,omitempty"`
	URL        string `json:"url,omitempty"`
	State      string `json:"state"`
	Draft      bool   `json:"draft"`
	Checks     string `json:"checks"`
	Unresolved int    `json:"unresolved_threads"`
	// ObservedAt is when GitHub last answered, and Stale says whether that is long enough ago to
	// matter. An agent acting on old information should know it is doing so.
	ObservedAt string `json:"observed_at,omitempty"`
	Stale      bool   `json:"stale"`
	// LastError is why the most recent attempt failed, if it did.
	LastError string `json:"last_error,omitempty"`
	Failures  int    `json:"failures,omitempty"`
}

// WatchDetail is a watch with everything known about the pull request.
type WatchDetail struct {
	Watch WatchView `json:"watch"`
	// Checks are the individual CI results.
	Checks []CheckView `json:"checks,omitempty"`
	// Threads are the review conversations, resolved or not.
	Threads []ThreadView `json:"threads,omitempty"`
	// Comments are the messages on the pull request itself, newest last.
	Comments []PullCommentView `json:"comments,omitempty"`
	// Changes are the recent events, newest first.
	Changes []WatchEventView `json:"changes,omitempty"`
}

// CheckView is one CI result.
type CheckView struct {
	Name  string `json:"name"`
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
}

// ThreadView is one review conversation on the diff.
type ThreadView struct {
	ID       string            `json:"id"`
	Path     string            `json:"path,omitempty"`
	Line     int               `json:"line,omitempty"`
	Resolved bool              `json:"resolved"`
	Outdated bool              `json:"outdated"`
	Comments []PullCommentView `json:"comments"`
}

// PullCommentView is one comment, on the pull request or in a review conversation.
type PullCommentView struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at,omitempty"`
}

// WatchEventView is one change that was noticed.
type WatchEventView struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
	At      string `json:"at"`
}

// WatchParams names a pull request, the way a person writes it: owner/repository#number, or a
// pasted URL.
type WatchParams struct {
	PullRequest string `json:"pull_request"`
}

// WatchResult is one watch.
type WatchResult struct {
	Watch WatchView `json:"watch"`
}

func (a *API) handleWatchPullRequest(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	if a.watcher == nil {
		return nil, errors.New("this daemon is not watching pull requests")
	}

	var arguments WatchParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	owner, repository, number, err := github.ParseReference(arguments.PullRequest)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", core.ErrInvalidArgument, err)
	}

	watch, err := a.watcher.Watch(ctx, caller.ID, owner, repository, number)
	if err != nil {
		return nil, err
	}

	return WatchResult{Watch: a.viewOfWatch(watch)}, nil
}

// WatchIDParams names a watch, either by its id or by the pull request it follows.
type WatchIDParams struct {
	WatchID     string `json:"watch_id,omitempty"`
	PullRequest string `json:"pull_request,omitempty"`
	// Wait blocks until the pull request is next read, for a caller that has just pushed and wants
	// the next observation rather than the last one.
	Wait bool `json:"wait,omitempty"`
	// Refresh reads GitHub now instead of reporting what is already known.
	Refresh bool `json:"refresh,omitempty"`
}

// resolveWatch finds the watch a request is about.
func (a *API) resolveWatch(ctx context.Context, arguments WatchIDParams) (core.Watch, error) {
	switch {
	case arguments.WatchID != "":
		return a.watcher.ByID(ctx, arguments.WatchID)

	case arguments.PullRequest != "":
		owner, repository, number, err := github.ParseReference(arguments.PullRequest)
		if err != nil {
			return core.Watch{}, fmt.Errorf("%w: %w", core.ErrInvalidArgument, err)
		}

		return a.watcher.ByReference(ctx, owner, repository, number)

	default:
		return core.Watch{}, fmt.Errorf("%w: which pull request? give pull_request or watch_id",
			core.ErrInvalidArgument)
	}
}

func (a *API) handleWatchStatus(
	ctx context.Context, _ core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	if a.watcher == nil {
		return nil, errors.New("this daemon is not watching pull requests")
	}

	var arguments WatchIDParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	watch, err := a.resolveWatch(ctx, arguments)
	if err != nil {
		return nil, err
	}

	switch {
	case arguments.Refresh:
		if watch, err = a.watcher.Refresh(ctx, watch.ID); err != nil {
			// A failed refresh is not a failed request: the last good answer is still worth having,
			// and the view says how old it is and why the attempt failed.
			if watch, err = a.watcher.ByID(ctx, watch.ID); err != nil {
				return nil, err
			}
		}

	case arguments.Wait:
		if watch, err = a.watcher.Await(ctx, watch.ID); err != nil {
			return nil, err
		}
	}

	return a.detailOf(ctx, watch), nil
}

// WatchListParams narrows a listing.
type WatchListParams struct {
	// All lists every watch on the machine rather than only the caller's.
	All bool `json:"all,omitempty"`
}

// WatchListResult is the watches.
type WatchListResult struct {
	Watches []WatchView `json:"watches"`
}

func (a *API) handleWatchList(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	if a.watcher == nil {
		return nil, errors.New("this daemon is not watching pull requests")
	}

	var arguments WatchListParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	list := a.watcher.Mine
	if arguments.All {
		list = func(ctx context.Context, _ string) ([]core.Watch, error) { return a.watcher.All(ctx) }
	}

	watches, err := list(ctx, caller.ID)
	if err != nil {
		return nil, err
	}

	views := make([]WatchView, 0, len(watches))
	for index := range watches {
		views = append(views, a.viewOfWatch(watches[index]))
	}

	return WatchListResult{Watches: views}, nil
}

// UnwatchResult confirms the caller has stopped listening.
type UnwatchResult struct {
	Watching bool `json:"watching"`
}

func (a *API) handleUnwatch(
	ctx context.Context, caller core.Agent, _ *jsonrpc.Session, params json.RawMessage,
) (any, error) {
	if a.watcher == nil {
		return nil, errors.New("this daemon is not watching pull requests")
	}

	var arguments WatchIDParams
	if err := jsonrpc.ParseParams(params, &arguments); err != nil {
		return nil, err
	}

	watch, err := a.resolveWatch(ctx, arguments)
	if err != nil {
		return nil, err
	}

	if err := a.watcher.Unwatch(ctx, caller.ID, watch.ID); err != nil {
		return nil, err
	}

	return UnwatchResult{Watching: false}, nil
}

func (a *API) viewOfWatch(watch core.Watch) WatchView {
	view := WatchView{
		ID:         watch.ID,
		Reference:  watch.Reference(),
		Title:      watch.Title,
		State:      watch.State,
		Draft:      watch.Draft,
		Checks:     watch.ChecksState,
		Unresolved: watch.Unresolved,
		ObservedAt: formatTime(watch.ObservedAt),
		Stale:      a.watcher.Stale(watch),
		LastError:  watch.LastError,
		Failures:   watch.Failures,
	}

	if observation, known := a.watcher.Snapshot(watch); known {
		view.URL = observation.URL
	}

	return view
}

// detailOf builds the full picture: the state, the checks, the conversations and what changed.
func (a *API) detailOf(ctx context.Context, watch core.Watch) WatchDetail {
	detail := WatchDetail{Watch: a.viewOfWatch(watch)}

	if observation, known := a.watcher.Snapshot(watch); known {
		for _, check := range observation.Checks.Checks {
			detail.Checks = append(detail.Checks, CheckView{
				Name: check.Name, State: check.State, URL: check.URL,
			})
		}

		for _, thread := range observation.Threads {
			detail.Threads = append(detail.Threads, ThreadView{
				ID:       thread.ID,
				Path:     thread.Path,
				Line:     thread.Line,
				Resolved: thread.Resolved,
				Outdated: thread.Outdated,
				Comments: viewsOfPullComments(thread.Comments),
			})
		}

		detail.Comments = viewsOfPullComments(observation.Comments)
	}

	changes, err := a.watcher.Events(ctx, watch.ID, 20)
	if err == nil {
		for _, change := range changes {
			detail.Changes = append(detail.Changes, WatchEventView{
				ID:      change.ID,
				Kind:    string(change.Kind),
				Summary: change.Summary,
				At:      formatTime(change.At),
			})
		}
	}

	return detail
}

func viewsOfPullComments(comments []github.Comment) []PullCommentView {
	views := make([]PullCommentView, 0, len(comments))
	for _, comment := range comments {
		views = append(views, PullCommentView{
			Author:    comment.Author,
			Body:      comment.Body,
			CreatedAt: formatTime(comment.CreatedAt),
		})
	}

	return views
}
