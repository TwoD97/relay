# Relay browser client

The React client is served by the authenticated Go controller. It uses the API in
`../docs/CONTRACT.md`; it does not hold SSH or provider credentials.

```sh
npm ci
npm run build
```

Build before compiling Go: `embed.go` embeds the generated `dist` directory. The
terminal is a separate lazy-loaded chunk; all assets are served locally.

Open the working web client through the desktop menu's **Open in Browser**, or
use the private link from `relay ui`. `npm run dev` on port 5198 is only the UI
preview and browser-test server: it does not connect to the controller. Unmocked
API requests there return an explicit JSON diagnostic instead of the HTML app.

```sh
npx playwright install chromium
npm test
RELAY_LIVE_BINARY=/absolute/path/to/relay npx playwright test e2e/live.spec.ts
```

The regular suite exercises desktop and phone layouts with controlled HTTP and
WebSocket fixtures. The optional live suite starts isolated controller/runtime
processes in a temporary directory, creates only a shell session, verifies
exclusive control from two browser tabs and session survival across controller
restart, then cleans up its own processes and directory. It keeps the real HOME
and never starts a coding-agent provider session. Screenshots are written to
`test-results`.

Runtime terminals attach as observers at the server's canonical geometry. Only
an explicit successful control claim enables input and PTY resizing. Input is
never buffered for reconnect. Setup terminals use OpenSSH's interactive prompts
directly. Source-backed attention notifications are shown as events; the UI does
not infer agent approval states or offer unverified approval shortcuts.

Each host's sessions and harness probes refresh independently, with bounded
request deadlines. A slow host cannot hold up another host or the fleet snapshot.
Mutating requests use the bootstrap CSRF token and same-origin cookies.
