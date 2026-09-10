# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- A Telegram bridge, off unless configured: `/agents`, `/resources`, `/queue`, `/report`, `/say` and
  `/broadcast` from a phone, and a way for an agent to reach the operator when they are away. Gated
  by a chat-ID allowlist that cannot be disabled; outbound long polling only, no listener.
- Reports: `lac report` asks every agent what it is doing and collects the answers, naming anyone
  who stayed silent. Agents answer with `lac_report` over MCP or `lac answer` from a shell.
- `docs/protocol.md`: the method-by-method JSON-RPC reference, enough to write a client in another
  language.
- `lac mcp`: a Model Context Protocol server, so Claude Code, Codex and other MCP clients discover
  LAC by themselves. Eight tools and two read-only resources, over the same client the CLI uses.
- `lac skill install`: the bundled skill teaching an agent when to queue, whom to tell, and what to
  read, installed to `~/.claude/skills/lac/SKILL.md`.
- `internal/auth`: agent tokens stored as keyed hashes, and capability checks that deny by default.
- `internal/service/registry`, `internal/service/messaging`, `internal/service/leasing`: the agent roster,
  durable messaging, and the fair, capacity-bounded resource queue.
- `internal/api`: the JSON-RPC method surface, with every call but registration behind a token.
- `pkg/lacclient`: the public Go client for the daemon.
- The `lac` command-line client, including `lac run --resource <name> -- <command>`.
- `internal/core`: the domain vocabulary and the storage contracts, with no I/O.
- `internal/config`: XDG path resolution and layered configuration with strict validation.
- `internal/store/sqlite`: a pure-Go SQLite store with embedded, versioned migrations.
- `internal/transport/unixsock`: a private Unix socket listener with kernel peer verification.
- `internal/transport/jsonrpc`: the JSON-RPC 2.0 server, with concurrent calls and graceful shutdown.
- `internal/daemon` and a working `lacd` serving `daemon.info` and `daemon.ping`.
- Project foundation: README, MIT license, changelog, TODO roadmap, contributing guide, code of conduct and security
  policy.
- GitHub issue and pull request templates, CI workflow and Dependabot configuration.
- Go module `code.sadeq.uk/lac`, Makefile, linter and pre-commit configuration.

[Unreleased]: https://github.com/sadeq-n-yazdi/lac/commits/main
