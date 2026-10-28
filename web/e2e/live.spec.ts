import { test, expect } from "@playwright/test";
import { execFile, spawn, type ChildProcess } from "node:child_process";
import { existsSync } from "node:fs";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import { tmpdir } from "node:os";
import { createServer, type Server } from "node:http";
import { promisify } from "node:util";

const binary = process.env.RELAY_LIVE_BINARY;

async function stop(child: ChildProcess | undefined) {
  if (!child || child.exitCode !== null) return;
  await new Promise<void>((resolve) => {
    const timer = setTimeout(() => child.kill("SIGKILL"), 6000);
    child.once("exit", () => { clearTimeout(timer); resolve(); });
    child.kill("SIGTERM");
  });
}

async function startUI(executable: string, stateDir: string, runtimeDir: string, local = true) {
  const options = ["--listen", "127.0.0.1:0", "--state-dir", stateDir, "--runtime-dir", runtimeDir, `--local=${local}`];
  const process = spawn(executable, ["ui", "--no-login-link", ...options], { stdio: ["ignore", "pipe", "pipe"] });
  let output = "";
  // The foreground child is owned by this test. Tauri's private bootstrap mints
  // the one-time authentication URL only after that same controller is ready.
  // No mutation is retried, and no public browser-launch command is needed.
  process.stderr?.resume();
  const url = await new Promise<string>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("The disposable Relay controller did not become ready.")), 20000);
    process.stdout?.on("data", (chunk) => {
      output += String(chunk);
      if (output.length > 65536) { clearTimeout(timer); reject(new Error("The disposable controller exceeded its startup output limit.")); return; }
      const match = output.match(/controller ready at (http:\/\/127\.0\.0\.1:\d+)/);
      if (match) { clearTimeout(timer); resolve(match[1]); }
    });
    process.once("error", (error) => { clearTimeout(timer); reject(error); });
    process.once("exit", (code) => { clearTimeout(timer); reject(new Error(`Disposable Relay controller exited with code ${code}.`)); });
  }).then(async (origin) => {
    let stdout: string;
    try {
      ({ stdout } = await promisify(execFile)(executable, ["desktop", ...options], { timeout: 20000, maxBuffer: 65536 }));
    } catch {
      // An execFile error may contain its stdout, including the private token.
      throw new Error("The disposable desktop authentication bootstrap failed.");
    }
    let launch: { pid: number; address: string; url: string; protocol: number };
    try { launch = JSON.parse(stdout); } catch { throw new Error("The disposable desktop bootstrap returned invalid JSON."); }
    if (launch.pid !== process.pid || launch.address !== origin || launch.protocol !== 1) throw new Error("Desktop bootstrap did not reuse the test-owned controller.");
    const parsed = new URL(launch.url);
    if (parsed.origin !== origin || parsed.pathname !== "/auth" || !parsed.searchParams.get("token")) throw new Error("Desktop bootstrap returned an invalid private authentication URL.");
    return launch.url;
  }).catch(async (error) => { await stop(process); throw error; });
  return { process, url };
}

test("real binary: shell, exclusive viewer, controller restart and finished-session actions", async ({ page, context }, testInfo) => {
  test.skip(!binary, "Set RELAY_LIVE_BINARY to a freshly built Relay executable.");
  test.setTimeout(90000);
  const executable = resolve(binary!);
  const temporary = await mkdtemp(join(tmpdir(), "relay-web-"));
  const runtimeDir = join(temporary, "rt");
  const stateDir = join(temporary, "ui");
  let daemon: ChildProcess | undefined;
  let client: ChildProcess | undefined;
  let frames = "";
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("websocket", (socket) => socket.on("framereceived", ({ payload }) => { frames += payload.toString(); }));
  try {
    // Owning this process explicitly lets cleanup target only this test's daemon.
    daemon = spawn(executable, ["daemon", "--state-dir", runtimeDir], { stdio: "ignore" });
    await expect.poll(() => existsSync(join(runtimeDir, "run", "daemon.sock"))).toBeTruthy();
    const launch = await startUI(executable, stateDir, runtimeDir);
    client = launch.process;
    await page.goto(launch.url);
    await expect(page.getByRole("heading", { name: "Fleet overview", exact: true })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("live-fleet.png"), fullPage: true });
    await page.locator(".machine-card").filter({ hasText: "This computer" }).click();
    await page.getByRole("button", { name: "New session", exact: true }).first().click();
    await page.getByLabel("Project folder").fill(temporary);
    await page.getByRole("dialog").getByLabel("Workspace", { exact: false }).fill("Disposable smoke");
    await page.getByLabel("Session name").fill("Browser smoke");
    await page.getByRole("button", { name: "Start session", exact: true }).click();
    await expect(page.getByText("Watching", { exact: true })).toBeVisible();
    await expect(page.getByLabel("Message or command")).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Send message", exact: true })).toHaveCount(0);
    await page.locator(".terminal-canvas").click();
    await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
    // A known interactive shell makes the editing check independent of the
    // user's configured shell/readline files, without changing their HOME.
    await page.keyboard.type("exec /bin/bash --noprofile --norc -i");
    await page.keyboard.press("Enter");
    await page.keyboard.type("printf 'RELAY_%s\\n' 'LIVE_PROBE_OK'");
    await page.keyboard.press("Enter");
    await expect.poll(() => frames.includes("RELAY_LIVE_PROBE_OK")).toBeTruthy();
    await page.keyboard.type("printf 'RELAY_%s\\n' 'DIRECT_KEYS_OKX'");
    await page.keyboard.press("ArrowLeft");
    await page.keyboard.press("Backspace");
    await page.keyboard.press("Enter");
    await expect.poll(() => frames.includes("RELAY_DIRECT_KEYS_OK\r\n")).toBeTruthy();
    await page.keyboard.type("printf 'RELAY_%s\\n' 'INTERRUPT_READY'; sleep 30");
    await page.keyboard.press("Enter");
    await expect.poll(() => frames.includes("RELAY_INTERRUPT_READY")).toBeTruthy();
    await page.keyboard.press("Control+c");
    await page.keyboard.type("printf 'RELAY_%s\\n' 'INTERRUPT_OK'");
    await page.keyboard.press("Enter");
    await expect.poll(() => frames.includes("RELAY_INTERRUPT_OK")).toBeTruthy();
    const route = new URL(page.url()).hash;
    await page.screenshot({ path: testInfo.outputPath("live-session.png"), fullPage: true });

    const watcher = await context.newPage();
    const sent: string[] = [];
    watcher.on("websocket", (socket) => socket.on("framesent", ({ payload }) => { sent.push(payload.toString()); }));
    await watcher.goto(`${new URL(launch.url).origin}/${route}`);
    await expect(watcher.getByText("Watching", { exact: true })).toBeVisible();
    await expect(watcher.getByRole("button", { name: "Take control", exact: true })).toBeDisabled();
    await watcher.locator(".terminal-canvas").click();
    await watcher.keyboard.type("this must never be sent");
    await watcher.keyboard.press("Enter");
    await watcher.keyboard.press("Control+c");
    expect(sent).toEqual([]);
    await page.getByRole("button", { name: "Release control", exact: true }).click();
    await expect(watcher.getByRole("button", { name: "Take control", exact: true })).toBeEnabled();
    // Becoming available does not implicitly grant the focused viewer control.
    expect(sent).toEqual([]);
    await watcher.locator(".terminal-canvas").click();
    await expect(watcher.getByText("Controlling", { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Take control", exact: true })).toBeDisabled();
    await watcher.keyboard.type("printf 'RELAY_%s\\n' 'SECOND_VIEWER_OK'");
    await watcher.keyboard.press("Enter");
    await expect.poll(() => frames.includes("RELAY_SECOND_VIEWER_OK")).toBeTruthy();
    await watcher.close();

    await stop(client);
    expect(daemon.exitCode).toBeNull();
    const restarted = await startUI(executable, stateDir, runtimeDir);
    client = restarted.process;
    frames = "";
    await page.goto(restarted.url);
    // The authentication document performs a same-origin handoff. Wait for it
    // to finish before restoring a route; navigation must not race that handoff.
    await expect(page.getByRole("heading", { name: "Fleet overview", exact: true })).toBeVisible();
    await page.goto(`${new URL(restarted.url).origin}/${route}`);
    await expect(page.getByRole("heading", { name: "Browser smoke", exact: true })).toBeVisible();
    await expect(page.getByText("Running", { exact: true })).toBeVisible();
    await expect.poll(() => frames.includes("RELAY_LIVE_PROBE_OK")).toBeTruthy();
    await page.getByRole("button", { name: "Take control", exact: true }).click();
    await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
    await page.keyboard.type("exit 7");
    await page.keyboard.press("Enter");
    const finishedActions = page.getByRole("region", { name: "Finished session actions" });
    await expect(finishedActions).toBeVisible();
    await expect(page.getByText("This process has exited with code 7.", { exact: false })).toBeVisible();
    await expect(page.getByText("Saved output", { exact: true })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("live-finished-actions.png"), fullPage: true });

    const origin = new URL(restarted.url).origin;
    const oldID = decodeURIComponent(route.split("/").at(-1)!);
    const sessionsURL = `${origin}/api/hosts/local/runtime/sessions`;
    const oldHistoryURL = `${sessionsURL}/${encodeURIComponent(oldID)}/history`;
    const savedOutput = await (await page.request.get(oldHistoryURL)).text();
    expect(savedOutput).toContain("RELAY_LIVE_PROBE_OK");
    const createdRequest = page.waitForResponse((response) => response.url() === sessionsURL && response.request().method() === "POST");
    await finishedActions.getByRole("button", { name: "Open terminal here", exact: true }).click();
    const createdResponse = await createdRequest;
    expect(createdResponse.status()).toBe(201);
    const continuation = await createdResponse.json();
    expect(continuation).toMatchObject({ title: "Browser smoke · Terminal", cwd: temporary, workspace: "Disposable smoke", harness: "shell", status: "running" });
    expect(continuation.id).not.toBe(oldID);
    await expect(page.getByRole("heading", { name: continuation.title, exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Take control", exact: true }).click();
    await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
    await page.keyboard.type("printf 'RELAY_%s\\n' 'CONTINUE_HERE_OK'");
    await page.keyboard.press("Enter");
    await expect.poll(() => frames.includes("RELAY_CONTINUE_HERE_OK")).toBeTruthy();
    expect(await (await page.request.get(oldHistoryURL)).text()).toBe(savedOutput);
    const bothSessions = await (await page.request.get(sessionsURL)).json();
    expect(bothSessions).toHaveLength(2);
    expect(bothSessions.find((session: { id: string }) => session.id === oldID)).toMatchObject({ harness: "shell", status: "exited", exitCode: 7 });

    // Stop only the new terminal, then explicitly close the old saved record.
    await page.getByRole("button", { name: "End and remove session", exact: true }).click();
    await page.getByRole("button", { name: "End and remove", exact: true }).click();
    await expect(page.getByRole("heading", { name: "This computer", exact: true })).toBeVisible();
    await page.goto(`${origin}/${route}`);
    await expect(finishedActions).toBeVisible();
    await finishedActions.getByRole("button", { name: "Close session", exact: true }).click();
    await expect(page.getByRole("dialog")).toContainText("removes the saved session and its terminal output");
    await page.getByRole("dialog").getByRole("button", { name: "Cancel", exact: true }).click();
    expect((await page.request.get(oldHistoryURL)).ok()).toBeTruthy();
    await finishedActions.getByRole("button", { name: "Close session", exact: true }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Close session", exact: true }).click();
    await expect(page.getByRole("heading", { name: "This computer", exact: true })).toBeVisible();
    expect(await (await page.request.get(sessionsURL)).json()).toEqual([]);
    expect((await page.request.get(oldHistoryURL)).status()).toBe(404);
    expect(errors).toEqual([]);
  } finally {
    await stop(client);
    await stop(daemon);
    await rm(temporary, { recursive: true, force: true });
  }
});

for (const variant of ["ascii", "wide", "graphics"] as const) {
  test(`real terminal: ${variant} right-edge output continues identically after a new viewer attaches`, async ({ page, context }) => {
    test.skip(!binary, "Set RELAY_LIVE_BINARY to a freshly built Relay executable.");
    test.setTimeout(60000);
    const temporary = await mkdtemp(join(tmpdir(), "relay-rendering-"));
    const runtimeDir = join(temporary, "runtime");
    const proceed = join(temporary, "continue");
    const script = join(temporary, "screen.py");
    let daemon: ChildProcess | undefined;
    let client: ChildProcess | undefined;
    try {
      // Output comes from a real PTY and pauses exactly at its right edge.
      // The second client must reconstruct cursor/charset state, then handle
      // new output just like the original xterm renderer. No provider account.
      await writeFile(script, `import os, time\nfrom pathlib import Path\ncols = os.get_terminal_size().columns\nmode = ${JSON.stringify(variant)}\ntext = ("X" * (cols - 2) + "界") if mode == "wide" else ("q" if mode == "graphics" else "X") * cols\nprefix = "\\x1b(0" if mode == "graphics" else ""\nos.write(1, ("\\x1b[2J\\x1b[H" + prefix + text).encode())\nwhile not Path(${JSON.stringify(proceed)}).exists(): time.sleep(.02)\nos.write(1, b"q" if mode == "graphics" else b"Z")\nwhile True: time.sleep(1)\n`);
      daemon = spawn(resolve(binary!), ["daemon", "--state-dir", runtimeDir], { stdio: "ignore" });
      await expect.poll(() => existsSync(join(runtimeDir, "run", "daemon.sock"))).toBeTruthy();
      const launch = await startUI(resolve(binary!), join(temporary, "client"), runtimeDir);
      client = launch.process;
      await page.goto(launch.url);
      await expect(page.getByRole("heading", { name: "Fleet overview", exact: true })).toBeVisible();
      await page.locator(".machine-card").filter({ hasText: "This computer" }).click();
      await page.getByRole("button", { name: "New session", exact: true }).first().click();
      await page.getByLabel("Project folder").fill(temporary);
      await page.getByRole("dialog").getByLabel("Workspace", { exact: false }).fill("Rendering test");
      await page.getByLabel("Session name").fill("Right-edge fixture");
      await page.getByRole("button", { name: "Start session", exact: true }).click();
      await page.getByRole("button", { name: "Take control", exact: true }).click();
      await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
      await page.locator(".xterm-helper-textarea").focus();
      await page.keyboard.type(`exec python3 '${script.replaceAll("'", "'\\''")}'`);
      await page.keyboard.press("Enter");
      const first = page.locator(".xterm-rows > div").nth(0);
      const finalGlyph = variant === "graphics" ? "─" : variant === "wide" ? "界" : "X";
      await expect(first).toHaveText(new RegExp(`${finalGlyph}$`));
      const initial = await first.textContent();
      expect(initial!.length).toBeGreaterThan(10);
      const watcher = await context.newPage();
      try {
        await watcher.goto(page.url());
        await expect(watcher.getByText("Watching", { exact: true })).toBeVisible();
        await expect(watcher.locator(".xterm-rows > div").nth(0)).toHaveText(initial!);
        expect(await watcher.locator(".terminal-shell").getAttribute("data-terminal-cols")).toBe(await page.locator(".terminal-shell").getAttribute("data-terminal-cols"));
        await writeFile(proceed, "continue");
        const next = variant === "graphics" ? "─" : "Z";
        for (const view of [page, watcher]) {
          await expect(view.locator(".xterm-rows > div").nth(0)).toHaveText(initial!);
          await expect(view.locator(".xterm-rows > div").nth(1)).toHaveText(next);
        }
      } finally { await watcher.close(); }
    } finally {
      await stop(client);
      await stop(daemon);
      await rm(temporary, { recursive: true, force: true });
    }
  });
}

test("one-time sign-in link works when clicked from an external site", async ({ page }) => {
  test.skip(!binary, "Set RELAY_LIVE_BINARY to a freshly built Relay executable.");
  test.setTimeout(45000);
  const temporary = await mkdtemp(join(tmpdir(), "relay-auth-"));
  let client: ChildProcess | undefined;
  let external: Server | undefined;
  try {
    const launch = await startUI(resolve(binary!), join(temporary, "ui"), join(temporary, "unused-runtime"), false);
    client = launch.process;
    external = createServer((_request, response) => {
      response.writeHead(200, { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store" });
      response.end(`<!doctype html><html lang="en"><title>External chat</title><body><a href="${launch.url}">Open Relay preview</a></body></html>`);
    });
    await new Promise<void>((resolve, reject) => { external!.once("error", reject); external!.listen(0, "127.0.0.1", resolve); });
    const address = external.address();
    if (!address || typeof address === "string") throw new Error("External test page did not obtain a TCP port.");
    await page.goto(`http://localhost:${address.port}`);
    const authRequest = page.waitForRequest((request) => request.url() === launch.url);
    await page.getByRole("link", { name: "Open Relay preview" }).click();
    const metadata = await (await authRequest).allHeaders();
    expect(metadata["sec-fetch-site"]).toBe("cross-site");
    expect(metadata["sec-fetch-mode"]).toBe("navigate");
    expect(metadata["sec-fetch-dest"]).toBe("document");
    await expect(page.getByRole("heading", { name: "Fleet overview", exact: true })).toBeVisible();
    await expect(page).toHaveTitle("Relay · Your fleet");
    const state = await page.request.get(`${new URL(launch.url).origin}/api/state`);
    expect(state.ok()).toBeTruthy();
    expect(await state.json()).toMatchObject({ hosts: [] });
  } finally {
    await stop(client);
    if (external) await new Promise<void>((resolve) => external!.close(() => resolve()));
    await rm(temporary, { recursive: true, force: true });
  }
});

test("real binary: permission hook decision and isolated observer preserve the agent PTY", async ({ page }, testInfo) => {
  test.skip(!binary, "Set RELAY_LIVE_BINARY to a freshly built Relay executable.");
  test.setTimeout(120000);
  const executable = resolve(binary!);
  const temporary = await mkdtemp(join(tmpdir(), "relay-activity-"));
  const runtimeDir = join(temporary, "runtime");
  const projectDir = join(temporary, "project with spaces");
  let daemon: ChildProcess | undefined;
  let client: ChildProcess | undefined;
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  try {
    await mkdir(join(runtimeDir, "bin"), { recursive: true, mode: 0o700 });
    await mkdir(projectDir, { mode: 0o700 });
    // These deterministic providers shadow user installations only inside this
    // private runtime. No account, authentication file, or real model is used.
    await writeFile(join(runtimeDir, "bin", "codex"), "#!/bin/sh\nif [ \"$1\" = --version ]; then printf 'codex-cli 0.153.4\\n'; exit 0; fi\nexit 1\n", { mode: 0o700 });
    await writeFile(join(runtimeDir, "bin", "claude"), String.raw`#!/usr/bin/env python3
import json, os, pathlib, subprocess, sys
args = sys.argv[1:]
flags = ["--print", "--safe-mode", "--tools", "--disallowedTools", "--strict-mcp-config", "--mcp-config", "--permission-mode", "--disable-slash-commands", "--no-session-persistence", "--no-chrome", "--output-format", "--json-schema", "--system-prompt", "--model", "--settings", "--setting-sources", "--max-turns"]
if args == ["--version"]:
    print("2.1.209 (Claude Code)")
    sys.exit(0)
if args == ["--help"]:
    print(" ".join(flags))
    sys.exit(0)
if args == ["auth", "status"]:
    print('{"loggedIn":true}')
    sys.exit(0)
root = pathlib.Path(__file__).resolve().parent.parent
if "--print" in args:
    for flag in flags:
        assert flag in args, "missing observer safety flag"
    for flag, value in {"--tools":"", "--disallowedTools":"mcp__*", "--mcp-config":'{"mcpServers":{}}', "--permission-mode":"dontAsk", "--setting-sources":"", "--settings":'{"disableAllHooks":true}', "--max-turns":"1", "--model":"fixture-small"}.items():
        assert args[args.index(flag)+1] == value, "unsafe observer flag"
    assert not any(key.startswith("RELAY_") for key in os.environ), "observer inherited session capability"
    cwd = pathlib.Path.cwd()
    assert cwd.parent == root and cwd.name.startswith(".observer-work-"), "observer used source project directory"
    context = json.load(sys.stdin)
    assert context["partialObservation"] is True
    assert "RELAY_APPROVAL_RECEIVED_ALLOW" in context["terminalTail"]
    assert "not-read-private-fixture-marker" not in json.dumps(context)
    proof = {"isolationVerified":True,"sourceObserved":True,"model":"fixture-small"}
    (root / "summary-proof.json").write_text(json.dumps(proof))
    print(json.dumps({"type":"result","subtype":"success","is_error":False,"structured_output":{"summary":"Fixture summary: two files inspected; next step awaits your input.","steps":["Inspected two fixture files."],"nextSteps":["Choose the next task in the existing terminal."],"blockers":[]}}))
    sys.exit(0)
settings = json.loads(args[args.index("--settings")+1])
hook = settings["hooks"]["PermissionRequest"][0]["hooks"][0]["command"]
assert " hook --provider claude --event permission" in hook
print("Waiting for fixture permission request.", flush=True)
request = {"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"printf fixture-only-action"},"cwd":os.getcwd(),"permission_mode":"default","transcript_path":"not-read-private-fixture-marker"}
result = subprocess.run(hook, shell=True, input=json.dumps(request), text=True, capture_output=True, timeout=75, check=True)
decision = json.loads(result.stdout)["hookSpecificOutput"]
assert decision["hookEventName"] == "PermissionRequest" and decision["decision"] == {"behavior":"allow"}
print("RELAY_APPROVAL_RECEIVED_ALLOW", flush=True)
print("Read two fixture files. Waiting for the next human instruction.", flush=True)
for line in sys.stdin:
    print("RELAY_AGENT_INPUT:" + line.strip(), flush=True)
`, { mode: 0o700 });
    daemon = spawn(executable, ["daemon", "--state-dir", runtimeDir], { stdio: "ignore" });
    await expect.poll(() => existsSync(join(runtimeDir, "run", "daemon.sock"))).toBeTruthy();
    const launch = await startUI(executable, join(temporary, "ui"), runtimeDir);
    client = launch.process;
    const origin = new URL(launch.url).origin;
    const sessionsURL = `${origin}/api/hosts/local/runtime/sessions`;
    await page.goto(launch.url);
    await page.locator(".machine-card").filter({ hasText: "This computer" }).click();
    await page.getByRole("button", { name: "New session", exact: true }).first().click();
    await page.getByRole("radio", { name: /Claude Code/ }).check();
    await page.getByLabel("Project folder").fill(projectDir);
    await page.getByLabel("Session name").fill("Permission fixture");
    const createdResponse = page.waitForResponse((response) => response.url() === sessionsURL && response.request().method() === "POST");
    await page.getByRole("button", { name: "Start session", exact: true }).click();
    const created = await createdResponse;
    expect(created.status()).toBe(201);
    const session = await created.json();
    const sessionRoute = `#host/local/session/${session.id}`;
    await expect(page.getByRole("heading", { name: "Permission fixture", exact: true })).toBeVisible();
    await page.goto(`${origin}/#activity`);
    const approval = page.getByRole("article", { name: "Bash request for Permission fixture" });
    await expect(approval).toBeVisible();
    await expect(approval).toContainText("printf fixture-only-action");
    await expect(approval).toContainText("Provider mode: default");
    const decisionResponse = page.waitForResponse((response) => response.url().endsWith("/decision") && response.request().method() === "POST");
    await approval.getByRole("button", { name: "Approve once", exact: true }).click();
    const decided = await decisionResponse;
    expect(decided.status()).toBe(200);
    expect(await decided.json()).toMatchObject({ sessionId: session.id, sessionCreatedAt: session.createdAt, status: "submitted", decision: "allow" });
    await expect(approval).toContainText("Approved once");
    await expect.poll(async () => (await page.request.get(`${sessionsURL}/${session.id}/history`)).text()).toContain("RELAY_APPROVAL_RECEIVED_ALLOW");
    const summaryHost = page.getByRole("region", { name: "This computer summaries", exact: true });
    await summaryHost.getByRole("button", { name: "Settings", exact: true }).click();
    await page.getByLabel("Enable session summaries on this machine").check();
    await page.getByLabel("Summary model").fill("fixture-small");
    await page.getByRole("button", { name: "Save settings", exact: true }).click();
    await expect(page.getByRole("dialog", { name: "Summary settings" })).toHaveCount(0);
    const summary = page.getByRole("article", { name: "Permission fixture summary" });
    await expect(summary).toContainText("Fixture summary: two files inspected; next step awaits your input.", { timeout: 20000 });
    expect(JSON.parse(await readFile(join(runtimeDir, "summary-proof.json"), "utf8"))).toEqual({ isolationVerified: true, sourceObserved: true, model: "fixture-small" });
    const observer = await (await page.request.get(`${origin}/api/hosts/local/runtime/observer`)).json();
    expect(observer.limits.remaining).toBe(59);
    expect(observer.summaries).toHaveLength(1);
    expect(observer.summaries[0]).toMatchObject({ sessionId: session.id, sessionCreatedAt: session.createdAt, status: "ready", model: "fixture-small" });
    const sessions = await (await page.request.get(sessionsURL)).json();
    expect(sessions).toHaveLength(1);
    expect(sessions[0]).toMatchObject({ id: session.id, createdAt: session.createdAt, status: "running", permissions: { support: "active", mode: "default" } });
    expect(await (await page.request.get(`${sessionsURL}/${session.id}/history`)).text()).not.toContain("RELAY_AGENT_INPUT:");
    await page.screenshot({ path: testInfo.outputPath("live-activity.png"), fullPage: true });
    await summary.getByRole("button", { name: "Open terminal", exact: true }).click();
    await expect(page).toHaveURL(`${origin}/${sessionRoute}`);
    await page.getByRole("button", { name: "Take control", exact: true }).click();
    await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
    await page.keyboard.type("still-my-original-agent");
    await page.keyboard.press("Enter");
    await expect.poll(async () => (await page.request.get(`${sessionsURL}/${session.id}/history`)).text()).toContain("RELAY_AGENT_INPUT:still-my-original-agent");
    expect(errors).toEqual([]);
  } finally {
    await stop(client);
    await stop(daemon);
    await rm(temporary, { recursive: true, force: true });
  }
});

test("real binary: Manual and Auto reach isolated Claude and Codex PTYs exactly once", async ({ page }) => {
  test.skip(!binary, "Set RELAY_LIVE_BINARY to a freshly built Relay executable.");
  test.setTimeout(120000);
  const executable = resolve(binary!);
  const temporary = await mkdtemp(join(tmpdir(), "relay-agent-mode-"));
  const runtimeDir = join(temporary, "runtime");
  const projectDir = join(temporary, "literal project ' $()");
  let daemon: ChildProcess | undefined;
  let client: ChildProcess | undefined;
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  try {
    await mkdir(join(runtimeDir, "bin"), { recursive: true, mode: 0o700 });
    await mkdir(projectDir, { mode: 0o700 });
    // These executables never consult HOME, provider configuration, or the
    // network. Discovery and help probes do not count as agent launches.
    const stub = String.raw`#!/usr/bin/env python3
import json, os, pathlib, sys
args = sys.argv[1:]
provider = pathlib.Path(__file__).name
root = pathlib.Path(__file__).resolve().parent.parent
if args == ["--version"]:
    print("fixture-1.0")
    sys.exit(0)
if args == ["--help"]:
    print("--dangerously-skip-permissions --append-system-prompt --dangerously-bypass-approvals-and-sandbox [PROMPT]")
    sys.exit(0)
if args in (["auth", "status"], ["login", "status"]):
    sys.exit(0)
identity = os.environ["RELAY_SESSION_ID"]
capture = {"provider":provider,"args":args,"mode":os.environ.get("RELAY_AGENT_MODE"),"sessionId":identity,"cwd":os.getcwd(),"tty":os.isatty(0) and os.isatty(1)}
(root / (identity + ".json")).write_text(json.dumps(capture))
with (root / (provider + "-launches")).open("a") as launches:
    launches.write(identity + "\n")
print("RELAY_MODE_FIXTURE_READY", flush=True)
for line in sys.stdin:
    print("RELAY_MODE_FIXTURE_INPUT:" + line.strip(), flush=True)
`;
    for (const harness of ["claude", "codex"]) await writeFile(join(runtimeDir, "bin", harness), stub, { mode: 0o700 });
    daemon = spawn(executable, ["daemon", "--state-dir", runtimeDir], { stdio: "ignore" });
    await expect.poll(() => existsSync(join(runtimeDir, "run", "daemon.sock"))).toBeTruthy();
    const launch = await startUI(executable, join(temporary, "ui"), runtimeDir);
    client = launch.process;
    const origin = new URL(launch.url).origin;
    const sessionsURL = `${origin}/api/hosts/local/runtime/sessions`;
    const createdIDs: string[] = [];
    const providerIDs: Record<string, string[]> = { claude: [], codex: [] };
    await page.goto(launch.url);
    for (const harness of ["claude", "codex"] as const) {
      for (const mode of ["manual", "auto"] as const) {
        await page.goto(`${origin}/#host/local`);
        await page.getByRole("button", { name: "New session", exact: true }).first().click();
        const dialog = page.getByRole("dialog", { name: "Start a session", exact: true });
        await dialog.getByRole("radio", { name: harness === "claude" ? /Claude Code/ : /Codex/ }).check();
        await dialog.getByLabel("Project folder").fill(projectDir);
        const title = `${harness} ${mode} fixture`;
        await dialog.getByLabel("Session name").fill(title);
        await expect(dialog.getByRole("radio", { name: "Manual", exact: true })).toBeChecked();
        if (mode === "auto") await dialog.getByRole("radio", { name: "Auto", exact: true }).check();
        const responsePromise = page.waitForResponse((response) => response.url() === sessionsURL && response.request().method() === "POST");
        await dialog.getByRole("button", { name: "Start session", exact: true }).click();
        const response = await responsePromise;
        expect(response.status()).toBe(201);
        expect(response.request().postDataJSON()).toMatchObject({ harness, agentMode: mode, cwd: projectDir });
        const session = await response.json();
        expect(session).toMatchObject({ harness, agentMode: mode, cwd: projectDir, title, status: "running" });
        createdIDs.push(session.id);
        providerIDs[harness].push(session.id);
        await expect(page.getByRole("heading", { name: title, exact: true })).toBeVisible();
        await expect.poll(() => existsSync(join(runtimeDir, `${session.id}.json`))).toBeTruthy();
        const capture = JSON.parse(await readFile(join(runtimeDir, `${session.id}.json`), "utf8"));
        expect(capture).toMatchObject({ provider: harness, mode, sessionId: session.id, cwd: projectDir, tty: true });
        const args = capture.args as string[];
        if (mode === "auto") {
          const instruction = args.at(-1)!;
          expect(instruction).toContain("Relay session mode: Auto, explicitly selected by the user");
          expect(instruction).toContain("If no task has been provided, wait for the user's task.");
          expect(instruction).toContain('"$relay_cli" fleet list');
          expect(args.filter((arg) => arg.includes("Relay session mode: Auto"))).toEqual([instruction]);
          expect(args.slice(-3)).toEqual(harness === "claude"
            ? ["--dangerously-skip-permissions", "--append-system-prompt", instruction]
            : ["--dangerously-bypass-approvals-and-sandbox", "--", instruction]);
        } else {
          expect(args.some((arg) => arg.includes("dangerously") || arg.includes("Relay session mode: Auto") || arg === "--append-system-prompt")).toBeFalsy();
        }
        const badge = `Launch mode: ${mode === "auto" ? "Auto · full access" : "Manual"}`;
        await expect(page.getByLabel(badge, { exact: true })).toBeVisible();
        await page.reload();
        await expect(page.getByLabel(badge, { exact: true })).toBeVisible();
        const historyURL = `${sessionsURL}/${session.id}/history`;
        await expect.poll(async () => (await page.request.get(historyURL)).text()).toContain("RELAY_MODE_FIXTURE_READY");
        await page.getByRole("button", { name: "Take control", exact: true }).click();
        await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
        await page.keyboard.type(`still-interactive-${harness}-${mode}`);
        await page.keyboard.press("Enter");
        await expect.poll(async () => (await page.request.get(historyURL)).text()).toContain(`RELAY_MODE_FIXTURE_INPUT:still-interactive-${harness}-${mode}`);
      }
    }
    const sessions = await (await page.request.get(sessionsURL)).json();
    expect(sessions).toHaveLength(4);
    expect(sessions.map((session: { id: string }) => session.id).sort()).toEqual([...createdIDs].sort());
    for (const harness of ["claude", "codex"]) {
      const launches = (await readFile(join(runtimeDir, `${harness}-launches`), "utf8")).trim().split("\n");
      expect(launches).toEqual(providerIDs[harness]);
    }
    expect(errors).toEqual([]);
  } finally {
    await stop(client);
    await stop(daemon);
    await rm(temporary, { recursive: true, force: true });
  }
});
