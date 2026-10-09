# Permissions and summaries

Activity is an optional runtime capability within protocol 1. Clients read each
host independently through the authenticated controller proxy. An older runtime
returns 404 for these endpoints; the client explains that an update is needed
without restarting the daemon or interrupting existing sessions. A compatible
new runtime is required, and approval hooks are attached only to newly started
ordinary Claude Code and Codex sessions.

## Permission requests

`GET /api/approvals` returns `{requests:[...]}`. Each request has `id`, `sessionId`,
`sessionCreatedAt`, `provider`, `toolName`, `input`, `cwd`, `createdAt`, `expiresAt`,
`status`, and optional `permissionMode`, `decision`, and `detail`. Status is
`pending`, `submitted`, `expired`, or `cancelled`. A submitted decision means
Relay handed the decision to the hook; it does not prove the tool ran successfully.

`POST /api/approvals/{id}/decision` takes
`{sessionId,sessionCreatedAt,decision:"allow"|"deny"|"terminal"}`. The first valid
decision wins atomically; later, stale, or wrong-session decisions return 409.
There are no automatic retries or bulk grants. `terminal` releases the hook
without a decision so the harness can display its own prompt. Opening a terminal
does not itself answer or release an approval request.

Only a per-session capability can call
`POST /api/sessions/{id}/approval-hook` over the private runtime socket. The CLI
helper normalizes provider input and waits for the exact request's result. The
controller strips Authorization before proxying runtime requests, preventing a
browser from impersonating this hook. Controller authentication, origin checks,
and CSRF checks apply to every user decision. Sessions on other hosts are never
addressed by a request intended for this host.

Requests expire after ten minutes. Disconnects, session exit, and runtime restart
cannot turn unanswered requests into grants. Limits are four pending requests
per session, 64 total, 128 recent records, and 64 KiB per hook payload. Raw tool
arguments are held in memory only, excluded from audit bodies and persisted
session metadata, and lost on daemon restart. Closing a client does not grant or
deny; another connected client can answer before timeout.

Session metadata optionally reports
`permissions:{support:"configured"|"active"|"terminal-only",detail,mode?,modeObservedAt?}`.
Configured hooks are not assumed to be trusted or active. A valid authenticated
hook must connect before integration is reported active. Modes are observed
facts, not editable policy. Relay does not auto-allow, weaken sandboxing, bypass
trust, copy credentials, or synthesize terminal keystrokes to answer prompts.

Conservative CLI compatibility checks use Claude Code 2.1.209+ or Codex 0.153.4+
with `hooks` enabled. Codex definitions still require trust in `/hooks`; managed
policy can disable them. Claude's sandbox network prompts do not emit the
PermissionRequest event. Terminal fallback remains available for these cases.
Official provider contracts:
[Claude hooks](https://code.claude.com/docs/en/hooks) and
[Codex hooks](https://learn.chatgpt.com/docs/hooks).

## Summary observer

`GET /api/observer` is read-only and returns `config`, `running`,
`activeSessionId?`, `summaries`, `error?`, `providers`, and
`limits:{requestsPerHour,remaining}`. It never invokes a model.

`POST /api/observer/config` takes the complete configuration:

```json
{"enabled":false,"provider":"claude","model":"haiku","intervalSeconds":300}
```

Intervals are 60–3600 seconds. Enabling validates installed CLI capabilities
without inference, then opts into future automatic observations on that host.
Disabling cancels its summary worker, leaving normal agents untouched.
`POST /api/observer/refresh` takes `{sessionId,sessionCreatedAt}`, requires enabled
settings, and returns 202 when queued. Wrong identity, unsupported session
purpose, and duplicate queued/running work return 409. The host-wide rolling
hour limit returns 429 when exhausted. Failed or interrupted calls need explicit
refresh; queued/running state recovered after a crash is not replayed.
Automatic observations require changed context. An explicit refresh can summarize
unchanged context again and counts toward the same usage cap.

Summary records contain identity, `provider`, `model`,
`status:"queued"|"running"|"ready"|"error"`, `summary`, `steps`, `nextSteps`,
`blockers`, `sampledAt?`, `generatedAt?`, `updatedAt`, `sourceStatus`, `stale`, and
`error?`. Earlier successful output remains visible after failure, retaining its
original provenance. Model text never sets real session or permission state.

The worker runs on the same machine as its observations, using that machine's
existing Claude Code authentication. A capability check requires 2.1.205+ and
all supported isolation flags. It uses a private temporary working directory,
disabled tools/MCP/hooks/skills/project settings, fixed system instructions,
structured JSON output, no conversation persistence, and no API-key or provider
routing overrides. It does not read authentication files into Relay or copy
credentials between hosts. The user chooses a model supported by their account;
no separate API key is required. Codex sessions are valid observation sources,
but Codex is not yet offered as the worker because a verified complete tool
disable mechanism is unavailable.

Only normal Claude Code/Codex sessions are observed. A bounded snapshot of the
terminal emulator's visible text is included as untrusted data alongside real
process status and an earlier summary. Complete historical output can also be
used after control sequences are removed. Recovered sessions with truncated
history and no trustworthy screen snapshot cannot be summarized; their terminal
remains available. Invisible clipboard/title control payloads are excluded.
Input is at most 32 KiB, output
128 KiB, each request has a two-minute deadline, and only one worker runs at a
time. At most 60 requests start per rolling hour per host. Attempts are recorded
before invocation so restart or disk errors cannot reset/replay uncertain work.
Raw observation prompts are not persisted by Relay. Summaries and configuration
are stored in private `observer.json`; terminal history retains its independent
retention rules. A model may still paraphrase sensitive ordinary terminal output,
so enable summaries only for machines whose agent context may be sent to that
provider. Explicit login and maintenance sessions are excluded.

Summary workers never create sessions, answer approvals, execute project tools,
or write `MEMORY.md`, `HANDOFF.md`, or instruction files. Provider conversations
and shared project memory remain separate from the dashboard cache.
