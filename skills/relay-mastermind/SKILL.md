---
name: relay-mastermind
description: Maintain Relay's Mastermind HTML pages and task board, and coordinate the user's work across connected Relay machines. Use inside the local Relay Mastermind session for plans, progress dashboards, handoffs, and remote delegation.
---

# Relay Mastermind

You run locally inside Relay's dedicated Mastermind workspace. The user's remote
machines have separate files, permissions, processes and provider accounts.
Your local provider subagents do not create sessions on those machines.

Use the executable in `RELAY_BIN`. In PowerShell, invoke it as
`& $env:RELAY_BIN`; in a POSIX shell, use `"$RELAY_BIN"`. Relay injects the private
controller connection and per-coordinator capability. Do not print, copy, or
embed that capability in pages, task details, terminal messages or other hosts.
If the capability is unavailable or revoked, report it; do not mint a browser
login token or search for another coordinator's credentials.

## Pages and tasks

Start with `workspace show` to read the current revision, pages and tasks. Use
`workspace pages get --id ID` to read a page before editing it. Keep useful
existing user content. Create several focused pages when the task benefits from
them, such as a plan, architecture notes, or a results dashboard. There is no
required page count or template.

Write a UTF-8 HTML file in your working directory, then publish it through Relay:

```sh
"$RELAY_BIN" workspace show
"$RELAY_BIN" workspace pages create --revision 0 --title "Plan" --html-file plan.html
"$RELAY_BIN" workspace tasks create --revision 1 --title "Inspect the target project" --status doing
```

The numbers above illustrate the first two writes to an empty workspace. Always
use the actual latest revision returned by Relay. Every successful mutation
returns the updated workspace and the changed item. Use `pages update --id ID`
or `tasks update --id ID` with its current title/content and the latest workspace
revision. Task statuses are `todo`, `doing`, `blocked`, and `done`; `--detail`
records evidence or blockers. Optional `--page`, `--host`, and `--session` link
a task to a page and the exact delegated session. `--help` documents each action.

HTML previews support ordinary markup, inline CSS, and embedded data images.
Scripts, event handlers, forms, external assets and navigation are disabled.
Use the native task board for checkboxes/status actions; JavaScript inside a page
cannot control Relay. Pages are limited to 128 KiB each, with 16 pages and 200
tasks per workspace. Keep large logs or artifacts in project files and summarize
their relevant results.

A revision conflict means someone changed the workspace. Read the current state
and incorporate those edits before resubmitting. After a lost reply, inspect the
returned or requested item ID before trying again. Do not automatically repeat
an uncertain create, update, or delete. Delete only content the user asked to
remove or disposable content you created for the task.

## Delegate across machines

Use `fleet list` to resolve the user's machine names. Inside Mastermind these
commands use the controller's already connected hosts; an offline or locked
connection needs the user's attention in Relay. Do not guess SSH destinations,
copy keys/provider tokens, or bypass host trust.

```sh
"$RELAY_BIN" fleet list
"$RELAY_BIN" session list --host "Build machine"
"$RELAY_BIN" session start --host "Build machine" --harness codex --mode manual --cwd /srv/project --workspace Review --title Reviewer
"$RELAY_BIN" session read --host "Build machine" --id RETURNED_ID
"$RELAY_BIN" session send --host "Build machine" --id RETURNED_ID --text 'Read project guidance, review the current change, and report findings.' --enter
```

Confirm the provider is ready before sending its task. A running process does
not prove readiness, progress, or completion. Read output to verify results and
update the task board accordingly. Keep the machine ID with every session ID.
Scope parallel work so agents do not overwrite each other's files. Use Auto
only when the user authorized autonomous delegation; a parent's mode does not
implicitly change a child session's permissions.

Terminal input respects Relay's single-writer lease. Wait for a viewer to release
control instead of forcing it. An accepted send confirms input delivery, not
task completion. Report lost/unknown outcomes and inspect state before another
mutation. Stop sessions only when termination is part of the user's request or
they are your own disposable test sessions.
