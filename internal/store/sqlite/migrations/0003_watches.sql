-- Watched pull requests.
--
-- The last good observation is kept in full, so an agent asking "what is happening with my pull
-- request?" during a GitHub outage gets the last thing that was true, marked as old, rather than an
-- error. That is the difference between a watcher and a proxy.

CREATE TABLE watches (
    id            TEXT    PRIMARY KEY,
    owner         TEXT    NOT NULL,
    repository    TEXT    NOT NULL,
    number        INTEGER NOT NULL,
    -- The last good observation, as JSON. Everything below it is denormalised from this for
    -- listing and for noticing changes without decoding it.
    snapshot      TEXT    NOT NULL DEFAULT '',
    state         TEXT    NOT NULL DEFAULT 'unknown',
    draft         INTEGER NOT NULL DEFAULT 0,
    title         TEXT    NOT NULL DEFAULT '',
    checks_state  TEXT    NOT NULL DEFAULT 'unknown',
    unresolved    INTEGER NOT NULL DEFAULT 0,
    -- observed_at is when GitHub last answered. Its distance from now is how stale this is.
    observed_at   INTEGER,
    -- failures counts consecutive failures, which is what the backoff is calculated from.
    failures      INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT    NOT NULL DEFAULT '',
    last_error_at INTEGER,
    next_poll_at  INTEGER NOT NULL,
    created_at    INTEGER NOT NULL,
    UNIQUE (owner, repository, number)
) STRICT;

-- The poller asks for what is due, oldest first.
CREATE INDEX watches_due ON watches (next_poll_at);

-- Who wants to hear about a pull request. A watch with no subscribers is left alone rather than
-- deleted: the history is worth keeping, and somebody usually comes back to it.
CREATE TABLE watch_subscribers (
    watch_id      TEXT    NOT NULL REFERENCES watches (id) ON DELETE CASCADE,
    agent_id      TEXT    NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    subscribed_at INTEGER NOT NULL,
    PRIMARY KEY (watch_id, agent_id)
) STRICT;

CREATE INDEX watch_subscribers_agent ON watch_subscribers (agent_id);

-- What changed, and when. An agent that was away reads this rather than trying to work out what it
-- missed by comparing two snapshots.
CREATE TABLE watch_events (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    watch_id TEXT    NOT NULL REFERENCES watches (id) ON DELETE CASCADE,
    kind     TEXT    NOT NULL,
    summary  TEXT    NOT NULL,
    at       INTEGER NOT NULL
) STRICT;

CREATE INDEX watch_events_recent ON watch_events (watch_id, id DESC);
