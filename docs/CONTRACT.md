# Relay implementation contract

Linux, WSL and native Windows clients; Linux amd64/arm64 remote hosts. A Go controller serves the
embedded desktop UI; a Linux Go runtime owns remote PTYs. Linux uses OpenSSH and
Windows uses native Go SSH for authentication and host trust. New
API namespace and data directories belong to Relay.

## Controller HTTP API (same origin, session cookie, CSRF)

- `GET /api/state`: `{version, hosts: Host[], maintenanceJobs, mastermind}`
- `GET /api/mastermind`: controller-owned coordinator configuration and status.
- `POST /api/mastermind/config`: `{revision,hostId,harness,agentMode,cwd}`;
  saves a new choice and queues one startup. Revisions use compare-and-swap.
- `POST /api/mastermind/start|disable`: `{revision}`; explicitly starts/resumes
  or disables automatic attachment. Disabling preserves the remote session.
- `POST /api/hosts`: `{name,target,port?}` -> Host. target is an OpenSSH alias or
  `user@hostname`; validated before argv use. Starts the setup PTY.
- `POST /api/hosts/{id}/connect`: retry login/bootstrap. No body.
- `POST /api/hosts/{id}/disconnect`: disconnect transport; leave remote sessions alive.
- `DELETE /api/hosts/{id}`: forget host; does not uninstall or kill remote sessions.
- `GET /api/hosts/{id}/setup-terminal`: WebSocket for SSH authentication terminal.
- `GET /api/hosts/{id}/directories?path=&prefix=&hidden=false`: directory browser.
  Returns `{home,path,parent,directories:[{name,path}],truncated}` with canonical
  directory paths and at most 200 matching subfolders. Empty path and `~` select
  the host home. Prefixes match literal names; hidden folders are optional.
  Lookup uses the existing authenticated SSH transport and never restarts or
  upgrades its runtime. Requests are cancellable, time/output/concurrency bounded,
  and a cancelled lookup does not close the shared SSH connection.
- `/api/hosts/{id}/runtime/*`: proxy to daemon `/api/*`, HTTP and WebSocket.
- `GET /api/bootstrap`: `{csrf,version}`. Mutations use `X-Relay-CSRF`.
- `GET /auth?token=...`: consumes startup token into HttpOnly SameSite=Strict cookie;
  hands off to `/` with a fixed local document for external/native navigation,
  or a same-origin redirect. Never put credentials in terminal websocket URLs.

Host: `{id,name,target,port,status,stage,error?,createdAt}`. status:
`disconnected|connecting|installing|online|error`. Stage is human-readable.
Poll state every 2 seconds, preserving form and terminal state.

## Runtime API (private Unix socket, SSH-only transport)

`GET /api/health`: `{version,protocol:1,home,hostname,capabilities:{mastermindV1:true}}`.
Capabilities are additive; older runtimes may omit them.
`GET /api/sessions`: Session[].
`POST /api/sessions`: `{title,workspace,cwd,harness,agentMode?,mastermindId?,cols?,rows?}` -> Session.
Harness `shell|claude|codex`. No arbitrary executable from this request.
`PATCH /api/sessions/{id}`: `{title?,workspace?}` -> Session.
`DELETE /api/sessions/{id}`: sends termination to process group, then removes session.
`GET /api/sessions/{id}/terminal`: websocket. Attach replays bounded terminal output.
`GET /api/sessions/{id}/history`: plain text bounded terminal history.
`POST /api/sessions/{id}/input`: `{data}`; explicit input, never queue offline.
`POST /api/sessions/{id}/events`: owning harness capability token in Authorization;
`{kind,source}` events contain no provider prompt/command payload.
`POST /api/sessions/{id}/attention/ack`: mark the displayed attention item reviewed.
`GET /api/harnesses`: Harness[]: `{id,name,installed,path?,version?,authenticated?,authDetail?,management?}`.
`management` is optional for older runtimes and contains `supportedActions`,
`source:"relay"|"external"|"missing"`, `strategy:"native"|"npm"`, and `detail`.
`POST /api/harnesses/{id}/install`: -> Session for installer PTY, no sudo.
`POST /api/harnesses/{id}/update|repair`: -> Session with the matching purpose.
Both download the latest verified release, retain prior release directories, and
atomically switch the managed launcher. Same-harness maintenance conflicts return
409. Install preserves healthy existing installations; update/repair prepare a
managed replacement without modifying external binaries or provider credentials.
`POST /api/harnesses/{id}/login`: -> Session for provider login PTY.

Session: `{id,title,workspace,cwd,harness,status,createdAt,updatedAt,exitCode?,attention?,permissions?,agentMode?,mastermindId?,purpose?,processIdentity?,recovery?}`.
`purpose:"login"` identifies an ephemeral provider-login transcript: memory only
while active, cleared on exit, and never persisted as terminal history.
Attention: `{kind:"completed"|"notification"|"permission",source,updatedAt}`.

Ordinary agent creation accepts `agentMode:"manual"|"auto"`, advertised through
`Harness.sessionModes`. Omission preserves provider-default launch behavior.
Auto explicitly selects full provider tool permissions and adds a bounded,
literal autonomy instruction; unsupported CLI flags reject creation. It does
not change existing sessions or grant OS/SSH authority. The persisted field
records launch choice, independently of hook-observed permission state. See
[ACTIVITY.md](ACTIVITY.md#agent-launch-modes) for the provider contract.

Native Mastermind startup requires `mastermindV1` and a signed-in provider. Its
`mastermindId` is a 32-character lowercase hexadecimal launch identity, never
derived from a title. The runtime persists it and adds a fixed coordinator role
to the CLI instructions without replacing project policy or upgrading Manual
permissions. Concurrent duplicate launch identities are rejected while their
session record exists. This is not a promise that deleted sessions can be
recreated idempotently: the controller never replays an unconfirmed POST.

The controller persists the selected host identity, canonical folder, launch
identity, and creation intent before dispatch. Recovery only lists sessions and
checks the exact token, harness, mode, folder, session ID, and creation time.
Unknown outcomes remain visible. Configuration changes preserve prior sessions;
the overview connects only to a fresh, exact matching session. A disabled
coordinator releases its viewer without stopping the agent. A corrupt optional
Mastermind state file disables that feature and preserves the file; other fleet
functions remain available.

On a compatible protocol-1 runtime without `mastermindV1`, the controller uses
`execution:"worker"` instead of `"native"`. It verifies the current Relay CLI and
selected provider's flags, then creates an ordinary shell session using only the
legacy request fields. It persists that session's exact ID and creation time
before recording input intent and sending one literal `exec` command. Startup
phases are `creating`, `worker-prepared`, `worker-dispatched`, then `attached`.
Recovery in an incomplete phase only observes sessions and receipts; it never
repeats create or input requests. Lost create responses without an exact returned
identity remain uncertain, even when a similarly named session exists.

`mastermind-worker probe|run|status` never opens the daemon database. The worker
verifies its inherited socket/session ID against the owning runtime and claims a
private, exclusive `<state>/mastermind-workers/<launchId>.json` receipt. It then
executes the provider in the same PTY and process. Attachment requires the exact
receipt, folder, session ID, creation time, provider, mode, and launch identity.
The runtime continues to describe this as a shell; controller state carries the
verified provider/mode without inventing runtime capabilities. Compatibility
workers do not install permission hooks; prompts remain in their terminal.
Neither preparation nor startup restarts the old daemon or an existing session.

An optional Mastermind `blocker` identifies setup or recovery needs (`host`,
`runtime`, `harness`, `folder`, `worker`, `launch`, `identity`, `storage`). Failed
worker preparation waits for an explicit setup check rather than repeatedly
uploading files. A setup check does not authorize replay of uncertain mutations.

Named fleet discovery is an agent CLI capability, independent of runtime HTTP
support. The controller publishes bounded, private snapshots beside each
connected host's runtime at `<state>/fleet/<controllerId>.json`. Snapshots carry
display names, IDs, resolved SSH user/hostname/port, source identity, revision,
and a ten-minute expiry. They contain no credentials or SSH configuration files.
The source identity and increasing revision persist across controller restarts;
complete snapshots replace removed hosts and reject delayed older revisions.
Refresh is asynchronous on fleet changes and once per minute.

`relay fleet list` reports these sources, including stale ones. Session commands
accept exact `--host NAME_OR_ID` and optional `--fleet CONTROLLER_ID`; stale,
unresolved and conflicting matches fail before session mutation. The source host
uses its owning runtime socket. Other hosts use the resolved endpoint through
the caller's existing SSH trust/authentication and the fixed Relay bridge.
Loopback endpoints are not portable between hosts. Discovery does not create
SSH trust, copy credentials, or authorize a session operation. Raw `--ssh`
remains available for destinations outside the saved fleet.

Optional approval and observer routes are specified in [ACTIVITY.md](ACTIVITY.md).
Older runtimes may return 404 for these capabilities without becoming incompatible.
Status is evidence-based `running|exited|interrupted`; do not fabricate agent approval
state from incidental screen text. Session metadata persists atomically; after daemon
restart previously running sessions become interrupted. Desktop/SSH disconnect does
not stop processes. A host reboot cannot preserve original processes.

Optional `processIdentity:{pid,startTime,bootId}` records the Linux session
leader before terminal input is accepted. After an unexpected daemon exit,
`recovery:{status,detail}` explains interrupted-session cleanup. Only matching
boot/leader/start-time/owner identity permits a bounded same-session process
census and pidfd signalling. Missing/reused identities and unsupported signalling
fail closed; legacy metadata remains readable. Recovery also adds a retained
history notice for compatible older clients. It never resumes the old PTY or
automatically repeats its work; detached descendants are outside this guarantee.

Websocket shared format: server sends binary terminal bytes; client sends JSON
`{type:"input",data:string}` or `{type:"resize",cols:number,rows:number}`.
Runtime terminals attach as observers. `{type:"claim"}` and `{type:"release"}`
manage a single writer lease; a lease expires after 60 seconds without input.
Server text frames `{type:"control",owner,available,reason?,cols,rows}` report
ownership and canonical geometry. SSH setup terminals do not use this lease protocol.
One read loop and one write loop per websocket. Clients reconnect explicitly/with
backoff and replay history once per attach. Bounded output and backpressure required.

## Go package boundaries

- `internal/runtime`: `New(stateDir, version string) (*Server,error)`,
  `(*Server).Handler() http.Handler`, `(*Server).Close() error`.
- `internal/controller`: local authenticated UI/API, host records and orchestration.
- `internal/transport`: Linux OpenSSH / Windows native SSH lifecycle, directory
  browsing, and stdio net.Conn.
- `cmd/relay`: `ui`, `daemon`, `ensure`, `bridge`, `version` subcommands.
  `ensure` starts a detached daemon and waits for health, never replaces a live daemon.
  `bridge` relays stdin/stdout to the private Unix socket.
- `web/`: React/TypeScript + xterm, compiled and embedded by Go.

## Delivery checks

Go unit/integration + race tests, frontend typecheck/build, browser smoke on desktop
and phone, real disposable SSH host integration (login, deployment, reconnect,
session survival), cross-build linux amd64/arm64, reproducible local build script,
checksums, explicit deployment/recovery documentation. No production host mutation
until a validated build and an explicit target are available.


## Native desktop and shared controller

`relay desktop` ensures one controller per private client state directory and
prints a single JSON launch record: `{url,address,version,protocol:1,pid}`.
Each desktop window connects its embedded interface to the same local HTTP
origin using a fresh, expiring, one-use launch link without rotating existing cookies or CSRF tokens.
Controller discovery and token minting use a private Unix control socket on Linux
or a current-user-only, peer-verified named pipe on Windows.
A lifetime file lock prevents independent controllers from overwriting one host
store. Existing controller reuse requires the same build and local runtime
configuration. A new client stages its verified private release before a
serialized handover, checks the kernel peer's process identity and configuration,
and preserves the loopback origin. The optional private handover capability uses
a per-process generation and performs cooperative controller shutdown once.
Lost replies never cause a repeated stop or force-kill escalation. Legacy
termination requires a pinned OS process handle and a verified immutable private
release; unverifiable legacy Linux launches require manual intervention.
Closing the native window leaves the controller and PTYs running.

The undecorated native window contains a controller content webview and separate
bundled title-bar/menu webviews. Only the bundled frame labels and exact local
URLs may invoke the closed set of window/menu actions, checked by both Tauri
capabilities and the native handler. Controller content may only read/save the
appearance preference; the native handler checks its exact current loopback
origin and content label independently of that narrow Tauri capability.
System/Light/Dark is stored in native app configuration, outside controller-port
storage. Native broadcasts synchronize all three views without reconnecting PTYs.
All content navigation remains restricted to the authenticated controller origin
and bundled loading/error pages. No arbitrary shell, filesystem, or URL-opening
action is available to the frame.

Runtime API protocol 1 is independent of the client build identifier. Clients
accept a compatible protocol with a valid server build identifier, preserving
sessions across UI-only updates. Breaking runtime API changes require a new
protocol number; optional fields must remain optional for older servers.

## Reconnect, maintenance and agent coordination

Controller creation, successful private desktop launch, and authenticated embedded interface
startup each request idempotent saved-host reconnect. Four concurrent connection
attempts are allowed; a pending or online connection is never replaced. This is
one launch attempt, not a password-retry loop. Closing views or the controller
closes local transports, never remote daemons or their PTYs.

`POST /api/reconnect` returns `{queued}` with HTTP 202. Host records may include
`setupWarning` and `runtimeOperation` with `status`, `stage`, `error?`,
`installedVersion?`, `runningVersion?`, and `restartRequired?`.
`POST /api/hosts/{id}/repair-runtime` starts bounded asynchronous verification and
activation of the bundled binary. It preserves the currently running compatible
daemon and its sessions, even when its build differs.

`POST /api/hosts/{id}/harnesses/{harness}/{action}` accepts `claude|codex`
and `update|repair`, returning HTTP 202 with a durable controller-owned
`MaintenanceJob`. `/api/state` includes optional `maintenanceJobs`. A job contains
`id`, `hostId`, `harness`, `action`, `status`, `stage`, `createdAt`, `updatedAt`,
and optional `sessionId`, `sessionCreatedAt`, `error`, and `cleanupStatus`.
Status is `preparing|starting|running|succeeded|failed|uncertain`; cleanup is
`pending|removed|retained|uncertain` when present.

Remote work stages the verified current CLI, creates a setup shell on the
existing daemon, and sends one fixed maintenance invocation. Local work requires
the runtime's advertised maintenance capability. Only exact controller-owned
host/session identities, including creation times, with confirmed exit code 0
are removed. These worker sessions are omitted from ordinary client session
lists; retained output remains available through its job. No ownership is
inferred from titles or purpose. The standalone worker never overwrites runtime
session metadata. The journal survives controller restart; observation resumes
after reconnect. Unknown creation/input/deletion outcomes are not replayed.
At most 16 jobs may be active and 256 retained; only successfully cleaned jobs
can be pruned to make room.

`POST /api/hosts/{id}/project-context` accepts `{path}` and returns a resolved
`path`, `files: [{path,status}]`, and `warnings: string[]`. It prepares opt-in
shared project instructions and notes without accessing native provider chat,
memory, or authentication stores. Remote paths travel as JSON stdin to a fixed
CLI command. See [shared project context](SHARED_CONTEXT.md) for filesystem
preservation, compatibility, and partial-failure behavior.

`relay session list|start|read|send|stop --ssh TARGET [--ssh-port PORT]` bridges
the private runtime API through a constant SSH command. Input remains JSON, strict
host-key checking and batch authentication are required, and no credentials or
agent sockets are forwarded. Target IDs remain host-qualified by the caller.
Read-only observation never acquires the writer lease; input respects it.

The skill is embedded from `skills/relay/SKILL.md` and installed in Relay's own
remote data directory. Personal Codex and Claude skill locations link to that
copy; conflicting user-created entries are left intact. Registrations
from earlier installations are upgraded only when the existing entry is a
symlink to a data-directory skill containing just an unmodified known bundled
`SKILL.md`. Relay preserves the old directory and saves the original link under
its managed `skills/.registration.*` directory before replacing that link.
Changed skills, extra files, real directories, and unknown targets remain
conflicts for the user to resolve; reconnecting does not overwrite them.
Registration never restarts a runtime or migrates its sessions. Discovery locations
follow the [Codex skill documentation](https://learn.chatgpt.com/docs/build-skills)
and [Claude Code skill documentation](https://code.claude.com/docs/en/skills).
