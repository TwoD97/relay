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

Your saved hosts, trusted SSH keys, and controller state stay in your Windows
account under %LOCALAPPDATA%\RelayData. The desktop keeps its private WebView2
profile under %LOCALAPPDATA%\dev.relay.desktop. These files are not included in
the ZIP. Moving the app folder does not move or erase them. Existing installed
and portable copies use the same account state; an already open Relay window
is focused when you launch another copy.

Closing Relay leaves its controller and remote sessions running. Reopen the app
to reconnect. The Relay menu provides Reload, Reconnect, and Quit.

To update, extract the new ZIP into a new folder. Close the old Relay window and
open the new copy when convenient; your remote sessions stay on their hosts.
Keep the full app folder while that copy is in use. Deleting the app folder does
not delete your saved account state or stop remote sessions.

Public license notices are included here and in runtime\. Third-party Rust
notices are in RUST_THIRD_PARTY_NOTICES.txt. SHA256SUMS lists every other file.
