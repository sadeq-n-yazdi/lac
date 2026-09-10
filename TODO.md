# Roadmap

The milestones below are mirrored as GitHub issues. This file is the at-a-glance version; the issues carry the detail
and the discussion.

## Milestone 0 — Project foundation

- [x] MIT license, README, changelog, roadmap
- [x] Contributing guide, code of conduct, security policy
- [x] Issue templates, pull request template, CI workflow, Dependabot
- [x] Go module, Makefile, golangci-lint and pre-commit configuration

## Milestone 1 — Daemon skeleton and storage

- [x] Configuration loading with XDG defaults and validation
- [x] SQLite store: embedded migrations, WAL mode, busy timeout, foreign keys
- [x] Domain types and repository interfaces in `internal/core`
- [x] Unix socket listener: permission hardening and peer-UID verification
- [x] JSON-RPC 2.0 codec, method router, graceful shutdown

## Milestone 2 — Registry, messaging, leases and CLI

- [x] Token issue and verification, per-agent capability checks
- [x] Agent registry with heartbeats and a stale-agent reaper
- [x] Messaging: send, inbox, acknowledge, topics, push notifications
- [x] Leasing: transactional grant, fair queue ordering, TTL reaper, blocking acquire
- [x] `pkg/lacclient` Go client and the `lac` CLI, including `lac run --resource`
- [x] Concurrency test: six clients on a capacity-four resource

## Milestone 3 — MCP provider

- [x] `lac mcp` stdio server exposing the API as MCP tools
- [x] MCP resources for agent list and queue status
- [x] Registration instructions for Claude Code and Codex

## Milestone 4 — Skill

- [x] `skills/lac/SKILL.md` teaching agents when and how to use LAC
- [x] `lac skill install` to put it in the user's skill directory

## Milestone 5 — Reporting and Telegram

- [x] Report request, submit and collect
- [ ] Telegram bridge with chat-ID allowlist and outbound long polling
- [ ] Bot commands: `/agents`, `/queue`, `/report`, `/say`
- [ ] Push notifications for lease grants and direct messages

## Milestone 6 — Dispatch (worker agents)

- [ ] `task.submit`, `task.claim`, `task.complete`
- [ ] Worker mode: claim queued tasks under a lease and execute a configured command
- [ ] Stream results back to the requester
