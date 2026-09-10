-- Dispatch: work one agent asks a shared worker to do on its behalf.
--
-- A task names a command *key*, never a command line. What that key runs is decided by the
-- daemon's configuration, so a requester can ask for "the tests" but can never say what "the tests"
-- means. The working directory is the requester's own, checked against the allowed roots when the
-- task is submitted.

CREATE TABLE tasks (
    id            TEXT    PRIMARY KEY,
    resource_name TEXT    NOT NULL REFERENCES resources (name) ON DELETE CASCADE,
    command_key   TEXT    NOT NULL,
    workdir       TEXT    NOT NULL,
    requester_id  TEXT    NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    worker_id     TEXT    REFERENCES agents (id) ON DELETE SET NULL,
    lease_id      TEXT    REFERENCES leases (id) ON DELETE SET NULL,
    state         TEXT    NOT NULL CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    exit_code     INTEGER,
    output        TEXT    NOT NULL DEFAULT '',
    failure       TEXT    NOT NULL DEFAULT '',
    submitted_at  INTEGER NOT NULL,
    started_at    INTEGER,
    finished_at   INTEGER
) STRICT;

-- Claiming reads the oldest queued task for a resource, which this index makes a single seek.
CREATE INDEX tasks_queued ON tasks (resource_name, submitted_at) WHERE state = 'queued';
CREATE INDEX tasks_requester ON tasks (requester_id, submitted_at);
CREATE INDEX tasks_worker ON tasks (worker_id) WHERE state = 'running';
