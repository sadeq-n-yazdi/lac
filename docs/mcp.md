# Using LAC from an AI tool

LAC speaks the Model Context Protocol, so Claude Code, Codex and anything else that reads MCP can
use it without being taught a command line. Register it once and every session on this machine can
see the others, queue for shared resources, and talk.

## 1. Start the daemon

```sh
lacd &
```

Everything else connects to it. Nothing works while it is not running — the tools say so plainly
rather than failing silently.

## 2. Register the MCP server

The server is the `lac` binary with one argument. It runs as a subprocess of your AI tool and talks
to the daemon over the socket.

**Claude Code**

```sh
claude mcp add lac -- lac mcp
```

Or, to make it available in every project, add it to `~/.claude.json` under `mcpServers`:

```json
{
  "mcpServers": {
    "lac": {
      "command": "lac",
      "args": ["mcp"]
    }
  }
}
```

**Codex** — add it to your Codex configuration's MCP server list:

```toml
[mcp_servers.lac]
command = "lac"
args = ["mcp"]
```

**Anything else** — the contract is the standard one: run `lac mcp`, speak JSON-RPC 2.0 over its
stdin and stdout, one message per line.

If `lac` is not on your `PATH`, use its full path. `go install code.sadeq.uk/lac/cmd/lac@latest`
puts it in `$(go env GOPATH)/bin`.

## 3. Install the skill

The MCP tools tell a model *what it can do*. The skill tells it *when to bother*:

```sh
lac skill install
```

That writes `~/.claude/skills/lac/SKILL.md`. Use `--dir` for another location, `lac skill print` to
read it, and `lac skill path` to see where it would go.

## What the model gets

| Tool               | For                                                             |
|--------------------|-----------------------------------------------------------------|
| `lac_agents`       | Who else is working, and in which directory                     |
| `lac_resources`    | The scarce things on this machine and how busy they are         |
| `lac_acquire_slot` | Wait for a slot before running a test suite or a build          |
| `lac_release_slot` | Give the slot back                                              |
| `lac_my_slots`     | What this session is holding                                    |
| `lac_queue`        | Who is waiting for a resource, in order                         |
| `lac_send_message` | Tell another agent — or everyone — something                    |
| `lac_inbox`        | Read what was sent to this session                              |
| `lac_report`       | Answer the operator's "what is everyone working on?"            |
| `lac_worker_commands` | What a shared worker on this machine will run for you        |
| `lac_ask_worker`   | Hand a job to a shared worker instead of running it yourself    |
| `lac_task`         | Follow a job you handed off                                     |

Two read-only resources are exposed as well: `lac://agents` and `lac://resources`, for a client that
would rather fetch state than spend a tool call.

## How a session appears to the others

The MCP server registers itself with the daemon on its first tool call. By default it is named
after the tool and the directory it is working in — `claude-myproject-48213` — so two Claude
sessions in the same project are still distinct. Override it if you like:

```json
{
  "mcpServers": {
    "lac": {
      "command": "lac",
      "args": ["mcp", "--name", "claude-backend", "--kind", "claude"]
    }
  }
}
```

The session sends a heartbeat while it runs, and deregisters when the tool closes it — which
releases any slot the model forgot to give back.

## Waiting

`lac_acquire_slot` blocks until the slot is granted. If it waits longer than five minutes it
returns and tells the model to call again; the request leaves the queue, so nobody is left waiting
behind a session that has gone. Change the limit with `--acquire-timeout`.

## Troubleshooting

- **"the lac daemon is not reachable"** — `lacd` is not running, or it is listening somewhere else.
  Check with `lac info`, and pass `--socket` to both if you use a non-default path.
- **Tools do not appear** — your tool caches the server list; restart the session. `lac mcp
  --verbose` logs to stderr, which your tool usually shows in its MCP logs.
- **Nothing is coordinated** — make sure every session registers the same MCP server, and that they
  all talk to one daemon. `lac agents` from a shell shows who the daemon can see.
