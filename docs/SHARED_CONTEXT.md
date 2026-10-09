# Shared project context

Relay can prepare shared Markdown instructions and notes when **Shared instructions
and memory** is selected while creating a Claude Code or Codex session. It writes
only to the explicitly selected Linux project folder. The same setup is available
with `relay project-context --path /absolute/project` on a Linux host.

The setup creates `AGENTS.md`, `CLAUDE.md`, `MEMORY.md`, and `HANDOFF.md` when
missing. Existing instruction files receive one marked block; existing bytes and
file permissions are preserved. Existing memory and handoff notes are never
overwritten. Claude's block imports the other three files. If an existing
`AGENTS.override.md` is present, it receives the shared-notes block too, and Relay
reports its normal precedence over `AGENTS.md` for Codex.

These files share project guidance, durable facts, and a concise handoff. They do
not merge Claude and Codex native conversations, private generated memories, or
authentication. Existing sessions may have already loaded their instructions;
start a new session to pick up the prepared project files. Later sessions and
humans can update the notes with relevant decisions and progress. Relay does not
automatically summarize private transcripts or run a model to populate them.

Review the new project files before committing them. Keep credentials and raw
conversation histories out of shared notes. Existing ancestor instructions,
nested instruction files, and provider context limits still apply.

## Safety and compatibility

The controller endpoint is `POST /api/hosts/{id}/project-context`, with an
authenticated, same-origin, CSRF-protected JSON body `{ "path": "/project" }`.
Unknown fields are rejected. Success returns the resolved `path`, a `files` array
of relative names and `created`, `updated`, or `preserved` statuses, and `warnings`.
The Windows controller prepares files on connected Linux hosts; it does not write
to native Windows project paths.

Remote preparation stages the matching verified Relay CLI if needed, then sends
the selected path as JSON on stdin to a fixed command. It works alongside an older
compatible daemon without replacing that daemon or interrupting its sessions.
The local Linux action does not need a runtime API or restart. Preparing CLI files
is serialized with other host maintenance operations.

Files must be regular, singly linked UTF-8 text owned by the current user, at most
128 KiB. Symlinks, hardlinks, special files, and edited or incomplete managed
blocks are rejected before changes begin. Project folder symlinks are resolved
first; file operations use directory descriptors without following new symlinks.
Cooperating preparations lock the directory. New files are published atomically
without replacing an existing name; managed blocks use a single append write.

The collection of files is not one filesystem transaction. Disk failure,
cancellation, or an external editor changing files during setup can leave partial
preparation; Relay reports an error and asks for review. It never truncates user
files to roll back, because doing so could erase concurrent edits. Directory
locking cannot stop unrelated editors that do not take that lock. No setup can
prevent a user or another process from changing the files afterward.

## Official behavior checked on 2026-10-09

- [Codex AGENTS.md discovery](https://learn.chatgpt.com/docs/agent-configuration/agents-md):
  instructions are collected at session start; `AGENTS.override.md` takes
  precedence, and the default combined instruction limit is 32 KiB.
- [Codex memories](https://learn.chatgpt.com/docs/customization/memories): native
  generated memory is a separate local store. Checked-in instructions are the
  appropriate place for required durable guidance.
- [Codex projects and chats](https://learn.chatgpt.com/docs/projects?surface=cli):
  chats retain their own transcript and working directory; project files can
  provide context across chats.
- [Claude Code memory and imports](https://code.claude.com/docs/en/memory):
  `CLAUDE.md` supports relative `@file` imports. Recent Claude versions can also
  discover `AGENTS.md` under documented conditions, but an import bridge remains
  useful when Claude instruction files already exist or older versions are used.
  Claude's automatic memory is separate from these project files.
