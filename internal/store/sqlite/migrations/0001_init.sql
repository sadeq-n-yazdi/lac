-- Initial LAC schema.
--
-- Every instant is stored as an integer count of microseconds since the Unix epoch, UTC. SQLite
-- has no time type, and a fixed integer keeps ordering, indexing and comparison unambiguous. A
-- NULL instant means "has not happened yet" — not released, not acknowledged, not revoked.

CREATE TABLE agents (
    id                TEXT    PRIMARY KEY,
    name              TEXT    NOT NULL,
    kind              TEXT    NOT NULL,
    workdir           TEXT    NOT NULL,
    process_id        INTEGER NOT NULL,
    capabilities      TEXT    NOT NULL, -- JSON, owned by internal/store/sqlite
    state             TEXT    NOT NULL CHECK (state IN ('active', 'stale', 'deregistered')),
    registered_at     INTEGER NOT NULL,
    last_heartbeat_at INTEGER NOT NULL
) STRICT;

-- A name identifies one live agent at a time. Stale and deregistered records keep their rows for
-- the audit trail but release the name, so an agent that comes back can register again.
CREATE UNIQUE INDEX agents_active_name ON agents (name) WHERE state = 'active';
CREATE INDEX agents_state ON agents (state, last_heartbeat_at);

CREATE TABLE credentials (
    agent_id   TEXT    PRIMARY KEY REFERENCES agents (id) ON DELETE CASCADE,
    token_hash BLOB    NOT NULL UNIQUE,
    issued_at  INTEGER NOT NULL,
    revoked_at INTEGER
) STRICT;

CREATE TABLE messages (
    id            TEXT    PRIMARY KEY,
    from_agent_id TEXT    NOT NULL,
    to_agent_id   TEXT,
    topic         TEXT,
    kind          TEXT    NOT NULL,
    body          BLOB    NOT NULL,
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER,
    -- Exactly one destination: a direct recipient or a topic, never both and never neither.
    CHECK ((to_agent_id IS NULL) <> (topic IS NULL))
) STRICT;

CREATE INDEX messages_expiry ON messages (expires_at) WHERE expires_at IS NOT NULL;

-- One row per recipient. A message is retained until every recipient acknowledges it, so an agent
-- that restarts still receives what it missed.
CREATE TABLE deliveries (
    message_id   TEXT    NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    agent_id     TEXT    NOT NULL,
    delivered_at INTEGER,
    acked_at     INTEGER,
    PRIMARY KEY (message_id, agent_id)
) STRICT;

CREATE INDEX deliveries_pending ON deliveries (agent_id, acked_at);

CREATE TABLE subscriptions (
    agent_id     TEXT    NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    topic        TEXT    NOT NULL,
    subscribed_at INTEGER NOT NULL,
    PRIMARY KEY (agent_id, topic)
) STRICT;

CREATE INDEX subscriptions_topic ON subscriptions (topic);

CREATE TABLE resources (
    name            TEXT    PRIMARY KEY,
    capacity        INTEGER NOT NULL CHECK (capacity > 0),
    lease_ttl_micros INTEGER NOT NULL CHECK (lease_ttl_micros > 0),
    description     TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL
) STRICT;

CREATE TABLE queue_entries (
    id            TEXT    PRIMARY KEY,
    resource_name TEXT    NOT NULL REFERENCES resources (name) ON DELETE CASCADE,
    agent_id      TEXT    NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    priority      INTEGER NOT NULL DEFAULT 0,
    reason        TEXT    NOT NULL DEFAULT '',
    state         TEXT    NOT NULL CHECK (state IN ('waiting', 'granted', 'cancelled', 'expired')),
    requested_at  INTEGER NOT NULL,
    resolved_at   INTEGER
) STRICT;

-- Service order: highest priority first, then earliest request. This index is what makes reading
-- the head of the queue a single index seek rather than a sort.
CREATE INDEX queue_entries_service_order
    ON queue_entries (resource_name, priority DESC, requested_at ASC)
    WHERE state = 'waiting';
CREATE INDEX queue_entries_agent ON queue_entries (agent_id, state);

CREATE TABLE leases (
    id             TEXT    PRIMARY KEY,
    resource_name  TEXT    NOT NULL REFERENCES resources (name) ON DELETE CASCADE,
    agent_id       TEXT    NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    queue_entry_id TEXT    REFERENCES queue_entries (id) ON DELETE SET NULL,
    acquired_at    INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL,
    released_at    INTEGER,
    metadata       BLOB
) STRICT;

-- Counting the slots in use on a resource is the hottest query in the system: it runs inside the
-- transaction that decides whether another agent may start work.
CREATE INDEX leases_held ON leases (resource_name, expires_at) WHERE released_at IS NULL;
CREATE INDEX leases_agent ON leases (agent_id) WHERE released_at IS NULL;

CREATE TABLE report_requests (
    id              TEXT    PRIMARY KEY,
    requester_id    TEXT    NOT NULL,
    question        TEXT    NOT NULL,
    asked_agent_ids TEXT    NOT NULL, -- JSON array
    created_at      INTEGER NOT NULL,
    deadline        INTEGER NOT NULL
) STRICT;

CREATE TABLE reports (
    request_id TEXT    NOT NULL REFERENCES report_requests (id) ON DELETE CASCADE,
    agent_id   TEXT    NOT NULL,
    body       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (request_id, agent_id)
) STRICT;

-- Append only. Nothing in LAC updates or deletes an audit row.
CREATE TABLE audit_log (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    actor  TEXT    NOT NULL,
    action TEXT    NOT NULL,
    target TEXT    NOT NULL DEFAULT '',
    detail TEXT    NOT NULL DEFAULT '',
    at     INTEGER NOT NULL
) STRICT;

CREATE INDEX audit_log_at ON audit_log (at);
CREATE INDEX audit_log_actor ON audit_log (actor, at);
