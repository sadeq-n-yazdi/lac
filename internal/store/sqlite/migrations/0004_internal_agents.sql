-- The daemon's own components — the Telegram bridge, the pull request watcher — hold a place on
-- the roster so agents can address them by name. They are not sessions, though: nobody is sitting
-- behind one, so asking them what they are working on only ever ends in a timeout.
--
-- Marking them here keeps that distinction in the one place that can answer it, rather than having
-- every caller guess from a name or a kind.

ALTER TABLE agents ADD COLUMN internal INTEGER NOT NULL DEFAULT 0;
