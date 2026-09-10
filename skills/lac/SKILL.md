---
name: lac
description: >-
  Use this skill when more than one AI agent may be working on this machine at the same time,
  and especially before running anything heavy: a test suite, a build, a long script, a
  benchmark, a container build. It explains how to queue for a shared slot with LAC (the local
  agents coordinator) instead of starting work that will thrash the machine, how to find out
  who else is working and where, and how to tell them what you are taking on.
  Trigger phrases include:
  "run the tests", "run the build", "who else is working on this", "tell the other agent",
  "is anyone else changing this file", "why is this waiting",
  and any request from the user to report what the agents are doing.
---

# LAC — coordinating with the other agents on this machine

You are probably not the only agent working here. LAC is a small daemon that keeps the agents on
this machine from tripping over each other. It gives you three things: a queue for scarce
resources, messages to and from the other agents, and a roster of who is working where.

Use it through the `lac_*` MCP tools if they are available. If they are not, the `lac` command line
does the same things — check with `lac info`.

## 1. Queue before you run anything heavy

This is the important one. The machine can only do so much at once. If four agents each start a
test suite, all four runs get slower and some of them fail for reasons that have nothing to do with
the code.

Before running a test suite, a build, or anything else that will use most of the machine:

```
lac_acquire_slot(resource: "test", reason: "running the parser tests")
… run the work …
lac_release_slot(lease_id: "<the id you were given>")
```

From a shell, one command does all three:

```sh
lac run --resource test -- make test
```

It waits for a slot, runs the command, releases the slot however the command ends, and passes the
exit status straight through. Prefer this form when you are running a shell command anyway: there
is no lease id to keep track of and no way to forget the release.

**Rules that matter:**

- Call `lac_resources` if you do not know what to queue for. Typical resources are `test` (a few
  concurrent runs), `reviewer` and `advisor` (usually one at a time).
- Being told to wait is normal, not an error. If `lac_acquire_slot` returns saying it timed out,
  call it again. Do not start the work anyway.
- Release the slot as soon as the work is done, **even if it failed**. Somebody is waiting.
- If you lose a lease id, `lac_my_slots` lists what you are holding.
- Do not queue for trivial things. A single unit test, a file read, a lint run on one file: just do
  it. The queue is for work that would slow the whole machine down.

## 2. Say what you are working on

Before starting on an area of the codebase, look at who else is here:

```
lac_agents
```

It lists the other agents and the directory each is in. If somebody else is in the same project,
tell them what you are taking on before you start:

```
lac_send_message(to: "claude-b", kind: "status", text: "I'm rewriting the parser tests in tests/parser/")
```

Read your own messages when you start work, and between tasks:

```
lac_inbox
```

Messages wait for you, so an agent that was busy still gets what it was sent. Reading acknowledges
them by default; they will not come back.

Answer questions from other agents when they arrive. If another agent says they are already
changing something you were about to change, believe them and pick something else, or ask.

Use `broadcast: true` sparingly — it interrupts every agent on the machine. It is right for "I am
about to rebase main" and wrong for "I finished a function".

## 3. Report to the user

When the user asks what everyone is doing, or why something is waiting:

- `lac_agents` — who is working, and where
- `lac_queue(resource: "test")` — who holds the resource and who is in line, in order

Summarise it in a sentence or two rather than pasting the raw output.

## When LAC is not running

If a tool reports that the daemon is not reachable, say so plainly and offer to start it:

```sh
lacd &
```

Do not silently carry on doing heavy work without a slot — mention that coordination is off, and
then use your judgement about how much to run at once.

## What LAC does not do

- It does not run your commands for you. It arbitrates; you do the work in your own directory.
- It does not tell you what to work on. It tells you who else is here and when it is your turn.
- It never reaches outside this machine, except an optional Telegram bridge the operator sets up
  for their own notifications.
