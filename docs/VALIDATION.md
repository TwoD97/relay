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
./scripts/build-legacy-test-runtime.sh
go test -race -tags relay_ssh_native ./internal/transport -count=1 -timeout 1m
RELAY_SSH_INTEGRATION=1 RELAY_SSH_HARNESSES=1 go test -race ./internal/transport -run TestRealSSH -count=1 -v -timeout 10m
CI=1 RELAY_TEST_RUN=live RELAY_LIVE_BINARY="$PWD/dist/relay-linux-amd64" RELAY_LEGACY_BINARY="$PWD/dist/relay-legacy-linux-amd64" npm --prefix web test -- e2e/live.spec.ts e2e/mastermind-live.spec.ts
```

The SSH gate uses Docker containers and can download the current official
provider binaries. It does not sign into provider accounts or run paid model
requests. Multi-host orchestration uses deterministic shell workers to test host
isolation, terminal control, recovery, and preservation across disconnects.

The legacy-fixture builder requires Go 1.27.1 and public commit
`fd7999b3bbcf20c8d852bf7461a58e211d04a376` in the local Git object database. For a
shallow checkout, fetch that exact commit with
`git fetch --no-tags --depth=1 origin fd7999b3bbcf20c8d852bf7461a58e211d04a376`.
It builds in a private temporary directory and requires the original shipped
SHA-256 `f3483628e68655cfae23beaf8f0cfd752ee11035708c8564563d1bf85bbbf31f`.
CI runs this gate explicitly; without the binary variables, live cases are
reported as skipped, not passed.

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
Windows `-LaunchOnly` smoke test opens extracted application files from the ZIP
or NSIS installer using default resource paths and isolated state/profile, then checks ordinary native startup without
WSL, console windows, or debug flags. It does not replace the full interactive
terminal, window focus, or optional Windows clipboard gate.

## Appearance coverage

Appearance cases exercise live system changes, persistent manual choices,
unavailable storage, native save rejection, delayed replies after newer native
events, and the reconnect screen. Terminal tests retain the same renderer,
writer lease, selection, output, and input while changing both ANSI palettes;
explicit RGB output remains unchanged.

Native Linux and Windows checks use the actual appearance command from the
embedded app and verify matching content, title-bar, and menu themes. They check
saved preferences across app close/reopen while the controller stays alive,
reject invalid values, and retain the restriction against invoking native frame
actions from controller content. Test state is isolated; no operating-system
theme preference is changed.

## Mastermind coverage

Controller regressions cover a single durable startup, configuration revisions,
parallel saves, delayed responses after reconfiguration, lost creation replies,
exact host/session ownership, disabled recovery, explicit restart after exit,
older runtime compatibility, missing sign-in, persistence failure, and corrupt optional
state without losing the rest of the fleet. Remote HTTP fixtures exercise
platform-independent recovery; Linux-only creation fixtures use real temporary
directories rather than pretending Windows has a local Linux runtime.

Runtime cases verify literal coordinator instructions and Manual/Auto arguments
for both harnesses, durable launch identities, unsupported CLI rejection, and
concurrent duplicate-token rejection. Opt-in installed-CLI checks use `--help`
only. Interface fixtures cover setup, stale edits, unknown outcomes, disabled or
unverified terminal attachment, and both appearance palettes.

`web/e2e/mastermind-live.spec.ts` uses disposable controllers, runtime directories,
and deterministic provider executables. It checks a real coordinator PTY,
reload/second-viewer behavior, controller restart, and a provider switch that
preserves the previous process. No provider login or inference occurs, so these
checks validate orchestration mechanics rather than model planning quality.

The compatibility cases additionally use a checksum-verified previous runtime
(`0.1.0-dev.50e9fc609217`). They exercise Claude Code and Codex in both modes,
unsupported-flag preflight, explicit setup recovery, and exact worker attachment
after a controller restart. The old daemon's process and an ordinary terminal
remain alive throughout, retaining their original identity, history, and input.
Worker regressions cover lost create/input replies, storage failures before and
after dispatch, every incomplete startup phase, duplicate claims, invalid
receipts, and stale configuration. These tests never use personal host state.

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

## Terminal geometry and continuation — 2026-10-28

Desktop version: `0.2.1`. Runtime bundle: `0.2.1-dev.889de5b3e6f3`.

- All 316 distinct interface checks passed without skips: 149 desktop fixtures,
  149 narrow-window fixtures, and 18 cases against the compiled runtime. The
  real-binary cases include six ASCII/wide/graphics right-edge continuations,
  controller recovery, exclusive control, permission decisions, Manual/Auto
  launches, and Mastermind compatibility with the checksum-verified old runtime.
- Baseline regressions reproduced clipped controlling grids and unreachable
  observer edges in both viewport projects. Real PTY tests against the previous
  bundle separately reproduced lost delayed wrap, wide-glyph continuation, and
  DEC line-drawing character-set state when a second xterm viewer attached.
- The corrected client sizes an unpadded mount, waits for canonical resize
  acknowledgements, and preserves ordered terminal bytes and geometry. Focused
  coverage includes deferred/rejected resizes, two viewers, hidden/revealed
  terminals, Unicode, scrollback, font preference bounds/persistence, and
  5000×4200 viewports within the runtime's 500-column/300-row limits.
- Go formatting, vet, and all 242 top-level race tests passed. Seven opt-in
  cases were skipped by the default suite. The 15 native SSH cases and both
  disposable SSH integration suites passed separately, including two-host
  control, controller crash/restart, session preservation, maintenance, and
  host-key rejection. Clean provider installs verified Codex 0.165.0 and Claude
  Code 2.1.312 without login or inference.
- An initial Go run overlapped replacement of the generated embedded frontend
  assets and failed to compile those deleted asset names. The complete race
  suite was rerun successfully after freezing the build inputs.
- The first real-xterm pass caught a character-set restore ordering regression:
  xterm treats the legacy GR selectors as GL aliases. Restoring GL last fixed
  initial and continued line drawing; a direct public-xterm-API regression also
  reproduces the former ordering failure. Final runtime payloads were rebuilt
  with the already validated, unchanged frontend assets.
- Linux installation regressions passed all ten cases. Portable archive checks
  passed eight cases with the Windows-only junction case skipped; the delivered
  Windows artifact remains an installer.
- Native Rust formatting, strict Clippy, and all tests passed on Linux (16)
  and Windows (15). The final-bundle Linux WebKit gate passed real PTY editing,
  exclusive control, isolated clipboard, appearance/IPC, authentication, and
  close/reopen preservation. At three odd window sizes, the visible grid and
  remote `stty size` agreed at 89×21, 110×28, and 69×16. The fit checks account
  for xterm's actual reserved scrollbar width while retaining strict edge bounds.
- The final Windows NSIS payload passed exact allowlist verification: 17
  application files, six installer plugins, GUI product version 0.2.1, and
  matching hashes for all three runtime binaries. Its extracted application
  passed isolated `-LaunchOnly` using default packaged resources, including
  exact native window geometry, appearance, authentication, and startup without
  WSL or a console window. No personal Windows installation was modified.
- The extracted Windows package passed `-RenderingOnly` against a disposable
  SSH host: verified host trust, password login/bootstrap, seven window/font
  states with exact PTY/visible-dimension agreement, exclusive control, saved
  host reconnect, and close/reopen preservation. The test used the monitor's
  existing 96 DPI, with no physical mouse input or Windows clipboard access.
  Preliminary attempts correctly rejected a mismatched fixture fingerprint and
  exposed a PowerShell 5 test-string encoding error; both test setup issues were
  corrected before the passing run, without changing the verified app payload.

The Windows installer is 12,100,379 bytes, SHA-256
`f4effca8618125b6a0fb15bbb148f2b213e90c678eff5942cfcfe5d25dcfd18d`.

Fractional scaling coverage uses deterministic native geometry tests and
Chromium device-scale emulation; it does not claim physical movement between
monitors at every Windows DPI. Tests use disposable sessions and state. Existing
remote daemons are not restarted to activate these fixes: client rendering
improvements apply immediately, while server snapshot corrections require the
updated runtime to start after its existing sessions have finished.

Snapshot coverage is targeted, not complete xterm emulation. The existing server
model clips its normal screen on resize while xterm can reflow long shell lines;
after resizing and reconnecting, those lines may need an application redraw.
Arbitrary terminal extensions and width-changing graphemes split across writes
are also outside the claimed continuation coverage. Raw history remains bounded
separately at 1 MiB.

## Live-runtime compatibility and Windows installer — 2026-10-15

Desktop version: `0.2.0`. Runtime bundle: `0.2.0-dev.52b7d81b0fc7`.

- All 284 distinct interface cases passed across project and targeted runs:
  272 fixture cases and 12 real-binary cases, with no remaining failures or
  skips. The combined run received SIGTERM after 218 passing cases; all 142
  desktop cases had completed. A separate phone run exposed an authentication
  redirect race in its test navigation. After adding an explicit authenticated
  view readiness check, all eight affected live tests passed in 34.1 seconds.
  Product code and packaged binaries did not change during that correction.
- The actual previous-runtime cases passed on the final bundle in both desktop
  (28.3 seconds) and narrow-window (24.2 seconds) views. All four provider/mode
  combinations attached through verified receipts while the old daemon and an
  ordinary terminal preserved their identities, history, and input. Controller
  restart reused the same worker without launching another provider.
- Full Go race tests passed 236 top-level cases; seven opt-in cases were skipped
  in that default run. Go vet and formatting passed. Fifteen native SSH cases
  and both disposable SSH integration gates passed separately, including
  two-host control and preservation through controller crash/restart. Clean
  harness installs verified Codex 0.162.1 and Claude Code 2.1.300 without login
  or inference.
- Native Rust formatting, Clippy, and tests passed on Linux (15) and Windows
  (14). Seven Windows controller Mastermind cases passed; 13 Linux-only
  creation cases were intentionally skipped on Windows.
- The NSIS installer was built and extracted. Its exact 17 application files
  and six installer plugins passed allowlist verification; all three runtime
  binaries, version, documentation, and notices matched the prepared bundle.
  The native executable is an AMD64 GUI application with product version 0.2.0.
- The extracted installer payload passed isolated Windows `-LaunchOnly` in
  11.03 seconds using its default packaged resources: native window, WebView2,
  controller authentication, rejected token replay, synchronized app/frame/menu
  appearance, saved Light preference after close/reopen, rejected invalid or
  privileged IPC, and normal startup without WSL, debug flags, or console
  windows. This gate did not exercise physical keyboard or clipboard input.
- Linux installer regressions passed all ten cases; portable archive regressions
  passed eight cases with the Windows-only junction case skipped. The new
  guarded Windows state-preservation script passed PowerShell parsing and is
  wired into fresh-account CI as a same-build reinstall check.

Installer SHA-256:
`d6d8c98a60b3caff378d6dc32fcdf77579ebdf3d05a9f69e612aed8a94dd8e8c`.
The installer remains unsigned. Installation and upgrade of a personal Windows
account were not exercised locally; the fresh-account installer/reinstall CI
gate and optional prior-version upgrade gate are distinct from extracted-payload
launch checks. No personal host configuration, provider account, or live remote
session was used in these tests.

## Appearance and Mastermind — 2026-10-15

Runtime bundle: `0.1.0-dev.1be423ff9815`.

Completed local checks:

- The final combined interface suite passed all 272 cases in 7.5 minutes:
  262 fixture cases and ten real-binary cases across desktop and narrow-window
  viewports, with no failures or skips. Appearance changes preserve terminal
  output, selection, focus and writer ownership without reconnecting.
- Full Go race tests passed 224 top-level cases across six packages; `go vet`
  and Go formatting checks passed. Seven opt-in cases were skipped in this
  default run; installed harness arguments and real SSH were checked separately.
- Mastermind controller cases cover persistent ownership and configuration,
  concurrent saves, delayed or lost replies, explicit restart after exit,
  old-runtime capability checks, and read-only recovery without duplicate
  agents. Native Windows passed five Mastermind and six authentication/security
  cases; ten Linux-only creation cases were intentionally skipped there.
- Both real-binary Mastermind interface cases passed. They used Claude and Codex
  fixture executables, actual PTY input, scoped `RELAY_BIN`/`RELAY_SOCKET` fleet
  discovery, reload and a second viewer, controller restart, and configuration
  switching while retaining the previous running session. No provider account
  or model inference was used.
- Installed Claude Code and Codex accepted the Mastermind and Manual/Auto
  arguments in help-only checks. This verifies CLI compatibility, not the
  coordinator's model planning quality or subscription access.
- Native SSH transport passed 15 race-test cases. Both disposable SSH gates
  passed, including two-host terminal control, trust boundaries, exclusive
  writers, controller crash/restart recovery, background maintenance, and
  preservation of remote process identities and shell state. Clean managed
  installs verified Codex 0.162.1 and Claude Code 2.1.300 without sign-in.
- Native Rust formatting, strict Clippy, and tests passed on Linux (15) and
  Windows (14). Linux WebKit passed appearance synchronization, connection
  recovery, terminal keys, isolated clipboard, writer ownership, and app
  close/reopen without replacing the runtime or session.
- The final extracted Windows portable payload passed the isolated
  `-LaunchOnly` gate in 11.72 seconds: System/Light/Dark across the embedded
  application, title bar and menu; saved appearance after reopening; rejected
  invalid or privileged IPC; and ordinary native startup without WSL, console
  windows or debugging flags. Physical Windows keyboard/clipboard interaction
  was not repeated in this gate.
- Installer tests passed all ten cases. Portable packaging passed eight cases
  on Linux with its Windows-only junction case skipped. The actual Windows ZIP
  was extracted, checked against its complete 22-file allowlist and manifest,
  and launched using its default packaged resources.

That release did not support starting Mastermind on an older running runtime
without `mastermindV1`. The compatibility-worker checks above cover the fix;
existing remote runtimes are still never restarted by a desktop update.

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
