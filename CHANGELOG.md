# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `lac log`: what the agents have said to each other, in order, with who read it and who
  acknowledged it. Narrow it with `--agent`, `--topic` and `--since` (a duration such as `30m`, or an
  absolute time), or watch it live with `--follow`. Reading everyone's traffic is an operator's
  power, gated on a new `can_read_log` capability; an ordinary agent still gets only its own inbox.
- `message_retention` (24h by default): how long a message that every recipient acknowledged stays
  readable in that log. Acknowledged messages used to be deleted the moment they were handled, which
  would have emptied the log of exactly the conversations that went well.

## [0.1.2] - 2026-09-11

### Fixed

- `lac info`, `lac version --json` and the MCP server's identity reported an installed binary's
  version as `dev`. Only the human-readable `lac version` line had been taught to fall back to the
  build information the toolchain embeds; the other three still read the linker stamp, which a
  `go install` build does not have.

## [0.1.1] - 2026-09-11

### Added

- A pull request watcher for GitHub. `lac pr watch owner/repository#number` keeps an eye on CI
  results, the pull request's state, comments and review conversations, and tells the agents that
  subscribed as soon as something changes. It reads through the `gh` CLI, backs off when GitHub is
  unreachable, and keeps serving the last good reading marked as stale rather than erroring.
- Repositories that belong to a second GitHub login are read as that login. `gh` keeps one active
  account and does not choose one by directory, so a machine whose work repositories belong to one
  login and personal ones to another could only see half of them; a watch now remembers which login
  can see it.
- One daemon per state directory. A second `lacd` against the same database and socket refuses to
  start, naming the process that holds it. The claim is an advisory lock the kernel drops with the
  process, so a crash leaves nothing to clear up.
- `lac reload` and the `daemon.reload` method: ask the daemon to re-read its configuration without
  finding its process id. It reports what it applied and what needs a restart, and needs the same
  capability as defining a resource.
- Configuration reloading. `SIGHUP` re-reads the file at once; otherwise the daemon applies a change
  once the file has settled — the same contents read three times at `restart_delay` (5s by default)
  apart, so about fifteen seconds after the last edit. Resources, capabilities, operators and worker
  commands apply live; settings that cannot change at run time are reported rather than ignored, and
  a configuration that does not parse leaves the running one alone.

### Fixed

- A binary installed with `go install sadeq.uk/lac/cmd/lac@<version>` reported its version as
  `dev`, because only `make build` stamps one in. The details now fall back to the build
  information the toolchain embeds, so an installed binary names the version it came from.
- `lac version` printed `-dirty` twice on a build from a modified tree.
- A command run under `lac run` reported two alarming errors if the daemon stopped mid-run. It now
  says once, plainly, that the slot is reclaimed automatically.

## [0.1.0] - 2026-09-11

The first release: a working coordinator for the AI agents on one machine. They can talk to each
other, queue for the scarce things, hand work to a shared worker, and tell you what they are doing.

### Coordination

- **Shared resources with a fair queue.** A resource has a capacity — four concurrent test runs, one
  shared reviewer — and agents queue for a slot. The check for room and the grant happen in one
  write transaction, so capacity is never exceeded; the queue is served by priority, then by arrival,
  and only the head of the queue may take a free slot. Waiters are woken, never left polling.
- **`lac run --resource test -- make test`**, the one-liner an agent puts in front of anything heavy.
  It waits its turn, runs the command, releases the slot however the command ends, and passes the
  exit status straight through.
- **Durable messaging.** Direct and topic messages, kept until the recipient acknowledges them, so an
  agent that was busy or restarting still gets what it missed. Connected agents are pushed a
  notification rather than polling.
- **Reports.** `lac report` asks every agent what it is doing and collects the answers, naming anyone
  who stayed silent.
- **Dispatch.** `lac ask <job>` hands work to a shared worker (`lac worker --resource test`), which
  runs it in the requester's own directory and streams the output back. A requester names a
  *configured job*, never a command line.
- **Nothing is stranded.** A slot comes back on exit, on failure, on a signal, on disconnect, on
  deregistration, and by the reaper when a holder simply vanishes. A worker that dies mid-task
  returns its work to the queue.

### Interfaces

- **`lacd`**, the daemon: starts from nothing, applies its own migrations, defines the resources in
  your configuration, and drains cleanly on a signal.
- **`lac`**, the command-line client: `run`, `ask`, `agents`, `send`, `inbox`, `queue`, `resources`,
  `define`, `report`, `worker`, `tasks` and the rest, each with a `--json` form for agents.
- **MCP server** (`lac mcp`): twelve tools and two read-only resources, so Claude Code, Codex and
  other MCP clients discover LAC by themselves. Protocol version negotiated, not assumed.
- **A skill** (`lac skill install`): teaches an agent when to queue, whom to tell, and what to read.
  Embedded in the binary, so `go install` is enough.
- **Telegram bridge**, off unless configured: `/agents`, `/resources`, `/queue`, `/report`, `/say`
  and `/broadcast` from a phone, and a way for an agent to reach you when you are away.
- **`pkg/lacclient`**, the public Go client — one implementation of the protocol, shared by the CLI,
  the MCP server and the tests.

### Security

- No network listener. The daemon binds a Unix domain socket only; the Telegram bridge calls outward
  and is never called into.
- The socket directory is `0700` and the socket `0600`, and every connection's peer UID is checked
  against the daemon's through the kernel — `LOCAL_PEERCRED` on darwin, `SO_PEERCRED` on linux, and
  a refusal to serve at all on platforms LAC has not been taught.
- Agents authenticate with a 256-bit token stored only as an HMAC keyed by a per-install secret, so a
  copied database permits no offline guessing attack. Every authentication failure returns the same
  message; the reason goes to the audit log.
- Capabilities come from the operator's policy, never from the agent asking. Broadcasting, defining
  resources and requesting reports are all gated.
- The daemon executes nothing. A worker does, and only a command from the operator's configuration,
  with no shell, in a directory checked against the allowed roots.
- Every state change is appended to an audit log.

### Project

- Go module `sadeq.uk/lac`, no cgo — the SQLite driver is pure Go. (The tag was re-cut shortly after
  it was first pushed, to move the module off `code.sadeq.uk`; nothing else about it changed.)
- Storage is SQLite in WAL mode with embedded, versioned migrations; a newer schema is refused
  rather than written through.
- Documentation: [architecture](docs/architecture.md), [protocol](docs/protocol.md),
  [MCP setup](docs/mcp.md), [Telegram setup](docs/telegram.md) and a commented example configuration.
- CI on Linux and macOS: build, `go vet`, golangci-lint (including gosec), `govulncheck`, and the
  whole suite under the race detector.

[Unreleased]: https://github.com/sadeq-n-yazdi/lac/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/sadeq-n-yazdi/lac/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/sadeq-n-yazdi/lac/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/sadeq-n-yazdi/lac/releases/tag/v0.1.0
