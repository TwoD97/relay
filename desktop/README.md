# Relay desktop

Relay is a desktop application with an embedded interface in a Tauri v2 window.
Windows x64 uses WebView2 and a native Windows controller and SSH
client; WSL is not required. Linux amd64 and arm64 use WebKitGTK. Linux packages
also work inside WSLg.

On Windows, install `relay-desktop-windows-amd64-setup.exe` and open **Relay**
from the Start menu. The installer operates for the current user, provides an
uninstaller, and installs WebView2 if it is missing.

The portable ZIP offers the same application features. Extract
`relay-desktop-windows-amd64-portable.zip` and open `Relay/relay-desktop.exe`.
Keep the whole folder together. No Relay installer, administrator access,
Microsoft account, or Relay account is required. WebView2 must already be
available; the ZIP's `README.txt` links to Microsoft's runtime download when it
is missing. Settings remain in the private Windows account directories, so the
ZIP is an installation-free application, not a roaming archive of credentials
or hosts. Coding harnesses
and terminal sessions run on the connected Linux hosts; local Windows PTYs are
not implemented. Windows SSH supports direct connections, aliases, private keys,
the Windows OpenSSH agent, passphrases, and passwords. ProxyJump and ProxyCommand
are not yet supported by the Windows transport.

The window starts or reconnects to a detached local Relay controller. Closing the
window or choosing **Quit Relay** leaves the controller, SSH connections, and
terminal sessions running. Reopening the app reconnects to the matching controller.
Starting the default desktop app again focuses its existing window.

## Windows updates and saved data

Close the existing Relay window before installing or opening a different build;
otherwise, single-instance activation focuses the already running copy. Closing
the window leaves its controller and remote sessions running. Install over the
existing installation, then open Relay from the Start menu. The desktop package
version is `0.2.0`; reinstalling that same version may offer **Add/Reinstall**.
The bundled runtime has its own build version and the launcher checks it before
reusing or replacing the local controller.

Installed and portable copies use the same Windows account data:

| Data | Default location |
| --- | --- |
| Saved hosts, Mastermind choice, controller state | `%LOCALAPPDATA%\RelayData\relay-client` |
| Appearance preference | `%APPDATA%\dev.relay.desktop\appearance.json` |
| Embedded WebView2 profile | `%LOCALAPPDATA%\dev.relay.desktop` |
| SSH configuration, keys, and trusted hosts | The user's `.ssh` directory, or configured SSH paths |

The installer writes application files under `%LOCALAPPDATA%\Relay`, separately
from this data. Its optional uninstall **Delete app data** checkbox removes the
native appearance configuration and WebView2 profile; it does not remove
`RelayData`, SSH files, or remote sessions. Normal reinstalls preserve these
settings. Neither packaging format restarts a live remote daemon automatically.
Mastermind can use a verified compatibility worker on an older runtime while
existing sessions keep running. Runtime-native features, including approval
hooks, still require a runtime that supports them; worker permission prompts
remain in the terminal.

## Appearance and window controls

The **Appearance** selector in the app's top bar defaults to **System** and also
offers **Light** and **Dark**. It updates the app, terminal ANSI palette, title
bar, menu, loading screen, and connection errors together. Preferences are saved
in the native app configuration directory, independent of the local controller
port. It does not change your operating system or provider settings, reconnect
terminals, or restart sessions. Explicit RGB colors printed by remote programs
remain unchanged.

The custom title bar replaces the native title and menu bars. Drag its center to
move the window, double-click to maximize or restore, and use the right-hand
buttons to minimize, maximize, or close. The **relay.** button opens the application
menu with Reload, Reconnect, and Quit. Keyboard shortcuts are Ctrl+R,
Ctrl+Shift+R, and Ctrl+Q. Reconnect renews the embedded client's authentication
after a controller restart. Relay has no browser-workspace menu or shortcut.
The embedded view retains its
standard text selection and clipboard shortcuts. In a terminal, Ctrl+V or
Ctrl+Shift+V pastes text from this computer's clipboard. Ctrl+C copies selected
terminal text, or sends an interrupt when nothing
is selected. Ctrl+Shift+C always means copy. The terminal's Copy and Paste
buttons provide the same actions. Ctrl+Alt+V sends a literal Ctrl+V to a remote
application that needs that key, including an agent's own image-paste command.

## Build

Install Node.js 22+, Go 1.26+, Rust via rustup, and the
[Tauri Linux system dependencies](https://v2.tauri.app/start/prerequisites/#linux).
On Debian/Ubuntu:

```sh
sudo apt install build-essential pkg-config libwebkit2gtk-4.1-dev libgtk-3-dev \
  libayatana-appindicator3-dev librsvg2-dev libssl-dev libxdo-dev curl wget file
```

Run `./scripts/build-desktop.sh` from the repository root to build the Go
runtime payload and native Debian package. The desktop Rust workspace pins its
toolchain and Cargo lockfile.

For a development run after staging the runtime resources:

```sh
cd desktop
npm ci
npm run dev
```

The resource folder `resources/runtime/` contains both Go binaries,
`relay-linux-amd64` and `relay-linux-arm64`, their SHA256SUMS and VERSION,
plus licensing and documentation. Tauri packages that folder at
`resource_dir/runtime/`. The desktop executes the binary for the current CPU using
an explicit path and forwards the resource directory as `--binaries` for SSH
deployment.

The Windows package additionally includes
`relay-controller-windows-amd64.exe` and `SHA256SUMS.windows`. Its native launcher
executes that PE directly with literal arguments and a hidden helper console.
The controller uses a current-user named pipe, Windows file locks, and private
ACLs. Persistent controllers stage verified immutable copies in their own data
directory so they do not lock the installer's resources during an upgrade.

To build the Windows package, prepare the matching Go payloads from the repository root:

```sh
./scripts/build.sh
./scripts/build-windows-controller.sh
python3 scripts/desktop_notices.py --target x86_64-pc-windows-msvc --output /path/to/windows-notices.txt
```

Then run `scripts/build-desktop-windows.ps1 -SourceRoot PATH_TO_REPOSITORY
-NoticesPath PATH_TO_WINDOWS_NOTICES` in Windows PowerShell. It stages sources on
a native Windows drive, checks payload hashes and versions, runs Rust tests and
Clippy, and produces `dist/relay-desktop-windows-amd64-setup.exe` with a SHA256
sidecar. Add `-Portable` to build only the ZIP and its checksum without invoking
NSIS. The mode still verifies all three runtime binaries and runs native checks.
The archive has `Relay/relay-desktop.exe`, adjacent `runtime/`, public and
third-party notices, launch instructions, and a checksum for every payload file.
Only the explicit resource allowlist is copied; build caches and account data
cannot be included accidentally.

After an installer build, create a ZIP from the same compiled stage without
recompilation:

```powershell
python scripts/package_desktop_windows.py --stage C:\path\to\relay-desktop-build --output dist/relay-desktop-windows-amd64-portable.zip --replace
```

Build prerequisites are Python 3, Rust 1.99.0, Node.js 22+, and the Visual Studio
C++ build tools/Windows SDK. The current installer is unsigned.

The build stage must be separate from the checkout and starts empty on first
use. Its `.relay-desktop-stage.json` marker binds it to that source checkout.
Subsequent builds replace source/resource trees and preserve Cargo's cache;
removed assets cannot leak into a later installer. An older unmarked stage is
left untouched: select a new empty `-StageRoot`. `-StageOnly` verifies and stages
inputs without compiling or starting anything.

Development and disposable test overrides:

| Environment variable | Meaning |
| --- | --- |
| `RELAY_DESKTOP_BUNDLE` | Absolute runtime bundle directory; overrides the packaged resources. |
| `RELAY_DESKTOP_STATE_DIR` | Private controller state directory passed to `relay desktop`; disables default single-instance activation for test isolation. |
| `RELAY_DESKTOP_RUNTIME_DIR` | Local daemon state directory passed to `relay desktop`. |
| `RELAY_DESKTOP_LOCAL=0` | Disable the local host. Default: enabled on Linux, disabled on Windows. |

These overrides do not change HOME or provider authentication. Parallel isolated
test runs should also use separate XDG data/cache/config directories for WebKit.
The wrapper never downloads or replaces tools itself.

The Windows resource directory resolves relative to the executable, so the
extracted folder works from another directory without installation or PATH
changes. The controller state stays under `%LOCALAPPDATA%\RelayData`, and the
embedded WebView2 profile stays under `%LOCALAPPDATA%\dev.relay.desktop`.
The GUI executable shows a native startup message if it cannot create its window,
including complete-folder and WebView2 guidance. It does not install prerequisites
or change security policy automatically. Unsigned builds remain subject to normal
Windows and organization policy.

## Launch and security boundary

The bundled controller's `desktop` command returns one JSON object:

```json
{"url":"http://127.0.0.1:PORT/auth?token=SECRET","address":"http://127.0.0.1:PORT","version":"VERSION","protocol":1,"pid":123}
```

The native process reads this response privately, limits it to 64 KiB, and imposes
a 30 second launch deadline. It validates the literal IPv4 loopback origin, matching
port, protocol, `/auth` path, and single token parameter before navigation. It does
not log or persist the response. The helper owns controller reuse and concurrent
launch handling; the native window never owns controller or agent lifetimes.

The controller webview permits only this controller origin and bundled
loading/error pages. The title bar and application menu are separate webviews
that load only immutable bundled assets. Their single `frame_action` command
accepts a fixed action enum and independently verifies the calling webview label
and exact local URL. Its explicit capability matches only `chrome` and
`app-menu`; the controller webview has no native capabilities and cannot
navigate to either privileged page. The frame assets prohibit embedded frames
and external scripts through CSP. Popup windows are denied everywhere, and no
global Tauri API is exposed. Native hidden menu entries retain keyboard
accelerators without drawing a system menu bar. The pinned Tauri `unstable`
feature supplies the separate child-webview API. The single-instance plugin
runs only in the native process. The HTTP controller
continues to enforce its existing cookie, CSRF, WebSocket, and origin checks.
Standard web clipboard access is enabled only for the controller content
webview. This does not grant Tauri commands or a native clipboard plugin to the
page. Paste requires a live terminal control lease, and a delayed clipboard
response is discarded if the terminal disconnects, loses control, or loses focus.

## Checks

Run Rust checks from `desktop/`:

```sh
cargo fmt --check
cargo test --locked
cargo clippy --locked --all-targets -- -D warnings
```

`scripts/test_desktop.py` (from the repository root) drives the real Linux WebKit webview with the Tauri
WebDriver and a disposable controller/runtime. It does not authenticate providers
or touch the user's existing sessions.

`test_windows_build_stage.ps1` verifies clean staging and failure preservation
without compilers. `test_windows_install.ps1` runs only in a fresh disposable
Windows account and checks the installed payloads and native shortcut. The full
interactive Windows gate is `test_windows_desktop.ps1`; it requires a matching
WebView2 driver and a disposable Linux SSH host. Hosted Windows CI does not claim
to cover native mouse/focus behavior; run that interactive gate before shipping.
Its `-LaunchOnly` mode needs no SSH fixture and checks an extracted app's default
resources, embedded fleet, private authentication, appearance synchronization
and persistence, privileged-command isolation, and ordinary native launch.
It uses isolated state and a separate WebView2 profile, disables single-instance
activation for that test, and does not send mouse/keyboard input or access the
clipboard. Cleanup is scoped to its own process identities.
Pass `-Clipboard` with a disposable Windows account clipboard to additionally
check native text copy/paste. This replaces clipboard contents with test text
and never captures the account's previous clipboard. The Linux gate always
checks copy/paste using its isolated Xvfb clipboard.
`python scripts/test_portable_package.py -v` checks archive integrity, required
resources, corruption, private-file exclusion, symbolic links, and Windows
junctions using disposable files only.

`test_windows_upgrade.ps1` also requires a fresh disposable Windows account. It
installs a previous package, seeds a fake saved host, Mastermind identity,
appearance preference, and WebView profile marker, then verifies those bytes
survive the candidate installer alongside the new payloads and Start menu
shortcut:

```powershell
./scripts/test_windows_upgrade.ps1 -Installer dist/relay-desktop-windows-amd64-setup.exe -Bundle dist -PreviousInstaller C:\fixtures\previous-setup.exe -PreviousBundle C:\fixtures\previous-runtime
```

Both installers need their `.sha256` sidecars. Without the previous-package
arguments, the test reports a same-build reinstall, not a version upgrade. It
refuses an existing installation or account data, never opens the app, and does
not claim to exercise live session preservation; that is covered separately by
the controller and native lifecycle gates. Do not run this installer test in a
personal Windows account or redirect account directories to bypass its guard.

Tauri API references used by this wrapper:
[window navigation](https://docs.rs/tauri/latest/tauri/webview/struct.WebviewWindowBuilder.html),
[native menus](https://v2.tauri.app/learn/window-menu/),
[resource paths](https://v2.tauri.app/develop/resources/), and
[capability boundaries](https://v2.tauri.app/security/capabilities/).
