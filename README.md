# LAC — Local Agents Coordinator

[![CI](https://github.com/sadeq-n-yazdi/lac/actions/workflows/ci.yml/badge.svg)](https://github.com/sadeq-n-yazdi/lac/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/sadeq-n-yazdi/lac?sort=semver)](https://github.com/sadeq-n-yazdi/lac/releases)

LAC is a small background daemon that lets the AI coding agents running on **your own machine** coordinate with each
other — talk, queue for scarce local resources, and report back to you — over a Unix socket. Nothing leaves the machine
except the optional Telegram bridge.

## Why

Running several AI agents at once on one laptop breaks down in predictable ways:

- They cannot talk to each other, so they duplicate work and overwrite each other's assumptions.
- They all want the same scarce resource at the same time. A machine that tolerates four concurrent test threads will
  thrash when four agents each start their own test run.
- You have no single place to ask "what is everybody doing right now?".

LAC solves those three problems and nothing else.

## What it gives you

| Capability     | What it means                                                                                       |
|----------------|-----------------------------------------------------------------------------------------------------|
| **Messaging**  | Direct and topic-based messages between registered agents, durable until acknowledged.               |
| **Resources**  | Named resources with a fixed capacity (`test` = 4 slots, `reviewer` = 1) and a fair, ordered queue.  |
| **Reporting**  | Broadcast a report request and collect every agent's answer in one place.                            |
| **Dispatch**   | Hand a job to a shared worker, which runs the operator's configured command in your directory.       |
| **Interfaces** | A CLI, an MCP server so AI tools discover it automatically, and an optional Telegram bot for you.    |

The core primitive is a **lease**: an agent asks for a slot on a resource, waits its turn in the queue, gets the slot,
runs its own work in its own working directory, and releases it. LAC arbitrates; it does not run your commands for you.

```
lac run --resource test -- make test
```

Six agents can issue that line at once; at most four will be running at any moment, and they are served in request
order.

## Architecture

```
                 ┌──────────────┐        ┌─────────────┐
   AI agent ────►│              │        │             │
   AI agent ────►│  MCP server  │───┐    │  Telegram   │◄──── you (remote)
   AI agent ────►│              │   │    │   bridge    │
                 └──────────────┘   │    └──────┬──────┘
                                    ▼           ▼
   you (local) ── lac CLI ──►  ┌──────────────────────┐
                              │  lacd  (JSON-RPC 2.0  │
                              │   over a Unix socket) │
                              ├──────────────────────┤
                              │ registry · messaging  │
                              │ leasing  · reporting  │
                              ├──────────────────────┤
                              │       SQLite          │
                              └──────────────────────┘
```

Dependencies point inward: `internal/core` (domain) ← `internal/service` (use cases) ← transports and storage. Adding a
new front end means adding a transport, not touching business rules.

- [docs/architecture.md](docs/architecture.md) — how the parts fit together, and why
- [docs/protocol.md](docs/protocol.md) — the JSON-RPC surface, for writing a client
- [docs/mcp.md](docs/mcp.md) — using LAC from Claude Code, Codex or another MCP client
- [docs/telegram.md](docs/telegram.md) — asking what the agents are doing from your phone

## Status

**v0.1.0** — working, and used on the machine it was written on. Every planned milestone is done:
the daemon, the CLI, the MCP server and skill, reports, the Telegram bridge and shared workers. It
is a first release, so expect the odd rough edge; see [CHANGELOG.md](CHANGELOG.md) for what it does
and the [issue tracker](https://github.com/sadeq-n-yazdi/lac/issues) for anything outstanding.

## Install

Requires Go 1.26 or newer. There is no cgo dependency — the SQLite driver is pure Go.

```sh
git clone https://github.com/sadeq-n-yazdi/lac.git
cd lac
make build      # lacd and lac in ./bin
```

`go install sadeq.uk/lac/cmd/lac@latest` will work once the page at `sadeq.uk/lac` names this module
rather than the old `code.sadeq.uk/lac` — one word, see [docs/vanity-import.md](docs/vanity-import.md).
Until then, build from a checkout.

## Quick start

```sh
lacd &                                   # start the daemon
lac resources                            # what this machine shares, from your config
lac run --resource test -- make test     # queue for a slot, run, release
lac agents                               # who is working right now
lac queue test                           # who is waiting, and for what
lac send claude-b question 'are you touching the parser?'
lac inbox --ack                          # read what was sent to you
lac report "what are you working on?"    # ask every agent, and wait for the answers
lac ask test                             # hand the job to the machine's shared worker
```

There is no setup step: the first command registers this shell as an agent, named after the directory
it is in. Give it a stable identity with `lac --name claude-a register --save` if you would rather.

Resources come from your configuration file — copy [docs/config.example.yaml](docs/config.example.yaml)
to `~/.config/lac/config.yaml` and edit it. An operator can also define one on the fly:

```sh
lac --name operator define --capacity 4 --description "concurrent test runs" test
```

## Using it from an AI tool

LAC speaks MCP, so Claude Code and Codex can use it directly:

```sh
claude mcp add lac -- lac mcp   # register the server
lac skill install               # teach the model when to use it
```

See [docs/mcp.md](docs/mcp.md) for Codex, for other clients, and for what the model gets.

## Running it

One daemon runs per state directory. A second `lacd` against the same database and socket refuses
to start and says who holds it — two of them would each serve half the agents and disagree about
who holds what. Run a throwaway instance with `--database` and `--socket` pointing elsewhere.

The configuration is re-read without a restart:

```sh
lac --name operator reload     # reload now, and say what changed
kill -HUP $(pgrep lacd)        # the same, without a client
```

`lac reload` prints what it applied and what needs a restart. Or simply edit the file and wait. The daemon applies a change once the file has stopped changing —
the same contents read three times at `restart_delay` apart, so about 15 seconds after your last
save. That way an editor writing in several steps never has half a configuration read out from
under it. Resources, capabilities, operators and worker commands all apply live; the socket, the
database and the Telegram bridge need a restart, and the daemon says so rather than pretending.

Stopping is graceful: `SIGINT` or `SIGTERM` stops accepting, lets calls that are in flight finish,
removes the socket and releases the lock, so the next daemon starts immediately. A command already
running under `lac run` is left to finish — its slot is reclaimed automatically.

## Files and paths

LAC follows the XDG base directory spec:

| Purpose | Path                                                                     |
|---------|--------------------------------------------------------------------------|
| Config  | `$XDG_CONFIG_HOME/lac/config.yaml` (default `~/.config/lac/config.yaml`) |
| Data    | `$XDG_STATE_HOME/lac/lac.db` (default `~/.local/state/lac/lac.db`)      |
| Socket  | `$XDG_RUNTIME_DIR/lac/lacd.sock`, falling back to the state directory   |
| Lock    | `lacd.lock` beside the database — one daemon per state directory        |

## Security

LAC listens on a Unix domain socket only — there is no TCP listener anywhere, including in the
optional Telegram bridge, which calls outward and is never called into. The socket directory is `0700`, the socket is
`0600`, and every connection's peer UID must match the daemon's. Agents authenticate with a token issued at
registration and stored only as a keyed hash. See [SECURITY.md](SECURITY.md) for the full model and how to report a
vulnerability.

## Contributing

Issues and pull requests are welcome — please read [CONTRIBUTING.md](CONTRIBUTING.md) first.

## License

MIT — see [LICENSE](LICENSE).
