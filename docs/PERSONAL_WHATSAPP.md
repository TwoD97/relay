# Personal WhatsApp linked device

Personal WhatsApp is an optional connector. Relay runs normally without it.
The official WhatsApp Business Cloud API is a separate integration; this
connector uses the unofficial [whatsmeow](https://github.com/tulir/whatsmeow)
linked-device client. WhatsApp protocol or account-policy changes may interrupt
it. No account is linked automatically.

In Mastermind's remote-control settings, choose Personal WhatsApp and enter
your international phone number. Relay returns a short-lived phone-link code.
On the phone, open WhatsApp's **Linked devices** flow and choose the option to
link using a phone number. Enter the code shown by Relay. A pending or existing
link must be explicitly disconnected before pairing another account.

Use your own **Message yourself** chat:

- `!relay status` reads Relay's current status.
- `!relay approvals` lists exact pending permission requests with ticket IDs.
- `!relay allow ID` approves the displayed request once, only while that exact
  request and session are still pending. An abbreviated request cannot be
  allowed from WhatsApp; open Relay to inspect it.
- `!relay deny ID` denies that exact request once.
- `!relay your instruction` adds an instruction to the Mastermind inbox.

Only messages sent by the linked account to its own chat are accepted. Group
messages, other chats, edits, history-sync events and Relay's replies are
ignored. Instructions are queued through the same control API as other
connectors, not injected as terminal keystrokes. Relay does not claim a task
has completed merely because a message was accepted.

The current bound is eight accepted commands per minute. Recent message IDs,
approval tickets and delivery receipts are stored privately to prevent replay.
Results are sent only to the account's own chat and never start with the command
prefix. A send or approval with an uncertain response is not automatically
repeated. Inspect its outcome in Relay before issuing a different instruction.

Linked-device credentials are kept in a private SQLite database below the
controller's `whatsapp-personal-private` directory. They are not part of peer
sync, workspace exports, terminal sessions, logs or package artifacts. The
phone number and pairing code are not saved in Relay's settings. Disconnect
first revokes command admission in Relay, then tries to unlink the device and
removes only its known private database files. If remote unlinking cannot be
confirmed, remove the device from WhatsApp on the phone as well.

The connector uses a separate executable. The core verifies the platform
executable against the exact three-entry `SHA256SUMS.whatsapp` manifest, copies
it into a private immutable release directory, then communicates over bounded
JSON-lines stdin/stdout pipes. It does not run a shell or expose a network
control endpoint. Each operation has a deadline; a malformed response, process
exit or timeout closes that helper without replaying an uncertain mutation.
The controller can later reconnect the saved linked device. Pairing still
requires explicit user action.

## Licensing and source

Relay's core remains Apache-2.0 and imports no whatsmeow, libsignal or SQLite
connector package. The optional executable is a separate Go module at
`connectors/whatsapp-personal`, distributed under **GPL-3.0-only**. Its
dependencies include whatsmeow under MPL-2.0 and libsignal under GPL-3.0.
This is an architectural separation, not a claim that dependency licenses
disappear when a component runs in a separate process.

Packages containing the optional executable also include:

- `WHATSAPP_PERSONAL_LICENSE.txt`, the complete GPL version 3 text;
- `WHATSAPP_PERSONAL_NOTICES.txt`, the incorporated dependencies' license texts;
- `relay-whatsapp-personal-source.tar.gz`, the connector and pinned vendored
  dependency source, notices and an offline build script for all three targets.

The complete corresponding source accompanies the binaries; obtaining it does
not require an account or a separate source request. To build the optional
component from the repository, run `RELAY_GO=/path/to/go1.27.1
./scripts/build-whatsapp-personal.sh dist` on one line. To rebuild the source
archive offline, unpack it and run its `build.sh` with Go 1.27.1. The main
module's tests cover the process boundary using a synthetic helper; the
connector module's tests cover private storage and message filtering without
pairing a real account.
