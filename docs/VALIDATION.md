# Validation

Relay has separate checks for the controller/runtime, embedded interface, SSH
orchestration, and native desktop shell. Use disposable state and hosts for
integration tests. Generated reports and machine-local deployment evidence are
excluded from the repository.

## Reproduce the checks

```sh
./scripts/check.sh
./scripts/build.sh
./scripts/build-windows-controller.sh
go test -race -tags relay_ssh_native ./internal/transport -count=1 -timeout 1m
RELAY_SSH_INTEGRATION=1 RELAY_SSH_HARNESSES=1 go test -race ./internal/transport -run TestRealSSH -count=1 -v -timeout 10m
CI=1 RELAY_TEST_RUN=live RELAY_LIVE_BINARY="$PWD/dist/relay-linux-amd64" npm --prefix web test -- e2e/live.spec.ts
```

The SSH gate uses Docker containers and can download the current official
provider binaries. It does not sign into provider accounts or run paid model
requests. Multi-host orchestration uses deterministic shell workers to test host
isolation, terminal control, recovery, and preservation across disconnects.

[Desktop validation](../desktop/README.md) covers native Rust checks, Linux
WebKit integration, Windows installation/staging, and interactive WebView2
behavior. Hosted Windows CI does not replace an interactive window/focus test.

## Desktop product and Fleet coverage

The embedded-interface suite uses desktop and narrow-window viewports. Fleet
cases cover three connected machines without sessions, direct terminal
navigation, exact approval/session identity, reported turn completion, unavailable
and cached state, opt-in summary settings, summary failures and usage limits.
Steps and next actions are labeled as model snapshots, not provider-native tasks.
The separate live suite drives the compiled controller and real PTYs through
the same private bootstrap used by the desktop shell.

CLI regressions check that the default command and retired public `ui` command
do not start a browser workspace or print authentication links. Native menu
checks retain Reload, Reconnect, and Quit without an Open in Browser action.

Portable packaging tests verify the complete resource allowlist, checksums,
Windows GUI executable, license notices, and exclusion of account state. They
reject incomplete or modified bundles, symlinks, and Windows junctions. The
Windows `-LaunchOnly` smoke test opens an extracted ZIP using its default resource
paths and isolated state/profile, then checks ordinary native startup without
WSL, console windows, or debug flags. It does not replace the full interactive
terminal, window focus, or optional Windows clipboard gate.

## Activity and clipboard coverage

The approval gate covers competing decisions, exact session identity, request
expiry, hook disconnect, host isolation, controller authentication/CSRF checks,
hook-token stripping, payload bounds, and absence of persisted tool arguments.
Provider hook definitions are also parsed by isolated installed CLI builds
without account login or inference. Real provider execution remains a separate
manual check; CLI compatibility is reported conservatively at session creation.

Observer tests use a deterministic executable, never a provider account. They
cover opt-in, capability validation, fixed tool-disabled arguments, environment
isolation, structured output, process cancellation, model usage limits,
persistence failure, crash recovery without replay, exact session identity,
deleted sessions, and bounded visible terminal context. The dashboard tests
cover offline/unsupported runtimes, stale summaries, explicit configuration,
and unknown mutation outcomes. These checks do not claim to validate model
quality or the availability of a model in a particular subscription.

Clipboard regressions exercise ordinary Ctrl+V text paste, selected-text Ctrl+C,
unselected Ctrl+C interrupts, multiline/bracketed paste, browser clipboard
round trips, and rejection of delayed paste after focus/control changes. The
Linux native gate uses a private Xvfb clipboard. Windows native clipboard checks
require the optional `-Clipboard` gate documented in the desktop README; ordinary
Windows installer checks do not exercise the user's clipboard.

## Fleet names and launch modes — 2026-10-12

Runtime bundle: `0.1.0-dev.50e9fc609217`.

Completed local checks:

- The combined interface suite passed all 230 cases: 222 fixture cases and
  eight real-binary cases, across desktop and narrow-window viewports, with no
  failures or skips.
- Full Go race tests and `go vet ./...` passed across all six tested packages.
  Native SSH transport race tests also passed.
- New mode regressions verify exact provider arguments, literal autonomy text,
  recorded mode and environment, Manual defaults, cancellation before launch,
  unsupported CLI rejection, and visible provider policy failures without retry
  or downgrade. Installed Claude Code 2.1.209 and Codex 0.153.4 accepted the
  required arguments in help-only checks; no model request was made.
- The real-binary interface mode scenario passed on desktop and narrow-window
  viewports. Each launched isolated Claude and Codex fixture processes in both
  Manual and Auto, checked captured arguments and the mode badge after reload,
  exercised real PTY input, and verified exactly one launch per session.
- Disposable two-host SSH orchestration passed named-host session start, list,
  read, input and stop; independent SSH trust, exclusive writer ownership, no
  replay after uncertain responses, and live PTY preservation through controller
  disconnect, restart and forced failure. Revision, expiry, ambiguity, removal,
  stale alias invalidation and source socket selection have separate regressions.
- The full SSH gate passed background maintenance/recovery and clean managed
  Codex 0.162.1 and Claude Code 2.1.296 installation without account login or
  inference. Its two-host binaries preceded the final duplicate-name rejection
  fix; the final fleet registry race suite verifies that correction separately.
- Native Windows checks passed: CLI 17, controller 35, SSH transport four, and
  fleet registry three. The transport cases include isolated SSH alias/user/port
  resolution. One optional migration and three Linux-only tests were skipped;
  privilege-specific subtests retain their own platform requirements.
- Native Rust tests passed on Linux (12) and Windows (11), with formatting and
  strict Clippy checks. Linux WebKit passed real terminal editing, isolated
  clipboard, exclusive viewer control, and close/reopen session preservation.
- The extracted Windows portable payload passed the final isolated
  `-LaunchOnly` check in 6.09 seconds, including default resources, private
  authentication, the desktop-only menu and ordinary native startup without
  WSL, debug switches or a visible console. An initial native-window identity
  timeout remains unexplained; a diagnostic run and the final run both passed
  on the same payload. Failure-only process/window diagnostics now preserve
  evidence for recurrence. The full interactive Windows mouse/clipboard gate
  was not rerun for this change.
- Installer regressions passed 10 cases; portable archive regressions passed
  eight cases on Linux, with the Windows junction case skipped on that platform.

These tests use disposable state, SSH hosts and provider stubs. They do not
measure model understanding or paid inference. Auto requires a compatible
running runtime; updating a controller or managed binary does not interrupt
existing remote sessions to activate a new runtime.

## Previous desktop release validation — 2026-10-12

Runtime bundle: `0.1.0-dev.817b7ea0ab5c`.

Completed local checks:

- The combined embedded-interface run passed all 210 cases: 204 fixture cases
  and six cases against the compiled controller/runtime, across desktop and
  narrow-window viewports. There were no failures or skips in that final run.
- Full Go race tests and `go vet ./...` passed. Installer and portable archive
  regressions passed on their supported platforms; privilege-specific symlink
  cases were skipped where unavailable.
- Native Rust unit tests passed on Linux (12) and Windows (11), with formatting
  and strict Clippy checks.
- Native Linux WebKit passed fleet/menu rendering, folder selection, real PTY
  editing, isolated text clipboard, competing viewer control, and window
  close/reopen without replacing the controller or terminating its session.
- The extracted Windows portable package passed `-LaunchOnly` using its default
  resources and an isolated profile. WebView2 rendered Fleet and the desktop-only
  menu, private authentication succeeded, and normal startup used no WSL,
  debugging switches, or console window. The full interactive Windows terminal,
  mouse/focus, and clipboard gate was not rerun for this release.
- Disposable two-host SSH orchestration passed cross-host session list, start,
  read, input, and stop from a source PTY. It checked exact host/session identity,
  competing writers, lost mutation responses without replay, and preservation
  of both daemon identities and live shell state across disconnects, controller
  restart, and forced controller failure.
- Runtime repair preserved the live daemon. Background maintenance survived a
  controller restart and removed only its successful owned worker, preserving
  failed output and unrelated sessions. Clean Codex 0.162.1 and Claude Code
  2.1.296 installations passed version checks without account login or inference.
- Native SSH race tests and both native Windows SSH protocol fixtures passed.
  The targeted native Windows CLI/controller lifecycle checks passed (7).

These runs used disposable sessions and test profiles. They did not exercise
paid provider inference or validate model-summary quality. Hosted CI results
are separate from these local checks.

## Standalone namespace validation — 2026-10-09

The source uses `github.com/TwoD97/relay`, `cmd/relay`, Relay artifact and storage
names, and `RELAY_*` environment variables. Fresh installs use Relay directories.
There is no automatic state import from development builds that used different
storage directories.

Completed checks:

- Full Go race tests and `go vet ./...` passed across all five tested packages.
- Native Windows: CLI 13 passed, controller 31 passed, native SSH transport two
  passed. Linux-only cases, an optional prior-release fixture, and a Windows
  symlink privilege case were skipped where appropriate.
- Go module verification and tidy consistency passed.
- Linux installer regressions: 10 passed.
- The bundled Relay coordination skill passed its structural validator.
- Browser regressions: 146 passed, with four live-only cases skipped. The
  separate real-binary browser run passed all four live cases.
- Disposable real-SSH and multi-host orchestration passed, including controller
  restart recovery, writer leases, background maintenance cleanup, and provider
  binary installation without provider login or inference.

Windows state creation also has native regression coverage for explicit user
ownership, protected permissions, staged payloads, and concurrent launchers.
Adversarial tests that create Administrator-owned state require an elevated
Windows token and are skipped on a standard-user test run.

Current hosted results are available in
[GitHub Actions](https://github.com/TwoD97/relay/actions). Local results above do
not imply that every hosted desktop test has passed.

The namespace migration was exercised in isolated state and containers. It did
not alter any existing personal application installation, saved host profile,
provider authentication, or live remote daemon.
