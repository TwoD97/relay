# Relay desktop

The native application opens the same Relay interface as the browser client in a
Tauri v2 window. Windows x64 uses WebView2 and a native Windows controller and SSH
client; WSL is not required. Linux amd64 and arm64 use WebKitGTK. Linux packages
also work inside WSLg.

Install `relay-desktop-windows-amd64-setup.exe` on Windows and open **Relay** from
the Start menu. The installer operates for the current user and provides an
uninstaller. It installs WebView2 if that runtime is missing. Coding harnesses
and terminal sessions run on the connected Linux hosts; local Windows PTYs are
not implemented. Windows SSH supports direct connections, aliases, private keys,
the Windows OpenSSH agent, passphrases, and passwords. ProxyJump and ProxyCommand
are not yet supported by the Windows transport.

The window starts or reconnects to a detached local Relay controller. Closing the
window or choosing **Quit Relay** leaves the controller, SSH connections, and
terminal sessions running. Reopening the app reconnects to the matching controller.
Starting the default desktop app again focuses its existing window.

The dark title bar replaces the native title and menu bars. Drag its center to
move the window, double-click to maximize or restore, and use the right-hand
buttons to minimize, maximize, or close. The **relay.** button opens the application
menu with Reload, Reconnect, Open in Browser, and Quit. Keyboard shortcuts remain
Ctrl+R, Ctrl+Shift+R, Ctrl+Shift+B, and Ctrl+Q. Open in
Browser obtains a fresh one-time sign-in link for the default browser. Reconnect
also renews authentication after a controller restart. The webview retains its
standard text selection and clipboard shortcuts. In a terminal, Ctrl+V or
Ctrl+Shift+V pastes text from this computer's clipboard; Cmd+V works in a Mac
browser. Ctrl+C copies selected terminal text, or sends an interrupt when nothing
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
sidecar. Build prerequisites are Rust 1.99.0, Node.js 22+, and the Visual Studio
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
Pass `-Clipboard` with a disposable Windows account clipboard to additionally
check native text copy/paste. This replaces clipboard contents with test text
and never captures the account's previous clipboard. The Linux gate always
checks copy/paste using its isolated Xvfb clipboard.

Tauri API references used by this wrapper:
[window navigation](https://docs.rs/tauri/latest/tauri/webview/struct.WebviewWindowBuilder.html),
[native menus](https://v2.tauri.app/learn/window-menu/),
[resource paths](https://v2.tauri.app/develop/resources/), and
[capability boundaries](https://v2.tauri.app/security/capabilities/).
