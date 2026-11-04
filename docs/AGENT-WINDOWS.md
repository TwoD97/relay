# Agent-requested terminal windows

Agents can ask the primary Relay desktop to open a separate viewer for an
existing session:

```sh
relay session open --host MACHINE --id SESSION_ID
```

Inside Mastermind, this uses its scoped private controller capability. Ordinary
Linux agents use the existing local runtime or authorized SSH route. First
start a session with `relay session start`, inspect the returned ID, then request
its window. Opening a viewer does not start another provider, send terminal
input, or grant the viewer keyboard control.

The request binds the host, session ID, and exact creation time. Setup and login
sessions cannot be opened through this automation. Only a primary native Relay
window consumes requests; detached terminal windows do not. Each request must
be claimed before invoking the native window command, so competing desktops
cannot both consume it. No arbitrary URL, executable, or shell text is accepted.

Requests expire after two minutes. The queue allows eight new requests per
minute, at most sixteen active requests, and sixty-four retained outcomes per
runtime. A lost claim or native response becomes uncertain and is never replayed
automatically. Inspect the desktop before making another explicit request.
Requests are transient and are dropped when the runtime restarts; session
metadata and PTYs are unaffected.

The private runtime API is `GET/POST /api/views`,
`POST /api/views/{id}/claim` with `{clientId}`, and
`POST /api/views/{id}/complete` with `{clientId,status:"opened"|"failed"}`.
The scoped Mastermind endpoint is `POST /agent/views` with
`{hostId,sessionId,sessionCreatedAt}`. Existing controller authentication, origin,
CSRF and private agent capability rules remain authoritative. Older runtimes
without `viewRequestsV1` do not support this feature; no fallback creates or
restarts a session.
