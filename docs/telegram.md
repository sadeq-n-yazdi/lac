# The Telegram bridge

The bridge lets you ask what the agents are doing from your phone, and lets an agent reach you when
you are not at the machine. It is **off unless you configure it**, and it is the only part of LAC
that talks to anything outside your computer.

It talks in one direction: the daemon calls Telegram, Telegram never calls in. There is no
listener, no port to open, and no inbound connection to firewall.

## 1. Make a bot

Message [@BotFather](https://t.me/BotFather) on Telegram, send `/newbot`, and follow it. You get a
token that looks like `123456789:AAF-abcdefghijk…`.

**That token is a credential.** Anyone who has it can read everything you ask the bot and tell your
agents whatever they like. Put it in a file of its own:

```sh
mkdir -p ~/.config/lac
printf '%s\n' '123456789:AAF-…' > ~/.config/lac/telegram.token
chmod 600 ~/.config/lac/telegram.token
```

The daemon refuses to start if that file is readable by other users.

## 2. Find your chat id

Message your new bot — anything, `/help` will do — then start the daemon with the token set and no
allowlist yet. It will refuse to start and tell you why; set a placeholder allowlist of `[0]`,
start it, message the bot again, and read the id from the daemon's log:

```
level=WARN msg="ignored a telegram message from a chat that is not on the allowlist" chat=987654321
```

That number is your chat id.

## 3. Configure it

In `~/.config/lac/config.yaml`:

```yaml
telegram:
  enabled: true
  token_file: /Users/you/.config/lac/telegram.token
  allowed_chat_ids:
    - 987654321
  # How long each long poll waits. The default is fine.
  # poll_timeout: 30s
  # How long /report waits for the agents to answer. It is a wait on a phone, so keep it short.
  # report_deadline: 30s
```

`allowed_chat_ids` is not optional and cannot be disabled. Without it, anybody who found your bot
could drive this machine. Messages from any other chat are ignored silently — no reply, because a
reply would confirm the bot is worth poking at — and recorded in the audit log.

If you would rather not have the token on disk at all, set `LAC_TELEGRAM_TOKEN` in the daemon's
environment instead; it takes precedence over both `token_file` and `token`.

Restart the daemon. It logs `telegram bridge started` with the bot's username.

## What you can ask it

| Command              | What it does                                            |
|----------------------|---------------------------------------------------------|
| `/agents`            | Who is working, and in which directory                  |
| `/resources`         | The shared resources and how busy each one is           |
| `/queue <resource>`  | Who holds it and who is waiting, in order               |
| `/report [question]` | Ask every agent what it is doing, and wait for answers   |
| `/say <agent> <text>`| Send a message to one agent                             |
| `/broadcast <text>`  | Tell every agent at once                                |
| `/help`              | The same list                                           |

Anything that is not a command is treated as a question for everybody, so typing *"what is
everyone doing?"* works as well as `/report`.

## Agents reaching you

The bridge appears on the roster as an agent called `telegram`, so an agent can message you
directly:

```sh
lac send telegram question "the migration failed, should I roll back?"
```

Through MCP, the same thing: `lac_send_message(to: "telegram", kind: "question", text: "…")`. It
arrives on your phone with the sender's name.

## If something is wrong

- **The daemon refuses to start** — read the message; it names the setting. A missing allowlist and
  a group-readable token file are the two common ones.
- **`telegram rejected the bot token`** — the token is wrong or was revoked in BotFather. The
  bridge stops; the rest of LAC carries on.
- **Nothing happens when you message the bot** — your chat id is probably not on the allowlist.
  Check the daemon's log for the "ignored a telegram message" line.
- **The bridge stops but the daemon keeps running** — that is deliberate. Coordination between your
  agents does not depend on your phone.

## What it never does

- It never accepts an inbound connection.
- It never sends your code, your files or your commands anywhere: it sends what the agents chose to
  report, and what you asked.
- It never talks to a chat that is not on your allowlist.
