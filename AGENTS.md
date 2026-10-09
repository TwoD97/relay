# Working on Relay

Relay is a personal control plane for Linux terminal sessions, with browser and
native desktop clients. Keep shared session facts in the runtime/controller API;
keep navigation, layout, and temporary view state in the client.

- `cmd/relay`: command entry points and platform-specific controller lifecycle.
- `internal/runtime`: persistent Linux PTYs, agent tools, and session recovery.
- `internal/controller`: local authenticated API, saved hosts, and background jobs.
- `internal/transport`: SSH authentication, deployment, and private runtime bridges.
- `web`: React client and browser tests.
- `desktop`: Tauri shell and platform-specific window integration.

Preserve live sessions across client disconnects and controller upgrades. Do not
restart remote daemons as an incidental step of a UI or binary update. Unknown
mutation outcomes must remain visible and must not be replayed automatically.
Use explicit identity evidence before cleaning up sessions or processes.

Keep platform-dependent behavior in platform files. Preserve optional API
capabilities when communicating with compatible older Relay runtimes. Never
infer maintenance ownership from a session title. Do not copy provider tokens
or authentication files between hosts.

Build with `./scripts/build.sh`. Run `./scripts/check.sh` for Go, installer, and
browser checks. Changes to SSH or recovery should also run the disposable SSH
integration tests documented in the README. Test fixtures must never use saved
personal host profiles or provider accounts. Do not commit generated packages,
terminal transcripts, private host configuration, screenshots of real machines,
or authentication output.
