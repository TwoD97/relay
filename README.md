# Relay

A personal workspace for coding agents across your machines. Add an SSH host,
sign in through its terminal, and Relay installs its Linux runtime without sudo.
Keep Claude Code, Codex, and shell sessions together in the browser or native
desktop app. Sessions survive browser and SSH disconnects.

Built with Go, React, and Tauri. Licensed under [Apache 2.0](LICENSE).

## Desktop and browser clients

The desktop app uses the same interface, hosts, and sessions as the browser.
On Windows x64, install `relay-desktop-windows-amd64-setup.exe`, then open
**Relay** from the Start menu. The Windows window, local controller, and SSH
client run natively; WSL is not required. Windows clients manage Linux SSH hosts.
Local Windows terminal sessions are not implemented.

On Linux and WSLg, open **Relay** from your application launcher or run
`relay-desktop`. It starts the local controller automatically. The native
**Relay → Open in Browser** menu opens a separately authenticated browser view;
both clients can be used together. Closing either view leaves agent sessions
running. **Reconnect** obtains a fresh connection after a controller restart.

Opening the desktop or browser reconnects saved hosts, with at most four SSH
setups at a time. Already connected hosts are reused. Passwords and new host-key
decisions still require attention in the host's setup terminal; passwords are
never saved. A failed login is not retried in a background loop.

The desktop has a dark custom title bar with dragging, minimize, maximize,
restore, close, and a Relay menu. Its trusted frame is separate from the app
content; no filesystem or shell APIs are exposed to either view. SSH and process
management stay in the controller/runtime.

```sh
./scripts/build-desktop.sh
sudo apt install "$(pwd)/dist/relay-desktop-linux-amd64.deb"
relay-desktop
```

Build prerequisites and native test commands are in `desktop/README.md`.
Native packages target Windows x64 and Linux amd64/arm64, including WSLg.
Windows and Linux desktop apps each expose their own authenticated loopback
browser view through **Relay → Open in Browser**.

## Build and start the browser client

Build requirements: Go 1.26+, Node.js 22+, npm, Python 3, Bash, and OpenSSH client.
Clients run on Linux or WSL; remote hosts need Linux amd64/arm64, a POSIX shell,
OpenSSH server, and `sha256sum`.

```sh
git clone https://github.com/TwoD97/relay.git
cd relay
./scripts/build.sh
./dist/install.sh
~/.local/bin/relay ui
```

Open the private login link printed by the command. The UI listens only on
`127.0.0.1:7340`; each link signs in once and then disappears from the address bar.
Run `relay ui` again to issue a fresh link without interrupting connected clients.
WSL users can open the link in their Windows browser. Use `--listen 127.0.0.1:7341`
if the default port is occupied.

The local computer is available immediately. To connect another machine, choose
**Add host**, enter a saved SSH alias or `user@hostname`, and complete any password,
key-passphrase, or new host-key prompt in the SSH terminal. Check the displayed
host fingerprint against a trusted source before accepting a new host. Existing
OpenSSH configuration supplies connection settings. The Linux client supports
jump hosts. The Windows client supports direct hosts, aliases, private keys,
key passphrases, the Windows OpenSSH agent, and password authentication;
ProxyJump and ProxyCommand are currently rejected with an explicit error.

After connection, install missing harnesses from the host view, then use their
**Sign in** action. Authentication already present on that host is reused. Login
can require a browser/device approval by the account owner. No password or account
credential is copied between machines.

Provider login output is kept in memory only while the login session is active;
its transcript is not written to disk and is cleared when login exits. The
provider CLI manages its own authenticated credentials in its normal location.
Automatic Node installation uses glibc Linux builds; musl-only hosts need a
compatible existing Node installation.

The terminal is a real PTY. In **Start a session**, choose **Browse** to select an
existing project folder on the connected host. Navigate Home, parent folders, or
breadcrumbs, optionally show hidden folders, and choose **Use this folder**.
Typing a path shows matching subfolders; `~` uses the remote home directory.
Spaces and punctuation in folder names are preserved. Browsing uses the existing
SSH connection and does not restart or update a remote runtime. Remote browsing
requires the Linux `find` and `head` utilities. Terminal input is sent only while
connected and is never queued for later execution.

Click the terminal and type directly; there is no separate command field.
Arrows, Tab completion, and Escape go to the remote PTY. Ctrl+C copies selected
text and interrupts the remote process when nothing is selected. Ctrl+V pastes
text; Ctrl+Shift+C/V also copy and paste. Pasted text respects bracketed-paste
mode. Touch screens retain a small row for keys absent from their keyboard.

Opening a terminal watches it. Clicking or focusing it claims available exclusive
typing and resize access; **Take control** also remains available. Other tabs
continue watching and cannot take an occupied lease. Closing the view or leaving
it idle for a minute releases control. Provider hooks report attention separately
from the underlying process status. Codex's per-session notification setting is
used by Relay while that session runs; configuration files are not edited.

## Permissions and session summaries

Open **Activity** for pending permission requests across your connected machines.
Each request shows the originating session and exact tool arguments. Choose
**Approve once**, **Deny**, or **Continue in terminal**. Decisions apply to one
request and never change provider policy or grant future tools access. A request
accepted by another client cannot be answered a second time. Requests expire;
lost replies are not replayed. Existing terminal sessions remain fully usable.

Approval integration requires a new session on a compatible runtime and provider
CLI. Relay conservatively checks Claude Code 2.1.209+ or Codex 0.153.4+ with hooks
enabled. Codex requires reviewing and trusting the definitions in `/hooks`.
Relay does not bypass hook trust, enable hooks against your configuration, or
override permission settings. Unsupported prompts (including Claude sandbox
network prompts), older CLIs, and untrusted hooks remain in the terminal. See
[the activity contract](docs/ACTIVITY.md) for exact guarantees and limitations.

Optional summaries are configured per machine in **Activity → Summary settings**.
They use that host's existing Claude Code sign-in and a configurable model
(default `haiku`) to summarize both Claude Code and Codex sessions. Summaries
start disabled. Enabling them sends bounded terminal excerpts to the provider
and consumes account usage. The worker has tools, MCP, hooks, and project
instructions disabled; it cannot approve requests or type into terminal sessions.
Codex is not yet available as the summary worker because a supported way to
disable all its tools has not been verified.

Automatic summaries require changed agent context, with a default five-minute interval,
one worker per host, and at most 60 requests per hour. Login, setup, shell, and
maintenance sessions are excluded. The dashboard labels stale summaries and
keeps their sampling time, model, and reported steps. Model output is advisory;
verify it in the terminal. Failed or interrupted requests require an explicit
refresh and are never automatically retried. Explicit refresh also counts toward
the usage cap, including when context is unchanged. Summaries do not write shared
project memory or merge provider conversations.

## Use from an agent or script

The local session API is also available as a CLI. Commands inside a managed
session select its owning runtime automatically. These commands never retry a
mutation or bypass a harness's permission settings.

```sh
relay session list
relay session start --harness codex --cwd ~/projects/app --workspace App --title Review
relay session read --id SESSION_ID
relay session send --id SESSION_ID --text 'Run the tests' --enter
relay session stop --id SESSION_ID
```

`session stop` terminates and removes that terminal. Browser control must be
released before another client uses `session send`.

## Install the client bundle

Keep both architecture binaries and `SHA256SUMS` together: the client sends the
matching runtime to a remote host. The install script validates the complete bundle
and installs a versioned client plus a `~/.local/bin/relay` command.

```sh
./dist/install.sh
~/.local/bin/relay ui
```

If invoking the binary through a separate symlink or copied location, use
`ui --binaries /path/to/the/complete/bundle` to select remote artifacts.

## Persistence and upgrades

Relay stores personal configuration and runtime state outside its installation.
This standalone distribution uses the following paths. It does not automatically
import state from development builds that used different storage directories.

- Client profiles: `~/.local/share/relay-client/hosts.json` (mode 0600).
- Client mutation audit: `audit.jsonl` beside the profiles, rotated at 8 MiB;
  request metadata only, with no message bodies or credentials.
- Runtime data: `~/.local/share/relay/` (mode 0700).
- Private daemon socket: `run/daemon.sock` beneath the runtime directory.
- Daemon log: `daemon.log`; terminal/session state lives beneath the same directory.
- Remote builds: `releases/<version>/<sha256>/relay`, with an active `bin/relay` symlink.

On Windows, controller data lives under `%LOCALAPPDATA%\RelayData\relay-client`,
separately from the installation directory.
Private files and the local control pipe are restricted to the current Windows
user. Background controllers run from verified private release copies so the
installed desktop files can be upgraded without replacing running sessions.

Closing the client, disconnecting SSH, or forgetting a host leaves the remote
daemon and sessions running. An intentional daemon stop ends its terminals;
rebooting a machine ends its processes. A hard daemon crash can leave processes
behind. On restart, Relay marks saved running sessions interrupted and attempts
bounded cleanup only when saved boot/start-time identity and pinned Linux process
handles prove ownership. Missing or changed identity produces a visible recovery
warning and no guessed termination. Detached descendants and a crash before
identity is saved cannot be fully recovered. Processes are not silently restarted
and prompts are not replayed.

Normal terminal history retains the latest 1 MiB per session and is checkpointed
every five seconds. A hard crash may lose output since the last checkpoint.

Installations upload to a private temporary file, verify SHA-256 on both ends,
rename atomically, and verify runtime health before activating the bridge. A
running runtime with the same API protocol can be reused across client builds,
so a desktop or browser update does not stop its sessions. An incompatible
runtime protocol causes an explicit error. Finish and close sessions before
intentionally stopping the old daemon and reconnecting with a new build. The
controller itself requires a matching client build. Opening an updated desktop
verifies and stages the replacement before handing over the private controller
at the same loopback address. Concurrent launches share one handover; runtime
daemons and their PTYs remain running. Views may need **Reconnect** to sign in
again. An older Linux controller launched from an unverified mutable path
requires the exact manual stop shown in its error message. Ambiguous handover
responses never trigger repeated or escalated termination.
Old version directories are retained for manual rollback. Back up the
runtime and client directories while the daemon/client is stopped.

## Access and operation

Each host's tools panel offers **Update** and **Repair** for Claude Code and
Codex. These prepare a verified, separate managed release and switch new sessions
to it; existing agents keep running and provider login files stay in place.
Progress appears on the machine page without taking over your current session.
Updates and repairs run as background jobs, including on older compatible
daemons. Successful workers exit and their temporary sessions are removed
automatically. Failed or uncertain jobs retain their output for inspection.
The controller remembers jobs across restarts and resumes observing them after
reconnecting; it never blindly repeats an update with an unknown outcome.

When an agent or shell exits, **Open terminal here** starts a fresh shell in the
same folder and workspace while keeping the original output. **Close session**
removes the finished session and its saved output after confirmation. Project
files stay on the host.

When starting Claude Code or Codex, optionally enable **Shared instructions and
memory**. Relay prepares shared `AGENTS.md`, `MEMORY.md`, and `HANDOFF.md` files
with a `CLAUDE.md` import bridge in that project folder. Existing content is
preserved. These notes carry guidance and handoffs between agents; native chat
histories and private provider memory remain separate. See
[shared project context](docs/SHARED_CONTEXT.md) for behavior and limits.

**Update / repair runtime** verifies and installs the bundled Relay executable.
The panel reports installed and running versions separately. A running daemon is
never restarted by maintenance; a staged daemon update takes effect at its next
start. Host shutdown or an intentional daemon stop still ends live processes.

The bundled [Relay skill](skills/relay/SKILL.md) is registered for Codex and Claude
Code when a remote host connects. Existing user-authored skills with that name
are preserved and shown as a setup warning. The skill can list, start, read,
send input to, and stop sessions, while respecting the terminal's writer lease.
Use `$relay` in Codex or `/relay` in Claude Code.

For another Linux machine, session commands accept `--ssh user@host` (or a saved
SSH alias) and optional `--ssh-port`. The source machine needs its own trusted SSH
key access and an OpenSSH client; a desktop connection does not automatically
grant credentials to agents on other machines. Prompts travel as HTTP JSON over
SSH, not interpolated commands. Mutations are never automatically retried.

```sh
relay session list --ssh build-host
relay session read --ssh build-host --id SESSION_ID
relay session send --ssh build-host --id SESSION_ID --text 'Review the diff.' --enter
```

The remote runtime binds a user-private Unix socket; it opens no remote TCP port.
The local UI requires a random session cookie, checks Host and Origin, and requires
CSRF tokens on mutations. SSH passwords stay in the SSH process; Relay never saves
them. Do not expose the loopback UI through a public reverse proxy: this build is a
single-user local client, not a hosted multi-user service.

Phone layouts are supported when accessing the client through a trusted private
tunnel that preserves the loopback endpoint. A hosted multi-user service and
push notifications are not included.

Check `daemon.log` for startup failures. An offline host can be reconnected from
the host menu. A failed mutation is not automatically retried: inspect session
state before repeating it. Run `relay help` and `relay ui --help` for
configuration flags.

## Validation

```sh
./scripts/check.sh
# Native SSH protocol fixtures and real disposable two-host orchestration:
go test -race -tags relay_ssh_native ./internal/transport -count=1 -timeout 1m
RELAY_SSH_INTEGRATION=1 go test -race ./internal/transport -run TestRealSSH -count=1 -v -timeout 10m
# Browser against the actual built controller/runtime:
RELAY_LIVE_BINARY="$PWD/dist/relay-linux-amd64" npm --prefix web test -- e2e/live.spec.ts
```

The Go suite checks process persistence, bounded terminal streams, HTTP and
WebSocket access controls, state persistence, safe artifact upload, and SSH
transport behavior. The frontend suite checks operator flows at phone and desktop
sizes. See `docs/VALIDATION.md` for the exact checks executed for this build and
remaining release limits.

Linux controller discovery and safe process signalling require pidfd support
(Linux 5.3 or newer); the native Windows client uses Windows process handles.

The SSH integration gate requires Docker and builds disposable Linux hosts with
distinct host keys. A parent terminal on one host creates and controls a worker
on the other. It tests host isolation, writer leases, rejected trust/authentication,
dropped replies without mutation replay, and remote session survival through
controller shutdown and SIGKILL. Workers are deterministic shell processes;
these checks do not spend provider credits or establish LLM task quality.

The pinned CI workflow runs browser, race, native transport, orchestration,
package and Linux desktop gates. It retains reports and failure evidence even
when a test fails. Windows mouse/focus and installed WebView2 behavior additionally
require the interactive Windows gate described in `desktop/README.md`. A release
is not validated by compilation alone; record the actual candidate and results
in `docs/VALIDATION.md` before distributing it.

Licensed under the repository's Apache-2.0 license.
