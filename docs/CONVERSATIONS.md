# Saved conversations

The New session dialog can list saved Claude Code or Codex conversations for
the selected Linux machine and project folder. Starting fresh preserves the
existing provider defaults. Resume passes the selected UUID to the provider's
native resume command; it never pastes old transcript text into a new chat.

Listings contain at most 100 entries: a conversation UUID, provider name or
Relay display name, canonical project folder, and modification time. They do
not contain prompts, assistant replies, tool output, or account information.
Unnamed conversations display their provider and short UUID. A truncated list
is labeled rather than presented as complete.

Claude Code metadata uses bounded head/tail reads of its local session files,
following the layout used by the official SDK. Only explicit title records and
project identity are extracted. **Rename in Relay** saves a private alias on
that same machine; it does not change Claude Code's transcript or `/resume`
name. Codex uses its app-server metadata API and native thread-name operation,
so a successful Codex rename is also visible to Codex.

History lookup may refresh the Relay helper binary without restarting the
daemon. Resume additionally requires the live runtime's explicit
`conversationResumeV1` capability, also advertised as
`Harness.conversationResume`. An older runtime can still list conversations
and rename them, but cannot resume through this dialog until it is updated.
No resume field is silently ignored, and no missing conversation falls back
to a fresh provider process.

Before starting, the runtime rechecks the UUID, exact canonical project, and
installed CLI's resume support. A conversation already running in this Relay
daemon is rejected; open its existing terminal instead. A provider startup
error remains visible in the terminal and is not retried or downgraded.
Separate external provider processes are outside this duplicate check.

`Session.conversationId` is captured only from a validated provider UUID on an
authenticated session hook or Codex notification, or from the explicit resume
request after preflight. `Session.resumedFrom` records that requested resume
identity. Existing sessions without this evidence keep these fields absent;
screen text and titles are never used to guess a conversation ID. If a provider
changes conversation inside a running terminal, a later authenticated event
can update its current conversation ID.

The controller exposes `GET /api/hosts/{id}/conversations?harness=...&cwd=...`
and `PATCH /api/hosts/{id}/conversations/{uuid}` with
`{harness,cwd,title}`. Both use normal controller authentication and origin
checks; PATCH additionally requires CSRF. Rename failures with an uncertain
outcome require refreshing the list before another explicit attempt. No
mutation is replayed automatically.

Provider references: [Claude session management](https://code.claude.com/docs/en/sessions),
[Claude SDK metadata helpers](https://code.claude.com/docs/en/agent-sdk/typescript#listsessions),
[Codex app-server protocol](https://learn.chatgpt.com/docs/app-server), and
[Codex resume command](https://learn.chatgpt.com/docs/cli/reference#codex-resume).
