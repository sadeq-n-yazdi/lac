# Running the daemon

`lacd &` is enough to try LAC. For a machine you actually work on, run it as a login service so it is
there before you are, and comes back if it dies.

## Install

```sh
go install sadeq.uk/lac/cmd/lac@latest
go install sadeq.uk/lac/cmd/lacd@latest
```

Both land in `$(go env GOPATH)/bin`. `lacd` is the daemon; `lac` is the client, the MCP server
(`lac mcp`) and the skill installer (`lac skill install`). Nothing in `lac` starts `lacd`.

## Where things live

| Purpose      | Path                                                                     |
|--------------|--------------------------------------------------------------------------|
| Configuration| `$XDG_CONFIG_HOME/lac/config.yaml` → `~/.config/lac/config.yaml`, mode 0600 |
| Database     | `$XDG_STATE_HOME/lac/lac.db` → `~/.local/state/lac/lac.db`                |
| Secret key   | `~/.local/state/lac/secret.key`                                          |
| Socket       | `$XDG_RUNTIME_DIR/lac/lacd.sock`, else `~/.local/state/lac/run/lacd.sock` |
| Lock         | `lacd.lock`, beside the database                                         |

Directories are created 0700 and files 0600. The socket path is capped at 103 bytes by the operating
system, so a deeply nested `XDG_RUNTIME_DIR` is refused with an error saying so rather than failing
obscurely at connect time.

## macOS: a launchd agent

`~/Library/LaunchAgents/uk.sadeq.lac.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>            <string>uk.sadeq.lac</string>
    <key>ProgramArguments</key> <array><string>/Users/you/go/bin/lacd</string></array>
    <key>RunAtLoad</key>        <true/>
    <key>KeepAlive</key>        <dict><key>SuccessfulExit</key><false/></dict>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key> <string>/Users/you</string>
        <key>PATH</key> <string>/Users/you/go/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    </dict>
    <key>StandardOutPath</key>  <string>/Users/you/.local/state/lac/lacd.log</string>
    <key>StandardErrorPath</key><string>/Users/you/.local/state/lac/lacd.log</string>
    <key>ThrottleInterval</key> <integer>10</integer>
    <key>ProcessType</key>      <string>Background</string>
</dict>
</plist>
```

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/uk.sadeq.lac.plist   # load and start
launchctl kickstart -k gui/$(id -u)/uk.sadeq.lac                             # restart
launchctl print gui/$(id -u)/uk.sadeq.lac                                    # state, pid, last exit
launchctl bootout gui/$(id -u)/uk.sadeq.lac                                  # stop and unload
```

`KeepAlive: {SuccessfulExit: false}` restarts a crash but respects a deliberate stop, because a
graceful shutdown exits zero.

**Set `PATH` in the plist.** A launchd agent inherits almost no environment. The pull request watcher
shells out to `gh`, and worker commands run `make` or `docker`; without a usable `PATH` the watcher
degrades to reporting every pull request as unreachable, which looks like a GitHub outage rather than
a configuration mistake.

## Linux: a systemd user unit

`~/.config/systemd/user/lac.service`:

```ini
[Unit]
Description=Local Agents Coordinator
After=default.target

[Service]
ExecStart=%h/go/bin/lacd
Restart=on-failure
RestartSec=10
Environment=PATH=%h/go/bin:/usr/local/bin:/usr/bin:/bin

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now lac.service
journalctl --user -u lac.service -f
loginctl enable-linger "$USER"    # keep it running when you are not logged in
```

## One daemon at a time

`lacd` holds an exclusive `flock` on `lacd.lock` for its lifetime. A second daemon against the same
state directory refuses to start and names the process holding it:

```
lacd: another lac daemon is already running (pid 4242). Stop it first, or point this one at a
      different state directory with --database and --socket
```

The kernel drops the lock when the process dies, so a crash leaves nothing to clean up. The case that
does need a person is an **orphan**: a daemon started by hand that outlives the service manager's
knowledge of it. The service then fails to start on every attempt with the message above, while the
orphan keeps answering on the socket. Kill the named pid and start the service again.

To run a second, independent daemon deliberately — for a test, say — give it its own state:

```sh
XDG_STATE_HOME=/tmp/lactest XDG_CONFIG_HOME=/tmp/lactest/config lacd
```

## Stopping, reloading, and what changes live

`SIGINT`/`SIGTERM` stops it gracefully: it stops accepting, lets in-flight calls finish for
`shutdown_grace`, removes the socket and releases the lock. A second signal restores the default
kill behaviour, so a wedged daemon can still be stopped.

Configuration is reloaded by `lac --name operator reload`, by `SIGHUP`, or on its own once the file
has settled — the same contents read three times at `restart_delay` apart, about fifteen seconds
after the last edit. Resources, capabilities, operators, worker commands and message retention apply
live. The socket, the database and the Telegram bridge need a restart, and a reload says so rather
than pretending. A configuration that does not parse leaves the running one alone.

## Checking it

```sh
lac info          # version, socket, database, uptime, methods
lac resources     # proves the configuration was read
lac agents        # who is registered, including the daemon's own components
```

`lac info` reporting a version you did not expect usually means the service is still running an older
binary: `go install` replaces the file on disk but does not restart anything.
