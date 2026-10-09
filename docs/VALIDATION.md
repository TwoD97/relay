# Validation

Relay has separate checks for the controller/runtime, browser client, SSH
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
