# Mastermind control

Open **Mastermind → Overview → Mastermind control** for the instruction inbox,
Telegram, Discord, and WhatsApp connections, recovery settings, and verified lessons. Instructions are saved
on the current controller machine before being offered to the coordinator. They are not pasted
blindly into an interactive terminal.

New Mastermind launches read the bundled skill. It instructs the agent to read
`control show`, claim a queued message, carry out its work, update tasks/pages,
and publish a result with `control reply`. An idle coordinator can use
`control wait --seconds 300`; waiting is a local long poll, without model calls.
The **Listening** indicator means a recent inbox poll was observed. It does not
prove that a model is currently doing work. Existing running coordinators retain
their original launch instructions/executable: ask them to read the updated
skill, or start a new coordinator after finishing their current work.

## Telegram

1. Create a bot with Telegram's BotFather. Enter its token in Relay's Telegram
   panel, on the computer that will keep the control plane running.
2. Open a private chat with that bot and send the one-time pairing command
   displayed in Relay. It expires after ten minutes.
3. Send plain text to queue an instruction. `/status` reports the inbox state;
   `/approvals` shows current provider-hook permission requests.

Only the paired numeric account and private chat can control this Relay
installation. Groups, other senders, and expired pairing codes are ignored.
Disconnect removes the bot token from Relay's saved settings. Keep Relay
running for polling and delivery; there is no public webhook or incoming port.
One bot should be used by one active Relay control plane.

Instructions, results, and approval details travel through Telegram. The token
is stored in a private `telegram-private.json` file, excluded from workspace
sync and API responses. HTTP errors never echo token-bearing request URLs.
Telegram redirects are refused. Relay's audit log records mutation metadata,
not request bodies.

An approval button applies to one pending provider request and exact session
creation identity. Relay rechecks both before sending a decision. Expired,
changed, or already handled requests are rejected. A truncated request offers
only **Deny**; inspect its full content in Relay before allowing it. Sessions
with terminal-only permission prompts still require their terminal. An unknown
decision result is not retried automatically.

## Discord

1. Create an application and bot in the Discord Developer Portal. Add that bot
   to a server you use and allow direct messages from it.
2. Enter the bot token and your numeric user ID in **Mastermind control → Discord**.
   Discord's Developer Mode exposes the **Copy User ID** action.
3. Send the displayed `!pair ...` command to the bot in a private DM within ten
   minutes. Relay accepts only that owner in that exact DM; server channels,
   group messages, webhooks, and bot-authored messages cannot control it.
4. Plain text queues work. `!status` reads the inbox. `!approvals` lists current
   permission requests; reply `!allow ID` or `!deny ID` with the ticket shown.

Relay uses the official [Discord REST API](https://docs.discord.com/developers/resources/message)
with a bot token, not a personal user token. It polls the private DM every five
seconds, honors rate limits, disables outbound mentions, and persists its
message cursor before later polls. An oversized unread backlog requires review
and explicit re-pairing instead of unbounded replay. The controller must remain
running; there is no public Discord webhook. Credentials live only in
`discord-private.json`, excluded from workspace snapshots and API responses.

## WhatsApp

The WhatsApp tab separates two optional connectors:

- **Personal account:** a linked device that accepts fresh `!relay ...` commands
  only from the owner's **Message yourself** chat. See
  [Personal WhatsApp](PERSONAL_WHATSAPP.md) for setup and limitations.
- **Business Cloud API:** Meta's official API, with a signed webhook bridge on
  your own HTTPS server. See [WhatsApp Business](WHATSAPP_BUSINESS.md).

Both use the tracked inbox and exact approval tickets. Pairing a connector does
not grant automatic approval authority. Tokens and linked-device keys are never
included in peer workspace copies.

## Recovery and learning

With recovery enabled, an explicitly blocked task linked to a host and session
queues one diagnosis request. Quiet or long-running processes do not trigger
recovery. Mastermind's skill requires checking current evidence, trying at most
one bounded non-destructive recovery, and asking before destructive actions,
process interruptions/restarts, permission decisions, or uncertain retries.
This is agent guidance, not an OS sandbox: the provider's configured permission
mode and host account remain the execution boundary.

Lessons record an observed problem, tested action, success/failure, and evidence.
The coordinator reads them as advisory context. They cannot grant authority or
rewrite its system instructions. The user can review and remove them. No model
training or automatic rewriting of global project instructions occurs.

The design was informed by OpenHands' separation of reusable knowledge from
individual task transcripts: [agent-memory](https://github.com/OpenHands/extensions/blob/main/skills/agent-memory/README.md)
and [skill architecture](https://github.com/OpenHands/docs/blob/main/sdk/arch/skill.mdx).
Relay's implementation and instructions are original; no OpenHands code or
runtime dependency is included. Telegram uses its documented
[Bot API](https://core.telegram.org/bots/api).

## Delivery guarantees and limits

- Inbox writes use caller-supplied identities, durable atomic saves, and one
  claim per message. Restarting does not replay processing instructions.
- Replies and remote approval deliveries record their send intent before
  transmission. A network failure may leave an unknown delivery, visible in
  Relay; avoiding duplicate actions takes precedence over automatic retries.
- The inbox retains up to 200 entries and 100 lessons. Completed/cancelled
  inbox entries can age out when capacity is reached; active work is preserved.
- Recovery generates one request per blocked task identity. Repeated attempts
  require a fresh explicit instruction and verification of the earlier result.
- Messaging and inbox transport tests use isolated fake services. Live provider
  reasoning and a user's real bot require configuration and are not claimed as
  deterministic automated test coverage.
