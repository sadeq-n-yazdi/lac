# Security Policy

## Supported versions

LAC is in early development. Only the tip of the `main` branch receives fixes until the first tagged release.

## Reporting a vulnerability

Please **do not open a public issue** for a security problem.

Report it privately through GitHub's
[private vulnerability reporting](https://github.com/sadeq-n-yazdi/lac/security/advisories/new) for this repository.
Include the version or commit, your platform, a reproduction, and the impact you believe it has. You can expect an
acknowledgement within seven days.

## Threat model

LAC is designed for a single trusted user on a single machine. The assets it protects are the agent registry, the
message log, and the resource queue — a hostile local process should not be able to read another agent's messages,
impersonate an agent, or starve or steal resource slots.

**In scope**

- A local process running as a *different* user reading or writing the socket, the database or the config file.
- One registered agent impersonating another, or acting beyond its granted capabilities.
- Token leakage through logs, error messages or the database.
- Queue starvation or capacity bypass.
- Anything reachable from the Telegram bridge: an unauthorised chat driving the daemon, or the bot token leaking.

**Out of scope**

- An attacker who already runs code as your user. They can read the database and the socket by design; LAC is not a
  sandbox.
- The behaviour of the agents themselves. LAC arbitrates access, it does not audit what an agent does with its slot.

## Controls

| Control                | Detail                                                                                            |
|------------------------|---------------------------------------------------------------------------------------------------|
| No network listener    | The daemon binds a Unix domain socket only. There is no TCP or HTTP listener.                      |
| Filesystem permissions | Socket and state directories are `0700`; the socket, database and config file are `0600`.          |
| Peer verification      | Every accepted connection's peer UID must equal the daemon's UID, checked via the kernel.          |
| Agent authentication   | A 256-bit token is issued at registration, stored only as a keyed hash, and compared in constant time. |
| Capabilities           | Deny by default. Each agent is granted the specific resources it may lease and whether it may broadcast. |
| Command execution      | The daemon executes nothing. A worker (`lac worker`) does, and only a command the operator wrote in the configuration: a requester names a key, and there is no field anywhere in the API that carries a command line. No shell is involved, so nothing is interpreted as a pipeline or a substitution. The working directory is checked against the allowed roots before the task is queued. |
| Telegram               | Outbound long polling only — no listener, no inbound connection. Commands are gated by a chat-ID allowlist that cannot be disabled; a message from any other chat gets no reply at all and is audited. The bot token comes from a mode-checked file or the environment, is never logged, and is stripped from error messages that would otherwise carry the URL it sits in. |
| Audit log              | Every state change is appended to an audit table with actor, action, target and timestamp.          |
| Supply chain           | `gosec` and `govulncheck` run in CI; Dependabot keeps modules and actions current.                  |

## Hardening notes for operators

- Keep `$XDG_CONFIG_HOME/lac/config.yaml` at mode `0600`; the daemon refuses to start if the file holding the Telegram
  token is group- or world-readable.
- Prefer supplying the Telegram bot token through the environment on shared machines.
- Revoke an agent with `lac agent deregister`, which invalidates its token immediately.
- Only run `lac worker` for resources whose configured commands you are content for any registered
  agent to trigger in its own working directory. No worker running means no dispatched execution.
