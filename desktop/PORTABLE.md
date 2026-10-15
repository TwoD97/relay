Relay for Windows — portable edition

1. Extract the entire ZIP into a folder on your computer.
2. Open Relay\relay-desktop.exe. Keep its runtime folder beside it.

No Relay installation, administrator access, Microsoft account, or Relay account
is needed. Add a Linux host and authenticate over SSH when you are ready. Claude
Code and Codex use their own sign-in on that host.

Microsoft Edge WebView2 Runtime must already be available. Windows 11 includes
it, and most Windows 10 computers have it. If the app cannot open its window,
install Microsoft's Evergreen WebView2 Runtime for your current user:
https://developer.microsoft.com/microsoft-edge/webview2/
Runtime details: https://learn.microsoft.com/microsoft-edge/webview2/concepts/distribution

This ZIP does not install WebView2 or change Windows security settings. Relay's
current builds are unsigned; Windows or your organization's policy may still
ask whether to run a downloaded app.

Your saved hosts, Mastermind choice, and controller state stay under
%LOCALAPPDATA%\RelayData\relay-client. The appearance preference is saved at
%APPDATA%\dev.relay.desktop\appearance.json, and the private WebView2 profile
stays under %LOCALAPPDATA%\dev.relay.desktop. SSH configuration, keys, and trusted
hosts use your normal .ssh directory or configured SSH paths. These files are
not included in the ZIP. Moving the app folder does not move or erase them.
Installed and portable copies offer the same application features and use the
same account state; an already open Relay window is focused when you launch
another copy.

Closing Relay leaves its controller and remote sessions running. Reopen the app
to reconnect. The Relay menu provides Reload, Reconnect, and Quit.

To update, extract the new ZIP into a new folder. Close the old Relay window and
open the new copy when convenient; your remote sessions stay on their hosts.
Keep the full app folder while that copy is in use. Deleting the app folder does
not delete your saved account state or stop remote sessions.

For Start menu integration, prerequisite installation, and an uninstaller, use
relay-desktop-windows-amd64-setup.exe instead. Switching packaging does not update
a live remote daemon; features that need a newer runtime remain unavailable
until that runtime is upgraded separately.

Public license notices are included here and in runtime\. Third-party Rust
notices are in RUST_THIRD_PARTY_NOTICES.txt. SHA256SUMS lists every other file.
