# Optional personal WhatsApp connector

This standalone executable is distributed under **GPL-3.0-only**. It uses
whatsmeow (MPL-2.0) and libsignal (GPL-3.0), with the other licenses reproduced
in the accompanying notices. Relay's Apache-2.0 core does not import or link
this module. This connector is optional; Relay works without it.

The executable owns one linked device's private SQLite database. Its JSON-lines
stdio protocol supports state, connect, phone-code pairing, self-chat sending,
logout and close. It never exposes contacts, history, account tokens or a Relay
terminal/controller interface. `--stdio --state-dir /absolute/private/path`
starts the interface; opening storage alone makes no network connection.
Protocol requests carry a monotonically increasing integer `id` and `method`.
Responses echo the ID; asynchronous `state` and `message` frames have no ID.
Frames are limited to 128 KiB and operations to 25 seconds. Diagnostic output
contains no provider responses or credentials. The parent must not replay an
operation after a lost response.

`relay-whatsapp-personal-source.tar.gz` contains this module's sources, pinned
vendored dependency sources, all dependency license notices, and `build.sh`.
Unpack it and run `GO=/path/to/go1.27.1 ./build.sh` to build all three distributed
targets offline. Go 1.27.1 is the general-purpose build tool, available from
<https://go.dev/dl/>; no WhatsApp account is needed to compile or run unit tests.
The repository packaging command is `./scripts/build-whatsapp-personal.sh dist`.
The vendored archive is produced separately, never copied from a personal
credential directory. The archive's `NOTICES.txt` and `dependency-licenses/`
retain the license texts for each incorporated module.

The connector is an unofficial linked-device integration. It is not endorsed
by WhatsApp/Meta; protocol changes or account policy may interrupt service.
Only incoming messages you send to your own **Message yourself** chat beginning
with `!relay ` are accepted. Other chats, groups, history sync, edits and Relay's
own replies are ignored. No actual account pairing is performed by tests.
