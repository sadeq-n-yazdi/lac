# Architecture

## Goal

LAC coordinates the AI agents running on one machine. It answers three questions for them: *how do I reach the other
agents*, *whose turn is it to use the scarce thing*, and *what should I tell the human*. Everything else is out of
scope.

## Layers

Dependencies point inward. Nothing in an inner layer imports an outer one.

```
      transports                storage
  ┌──────────────────┐   ┌────────────────────┐
  │ jsonrpc / unixsock│   │ store/sqlite       │
  │ mcp / telegram    │   │ migrations         │
  └─────────┬────────┘   └─────────┬──────────┘
            │                      │
            └──────────┬───────────┘
                       ▼
              ┌────────────────┐
              │ internal/service│   registry, messaging, leasing, reporting, dispatch, prwatch
              └────────┬───────┘
                       ▼
              ┌────────────────┐
              │ internal/core   │   domain types + repository interfaces
              └────────────────┘
```

| Package                     | Responsibility                                                                     |
|-----------------------------|-------------------------------------------------------------------------------------|
| `internal/core`             | Domain types (`Agent`, `Message`, `Resource`, `Lease`, `QueueEntry`, `Report`, `Task`, `Watch`) and the repository interfaces the services depend on. No I/O, no SQL, no JSON. |
| `internal/service`          | The business rules. The only place that decides who may hold a slot, in what order, and for how long. |
| `internal/store/sqlite`     | Repository implementations, embedded migrations, transaction handling.              |
| `internal/transport/jsonrpc`| JSON-RPC 2.0 framing and method dispatch over any `net.Listener`.                   |
| `internal/transport/unixsock`| Socket creation, permission hardening, peer-credential verification.               |
| `internal/transport/mcp`    | Maps MCP tool calls onto service calls so AI tools discover LAC natively.           |
| `internal/transport/telegram`| Outbound long-poll bridge for the human operator.                                  |
| `internal/api`              | The JSON-RPC method surface: parameter types, the wire views, and the capability check in front of each method. |
| `internal/github`           | The `gh` CLI wrapper the watcher reads through, including per-account tokens.        |
| `internal/auth`             | Token issue and verification, capability checks.                                    |
| `internal/config`           | Configuration loading, XDG path resolution, validation.                             |
| `pkg/lacclient`             | The public Go client. The CLI, the MCP adapter and the integration tests all use it — request logic is never duplicated. |

Adding a new front end means adding a transport. It must not add a business rule.

## The lease, in detail

A lease is the core primitive, and the reason LAC exists.

1. An agent calls `lease.acquire` for a named resource, optionally with a priority and a timeout.
2. The service inserts a **queue entry** and then tries to grant it.
3. A grant is a single `BEGIN IMMEDIATE` transaction: count the active leases on the resource, compare against the
   resource's capacity, and if there is room, insert a lease and mark the entry granted. SQLite's write lock makes this
   race-free without an in-process mutex, and it stays correct if a future version runs more than one daemon process.
4. If there is no room, the call blocks — the client is parked on a channel, not polling — until a slot frees, the
   caller's context expires, or the request is cancelled.
5. The holder runs its own work in its own working directory and calls `lease.release`.
6. A reaper expires leases past their TTL and grants the freed slot to the head of the queue, so an agent that crashes
   mid-run cannot hold a slot forever. Long-running holders call `lease.renew`.

Queue order is `(priority DESC, requested_at ASC)`, which is fair by default and lets an interactive request jump ahead
of a batch one when the operator asks for it.

## Message durability

Messages use an outbox: a row in `messages` plus one row per recipient in `deliveries`. A message stays undelivered
until the recipient acknowledges it, so an agent that restarts still receives what it missed. Subscribers get a push
over the same connection; the inbox is the fallback for agents that are not connected at the time.

An acknowledged message is not deleted at once. It is kept for `message_retention` (a day by default) so that
`message.log` can still show it: pruning on acknowledgement would empty the operator's log of exactly the conversations
that were handled properly. Unacknowledged messages are kept regardless, until they expire.

The log is deliberately a different question from the inbox. An inbox read is always the caller's own — an agent id in
the request would be a claim, and honouring it would let one agent read another's post. `message.log` ignores who is
asking and returns everybody's traffic, which is why it is gated on `can_read_log`, an operator capability, rather than
being a variation on the inbox.

## Watching pull requests

The watcher polls GitHub through the `gh` CLI, compares each reading with the last one, and turns the difference into
events an agent can subscribe to: CI results, draft and merge state, comments, review threads, pushes.

Two properties make it a watcher rather than a proxy. It keeps the **last good observation** in full and serves that,
marked stale, when GitHub cannot be reached — an agent asking during an outage gets the last thing that was true rather
than an error. And it backs off on failure, reporting an outage once rather than on every attempt.

`gh` keeps one *active* account and does not choose one by directory, so on a machine where one login owns the work
repositories and another the personal ones, the active account cannot see half of them. A watch therefore remembers the
login that can see it, found by trying the active account first and then the others, and each request is made with that
account's token rather than by changing which account is active for the operator's own shell.

## Why JSON-RPC over a Unix socket

- MCP already speaks JSON-RPC 2.0, so the MCP transport is a thin adapter over the same service layer rather than a
  parallel implementation.
- A Unix socket gives us filesystem permissions and kernel-verified peer credentials for free, and it cannot be reached
  from another host.
- It is debuggable with ordinary tools — you can drive the daemon by hand from a shell.

## Why SQLite

State must survive a daemon restart: a queue that forgets its order on restart is worse than no queue. SQLite in WAL
mode gives durability, transactions for the grant path, and a file the operator can inspect. The driver is pure Go, so
there is no cgo and no toolchain requirement for `go install`.

## Extension points

- **A new resource kind** is configuration, not code: define a resource with a capacity.
- **A new front end** is a new package under `internal/transport` that calls the services.
- **A new storage backend** is a new implementation of the `internal/core` repository interfaces.
- **Dispatch** (worker agents that execute work on another agent's behalf) is layered on top of leases; the coordinator
  never gains the ability to execute arbitrary commands as a side effect.
