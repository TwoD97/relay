# WhatsApp Business Cloud API

Relay can receive instructions and supported permission decisions through the
official WhatsApp Business Cloud API. This is separate from the optional personal
WhatsApp connector. It requires your own Meta application, WhatsApp Business
phone number, Cloud API credentials, and an HTTPS host. Relay does not create or
deploy those services for you.

The public webhook runs as `relay whatsapp-bridge` on Linux. The Windows or Linux
desktop controller polls it over HTTPS; the desktop API remains private. The
bridge holds a Meta app secret and a separate polling secret, but never the
Cloud API access token. It only validates and queues messages; it cannot execute
commands, access Relay sessions, or grant permissions.

## Host the webhook

Install the matching Relay Linux CLI on a host you administer. Create a private
directory and a mode `0600` JSON configuration file containing:

```json
{
  "appSecret": "YOUR_META_APP_SECRET",
  "verifyToken": "YOUR_RANDOM_WEBHOOK_VERIFICATION_SECRET",
  "pollToken": "YOUR_SEPARATE_64_LOWERCASE_HEXADECIMAL_SECRET",
  "phoneNumberId": "YOUR_BUSINESS_PHONE_NUMBER_ID",
  "owner": "YOUR_ALLOWED_WHATSAPP_SENDER_ID"
}
```

Generate independent verification and polling secrets, for example with
`openssl rand -hex 32` for each. The verification secret is supplied to Meta
when registering the webhook. The polling secret is supplied only to the Relay
desktop. Never put either secret or the Meta app secret in a command line,
source repository, screenshot, or reverse-proxy access log.

Run the bridge as an unprivileged account with a private persistent state path:

```sh
relay whatsapp-bridge \
  --config /private/relay-whatsapp/bridge.json \
  --state-dir /private/relay-whatsapp/state \
  --listen 127.0.0.1:8787
```

Place a maintained HTTPS reverse proxy in front of that loopback listener.
Forward `/webhook` and `/api/` without rewriting request bodies. Limit request
bodies to 1 MiB and avoid logging query strings on `/webhook`: Meta verification
includes the verification secret in the query. For direct TLS hosting, pass
`--tls-cert` and `--tls-key`; unencrypted public binds are rejected. Supervise the
process with your host's service manager. The bridge refuses a second process
using the same state directory.

Configure Meta's callback URL as `https://YOUR_HOST/webhook`, provide the
verification secret, and subscribe the application/Business account to the
`messages` webhook field. The app secret validates `X-Hub-Signature-256` over the
exact POST body. Set `phoneNumberId` to the Graph API phone-number resource ID,
not the displayed phone number. Set `owner` to the exact sender ID Meta includes
in messages from the person who may control Relay. Only that sender's text
messages for that Business number are accepted.

## Connect the desktop

In Mastermind's remote connections, choose WhatsApp Business and enter:

- HTTPS bridge base URL, such as `https://YOUR_HOST`.
- Bridge polling secret.
- Cloud API access token with the permissions required for your Business phone
  number. Use an appropriately managed token; temporary test tokens expire.
- The same phone-number resource ID and allowed sender ID as the bridge.
- The Graph API version configured for your Meta application, such as the
  version shown in its setup screen. Relay does not silently select a version.

Setup verifies the bridge's phone/sender binding and the token's phone-number
resource before saving. Secrets stay in private local controller state and are
not returned by status APIs or included in peer synchronization. HTTP redirects
are refused. Changing the connection creates a new account epoch; results from
the previous connection are not delivered to the new one.

Send a **new** text message from the allowed sender after setup. Ordinary text
becomes a durable Mastermind instruction, not terminal keystrokes. Supported
commands are:

| Message | Result |
| --- | --- |
| `!status` | Current coordinator and inbox status |
| `!approvals` | Current supported, identified permission requests |
| `!allow ID` | Allow one still-pending request when its complete details fit |
| `!deny ID` | Deny one still-pending request |

Only exact, fresh permission identities can be answered. Truncated requests
cannot be allowed remotely. Terminal-only prompts must be reviewed in Relay.
No terminal text is treated as an approval request, and sending an instruction
does not itself grant additional provider permissions.

## Delivery and recovery

Inbound messages expire after 10 minutes. Messages from before the desktop
connection was configured, unknown senders, other Business phone numbers,
attachments, and future-dated events are ignored. The bridge stores at most 256
queued messages and 2,048 freshness-window receipts, within a 3 MiB file. It
acknowledges a webhook only after a durable queue write. Queue storage failures
fail closed; preserve the state before recovery.

The desktop polls every five seconds while its controller is running. It saves
an instruction before acknowledging it to the bridge. Duplicate deliveries
retain the same instruction identity. A controller disconnect does not replay
work; messages that expire while it is offline are not subsequently executed.

Free-form Cloud API replies are sent only within the 24-hour customer-service
window opened by the allowed sender's message. Relay does not send templates to
reopen that window. Send a new message when it expires. Meta pricing, account
eligibility, app configuration, and messaging policies still apply.

Outgoing intent is stored before a send. If an acknowledgment is lost or Meta
rejects a send, Relay does not repeat it automatically. Review the result in
Relay. Rate-limit responses pause requests for at least one minute; they do not
cause an unknown outgoing action to be replayed. Permission decisions likewise
consume their exact ticket before submission.

Disconnecting the desktop removes its configured credentials and retains the
existing instruction history. It does not stop your Linux bridge or remove the
Meta webhook subscription; manage those separately when retiring the service.
Testing covers synthetic signed webhooks and local fake Graph endpoints. No
live Meta account, public deployment, paid messaging, or credentials were used
to validate this feature.

## Protocol references

- [Meta webhook overview](https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/overview)
- [Meta webhook verification and signature documentation](https://whatsapp.github.io/WhatsApp-Nodejs-SDK/api-reference/webhooks/start/)
- [Meta text-message request documentation](https://whatsapp.github.io/WhatsApp-Nodejs-SDK/api-reference/messages/text/)
- [Meta quickstart and 24-hour reply window](https://whatsapp.github.io/WhatsApp-Nodejs-SDK/)

The archived SDK documentation above is used only as a protocol reference. Relay
uses its own bounded HTTP implementation and does not bundle that SDK or copy
Meta sample source code.
