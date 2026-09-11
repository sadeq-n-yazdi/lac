# Watching pull requests

An agent that has just opened a pull request should not sit there refreshing GitHub, and a person
should not have to relay "CI went red" between sessions. Hand the pull request to LAC instead: it
watches, and tells whoever subscribed when something actually changes.

```sh
lac pr watch huma-engineering/product-store#406
lac pr status huma-engineering/product-store#406 --refresh --threads
lac pr list
lac pr unwatch huma-engineering/product-store#406
```

From an AI session the same thing is `lac_watch_pr` and `lac_pr_status`.

## What it notices

Each poll is compared with the last one, and the difference becomes an event with a sentence a person
can read:

| Kind          | Example                                                   |
|---------------|-----------------------------------------------------------|
| `checks`      | `CI failed: Build and test (macos-latest)`                 |
| `state`       | `merged`, `closed`, `marked as a draft`, `ready for review`|
| `comment`     | somebody commented on the pull request                     |
| `review`      | a review was submitted                                     |
| `thread`      | `review comment resolved on main.go:12`                    |
| `commits`     | `pushed: the branch is now at abc1234`                     |
| `unreachable` | GitHub could not be reached — recorded once, not per attempt |
| `recovered`   | GitHub answered again                                      |

Review conversations are kept as threads, so "four conversations waiting on somebody" is a number
rather than an impression, and `--threads` prints them in full.

## When GitHub is down

The watcher keeps the **last good observation** and serves it, marked stale, rather than returning an
error. An agent asking during an outage gets the last thing that was true, and is told how old it is.

Failures back off exponentially with jitter, up to fifteen minutes. An outage is reported to
subscribers once, not on every attempt, and recovery is reported when it answers again. A pull
request that has settled — merged or closed — drops to a half-hourly poll.

## More than one GitHub account

`gh` keeps one **active** account and does not choose one by directory. Standing in a repository that
belongs to another login does not make `gh` use that login, so on a machine where one account owns the
work repositories and another owns the personal ones, the active account cannot see half of them —
they come back as "not found", which is indistinguishable from a typo until you know this.

LAC handles it: the first time it reads a pull request it tries the active account, then each other
login `gh auth status` reports, and remembers the one that could see it. Every later request for that
pull request is made with that account's token, in that request's environment — your own shell's
active account is never changed underneath you.

Only a genuine "not found" moves on to the next account. An outage is never mistaken for a visibility
problem, because retrying every account on every network failure would turn one unreachable moment
into several.

Nothing is needed from you beyond `gh auth login` for each account. To check what LAC can see:

```sh
gh auth status          # the logins it will try, active one first
lac pr status owner/repository#1 --refresh
```

## Requirements and limits

- The `gh` CLI must be installed and authenticated, and must be on the daemon's `PATH` — see
  [running.md](running.md), since a service manager does not pass your shell's environment through.
- Only GitHub is supported today.
- Changes are delivered to the agents that **subscribed**. A one-shot shell command registers a
  passing identity and lets it go, so `lac pr watch` from a terminal records the watch but has nowhere
  to deliver to; it says so, and tells you to read `lac pr status` or register a lasting identity
  with `lac --name me register --save`.
- The watcher appears on the roster as `pr-watcher`. It is one of the daemon's own components rather
  than a session, so it is not asked to report — nobody is behind it to answer.
