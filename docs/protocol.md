# The LAC protocol

The daemon speaks **JSON-RPC 2.0** over a Unix domain socket, one message per line. This document
is enough to write a client in another language; if you are writing Go, use
[`pkg/lacclient`](../pkg/lacclient) instead and skip all of this.

- Socket: `$XDG_RUNTIME_DIR/lac/lacd.sock`, falling back to `~/.local/state/lac/run/lacd.sock`
- Framing: newline-delimited JSON. One request per line, one reply per line.
- Batching: **not supported.** Send one request per message.
- Concurrency: requests on one connection are handled concurrently, and replies may arrive in any
  order. Match them by `id`.

## Authenticating

Two methods work without a token: `agent.register` and `agent.authenticate`. Everything else is
refused with code `-32000` until the connection has been authenticated.

The caller's identity comes from the connection, never from a request body. There is no `agent_id`
parameter anywhere in this API, because honouring one would let any agent act as any other.

```jsonc
// Register: the only time a token is ever sent to you.
--> {"jsonrpc":"2.0","id":1,"method":"agent.register",
     "params":{"name":"claude-a","kind":"claude","workdir":"/home/you/code/project","pid":4242}}
<-- {"jsonrpc":"2.0","id":1,"result":{
      "agent":{"id":"agent_06g8…","name":"claude-a","kind":"claude","state":"active",
               "workdir":"/home/you/code/project","pid":4242,
               "capabilities":{"resources":["*"],"can_broadcast":true,
                               "can_define_resources":false,"can_request_reports":false},
               "registered_at":"2026-03-01T12:00:00Z","last_heartbeat_at":"2026-03-01T12:00:00Z"},
      "token":"lac_R2h0…"}}
```

Registering also authenticates that connection, so a fresh client does not have to turn around and
present the token it was just handed. On a later connection:

```jsonc
--> {"jsonrpc":"2.0","id":1,"method":"agent.authenticate","params":{"token":"lac_R2h0…"}}
<-- {"jsonrpc":"2.0","id":1,"result":{"agent":{…}}}
```

An agent may register under a name it already holds **from the same process id**, which reclaims
its identity and issues a fresh token. A different process asking for a live name is refused with
`-32002`.

Capabilities are decided by the daemon's configuration from the agent's name. There is no way to
ask for them.

## Staying on the roster

Call `agent.heartbeat` regularly. The reply says how long you may stay silent; beat about three
times per that interval.

```jsonc
--> {"jsonrpc":"2.0","id":2,"method":"agent.heartbeat"}
<-- {"jsonrpc":"2.0","id":2,"result":{"next_by_seconds":90}}
```

An agent that stops beating is marked stale, and everything it holds is released.

## Errors

```jsonc
<-- {"jsonrpc":"2.0","id":3,"error":{"code":-32004,"message":"resource capacity reached: \"test\" has no free slot"}}
```

| Code     | Meaning                                                                       |
|----------|-------------------------------------------------------------------------------|
| `-32700` | The line was not valid JSON. The connection is then closed.                    |
| `-32600` | Not a valid JSON-RPC 2.0 request.                                              |
| `-32601` | No such method.                                                                |
| `-32602` | Bad parameters — including a misspelled field, which is rejected rather than ignored. |
| `-32603` | Something went wrong inside the daemon.                                        |
| `-32000` | Unauthorised: no token, a bad one, or a capability you do not have.            |
| `-32001` | Not found.                                                                     |
| `-32002` | Already exists.                                                                |
| `-32003` | Conflict: the state changed underneath you (a lease you no longer hold).       |
| `-32004` | Capacity reached. An ordinary answer to a non-blocking acquire, not a fault.   |
| `-32005` | The daemon is shutting down. The work was not started; retry when it is back.  |

Every authentication failure returns the same message whatever went wrong, so a caller learns
whether its token works and nothing else. The reason is in the daemon's audit log.

## Notifications

The daemon pushes notifications — JSON-RPC messages with no `id` — to a connection that owns the
agent concerned. Do not reply to them.

```jsonc
<-- {"jsonrpc":"2.0","method":"message.received",
     "params":{"message_id":"msg_06g8…","from":"agent_06g8…","kind":"question",
               "topic":"","created_at":"2026-03-01T12:00:01.123456Z"}}
```

`message.received` says only that something arrived; fetch it with `message.inbox`.

## Methods

### Agents

| Method              | Parameters                                | Returns                             |
|---------------------|-------------------------------------------|-------------------------------------|
| `agent.register`    | `name`, `kind`, `workdir`, `pid`          | `{agent, token}`                    |
| `agent.authenticate`| `token`                                   | `{agent}`                           |
| `agent.heartbeat`   | —                                         | `{next_by_seconds}`                 |
| `agent.list`        | `states[]`, `kinds[]` (both optional)     | `{agents[]}`                        |
| `agent.deregister`  | —                                         | `{released_leases}`                 |

`agent.list` with no states returns the agents that are present. Pass `["active","stale","deregistered"]`
to see the history.

### Messages

| Method               | Parameters                                  | Returns                              |
|----------------------|---------------------------------------------|--------------------------------------|
| `message.send`       | exactly one of `to` / `topic`, plus `kind`, `body` | `{message_id, recipients, notified}` |
| `message.inbox`      | `limit` (optional)                          | `{messages[]}`                       |
| `message.ack`        | `message_ids[]`                             | `{acknowledged}`                     |
| `message.log`        | `agent`, `topic`, `since`, `limit` (all optional) | `{messages[]}`                 |
| `message.subscribe`  | `topic`                                     | `{topic}`                            |
| `message.unsubscribe`| `topic`                                     | `{topic}`                            |

`to` is an agent **name**; `body` is any JSON object. The topic `all` reaches every live agent and
needs the broadcast capability.

`message.log` is the operator's view rather than an agent's: it returns traffic between *other*
agents, with how many recipients have read and acknowledged each message, and it needs the
`can_read_log` capability. An ordinary agent reads its own inbox and nothing else. A message that
every recipient acknowledged stays readable here for `message_retention` (a day by default) before
it is pruned.

Reading is not acknowledging: a message comes back from `message.inbox` until `message.ack` clears
it, which is what makes delivery survive an agent that crashes mid-task. Acknowledging a message
addressed to somebody else changes nothing, and `acknowledged` tells you how many really cleared.

### Resources and leases

| Method            | Parameters                                                        | Returns             |
|-------------------|-------------------------------------------------------------------|---------------------|
| `resource.define` | `name`, `capacity`, `lease_time_to_live`, `description`           | `{resource}`        |
| `resource.list`   | —                                                                 | `{resources[]}`     |
| `resource.status` | `resource`                                                        | `{resource}`        |
| `lease.acquire`   | `resource`, `priority`, `reason`, `metadata`, `no_wait`, `timeout`| `{lease}`           |
| `lease.renew`     | `lease_id`                                                        | `{lease}`           |
| `lease.release`   | `lease_id`                                                        | `{released}`        |
| `lease.held`      | —                                                                 | `{leases[]}`        |
| `queue.status`    | `resource`                                                        | `{resource, waiting[]}` |
| `queue.cancel`    | `entry_id`                                                        | `{cancelled}`       |

**`lease.acquire` blocks.** That is the point: it returns when the slot is yours.

```jsonc
--> {"jsonrpc":"2.0","id":4,"method":"lease.acquire",
     "params":{"resource":"test","reason":"make test","timeout":"5m"}}
<-- {"jsonrpc":"2.0","id":4,"result":{"lease":{
      "id":"lease_06g8…","resource":"test","agent_id":"agent_06g8…",
      "acquired_at":"2026-03-01T12:00:02Z","expires_at":"2026-03-01T12:15:02Z",
      "renew_after_seconds":450}}}
```

- Order is priority first (higher wins), then the order requests arrived.
- `no_wait: true` returns `-32004` immediately instead of queueing.
- `timeout` gives up after a duration such as `"5m"`; the request leaves the queue, so nobody waits
  behind you.
- Closing the connection while waiting also leaves the queue.
- Renew before `renew_after_seconds` elapses, or the slot is reclaimed and given to whoever is next.
  `lease.renew` on a slot you have lost returns `-32003`, which means stop working.
- Release is idempotent, so a retry after a dropped connection is safe.

### Dispatched work

| Method           | Parameters                                     | Returns              |
|------------------|------------------------------------------------|----------------------|
| `task.commands`  | —                                              | `{commands[]}`       |
| `task.submit`    | `command`, `workdir`, `wait`, `timeout`        | `{task}`             |
| `task.wait`      | `resource`                                     | `{available}`        |
| `task.claim`     | `resource`, `lease_id`, `no_wait`              | `{task, run[], time_limit}` |
| `task.output`    | `task_id`, `chunk`                             | `{recorded}`         |
| `task.complete`  | `task_id`, `exit_code`, `failure`              | `{task}`             |
| `task.status`    | `task_id`, `wait`                              | `{task}`             |
| `task.cancel`    | `task_id`                                      | `{task}`             |
| `task.list`      | `limit`                                        | `{tasks[]}`          |

`command` is a **key**, never a command line: what the key runs comes from the daemon's
configuration. There is no parameter anywhere in this API that lets a requester say what to
execute, which is what makes dispatch safe to have at all. An empty `workdir` means the
requester's own, and any other is checked against the allowed roots.

A worker's loop is `task.wait`, then `lease.acquire`, then `task.claim` with `no_wait`. That order
matters: `task.wait` grants nothing and needs no slot, so an idle worker does not occupy capacity it
is not using — two idle workers on a two-slot resource would otherwise leave nobody else able to run
anything. `task.claim` then requires a `lease_id` the caller really holds on that resource, which is
what keeps dispatched work inside the same capacity limit as everything else; with `no_wait`, a
worker that lost the race to another is told at once and can give its slot back. The reply carries
`run`, the command from the configuration, which is the only place the worker gets it from.

A worker reports progress with `task.output`; the daemon pushes each chunk to the requester as a
`task.output` notification, so a waiting requester sees the run rather than silence.

### Watched pull requests

| Method        | Parameters                                        | Returns          |
|---------------|---------------------------------------------------|------------------|
| `pr.watch`    | `pull_request`                                    | `{watch}`        |
| `pr.status`   | `pull_request` or `watch_id`, `refresh`, `wait`   | watch detail     |
| `pr.list`     | `all`                                             | `{watches[]}`    |
| `pr.unwatch`  | `pull_request` or `watch_id`                      | `{watching}`     |

`pull_request` is written the way a person writes it — `owner/repository#number`, or a pasted
`https://github.com/owner/repository/pull/123`.

The daemon reads GitHub through the `gh` command line, so LAC holds no credential of its own and
sees exactly what your login sees. It polls; there is no inbound webhook, because there is no
listener.

Whoever calls `pr.watch` is subscribed, and each change arrives as an ordinary message of kind
`pr-update` — so it waits in the inbox of an agent that was busy, and reaches a connected one at
once. Changes are CI results, the state (opened, closed, merged, draft), pushes, comments, reviews,
and review conversations appearing, being replied to, or being resolved.

A watch view says how old it is:

```jsonc
{"watch":{"id":"watch_06g8…","reference":"sadeq-n-yazdi/lac#31","title":"feat: lac reload",
  "state":"open","draft":false,"checks":"pending","unresolved_threads":2,
  "observed_at":"2026-09-11T08:46:12Z","stale":false}}
```

`stale` is true when the last attempt failed or the last success is old. During a GitHub outage the
last good answer is still returned, marked stale and carrying `last_error` — an agent acting on old
information should know it is doing so. `refresh` reads GitHub now; `wait` blocks until the next
reading, which is what to use straight after pushing.

### Reports

| Method           | Parameters                            | Returns            |
|------------------|---------------------------------------|--------------------|
| `report.request` | `question`, `deadline`, `wait`        | collection         |
| `report.submit`  | `request_id`, `body`                  | `{recorded}`       |
| `report.collect` | `request_id`, `wait`                  | collection         |

`report.request` needs the report capability. The question reaches each agent as an ordinary
message of kind `report-request` whose body carries `request_id`, `question` and `deadline` — so an
agent answers by reading its inbox, not by listening for anything special.

A collection looks like this:

```jsonc
{"request_id":"report_06g8…","question":"what are you working on?","asked":2,
 "reports":[{"agent_id":"agent_06g8…","agent_name":"claude-a",
             "body":"rewriting the parser tests","created_at":"2026-03-01T12:00:05Z"}],
 "silent":["claude-b"],"complete":false}
```

`silent` names the agents that were asked and did not answer, by name. They are listed rather than
omitted: an agent that has gone quiet is exactly what the operator wants to see.

### The daemon itself

| Method          | Parameters | Returns                                                        |
|-----------------|------------|----------------------------------------------------------------|
| `daemon.info`   | —          | version, protocol, uptime, and the methods this build serves    |
| `daemon.ping`   | —          | `{pong}`                                                        |
| `daemon.reload` | —          | `{source, applied[], deferred[]}`                               |

`daemon.info` and `daemon.ping` work without a token.

`daemon.reload` re-reads the configuration file at once, rather than waiting for the daemon to
notice it has settled. It needs the same capability as defining a resource — a reload can redefine
them — and returns what it changed along with anything that changed in the file but needs a
restart, such as the socket path:

```jsonc
--> {"jsonrpc":"2.0","id":9,"method":"daemon.reload"}
<-- {"jsonrpc":"2.0","id":9,"result":{"source":"/home/you/.config/lac/config.yaml",
      "applied":["resources (2)","commands"],"deferred":["socket_path"]}}
```

Call `daemon.info` first to check you are talking to a version you understand, and to discover what
it can do.

## Driving it by hand

Nothing more than a socket and a shell is required:

```sh
python3 - <<'EOF'
import json, socket
s = socket.socket(socket.AF_UNIX)
s.connect("/home/you/.local/state/lac/run/lacd.sock")
s.sendall(json.dumps({"jsonrpc":"2.0","id":1,"method":"daemon.info"}).encode() + b"\n")
print(s.makefile().readline())
EOF
```
