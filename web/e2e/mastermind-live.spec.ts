import { test, expect } from "@playwright/test";
import { execFile, spawn, type ChildProcess } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync } from "node:fs";
import { chmod, copyFile, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import { tmpdir } from "node:os";
import { promisify } from "node:util";

const binary = process.env.RELAY_LIVE_BINARY;

async function stop(child: ChildProcess | undefined) {
  if (!child || child.exitCode !== null) return;
  await new Promise<void>((done) => {
    const timer = setTimeout(() => child.kill("SIGKILL"), 6000);
    child.once("exit", () => { clearTimeout(timer); done(); });
    child.kill("SIGTERM");
  });
}

// The test owns a foreground controller and daemon. The private desktop
// bootstrap only issues a login capability for that same controller.
async function startController(executable: string, stateDir: string, runtimeDir: string) {
  const options = ["--listen", "127.0.0.1:0", "--state-dir", stateDir, "--runtime-dir", runtimeDir, "--local=true"];
  const child = spawn(executable, ["ui", "--no-login-link", ...options], { stdio: ["ignore", "pipe", "pipe"] });
  child.stderr?.resume();
  try {
    const origin = await new Promise<string>((done, reject) => {
      let output = "";
      const timer = setTimeout(() => reject(new Error("Disposable controller startup timed out.")), 20000);
      child.stdout?.on("data", (chunk) => {
        output += String(chunk);
        if (output.length > 65536) { clearTimeout(timer); reject(new Error("Disposable controller startup exceeded its output limit.")); return; }
        const match = output.match(/controller ready at (http:\/\/127\.0\.0\.1:\d+)/);
        if (match) { clearTimeout(timer); done(match[1]); }
      });
      child.once("error", () => { clearTimeout(timer); reject(new Error("Disposable controller could not start.")); });
      child.once("exit", (code) => { clearTimeout(timer); reject(new Error(`Disposable controller exited with code ${code}.`)); });
    });
    let stdout: string;
    try {
      ({ stdout } = await promisify(execFile)(executable, ["desktop", ...options], { timeout: 20000, maxBuffer: 65536 }));
    } catch {
      // Never expose execFile stdout: it can contain the private launch token.
      throw new Error("Disposable desktop authentication bootstrap failed.");
    }
    let launch: { pid: number; address: string; url: string; protocol: number };
    try { launch = JSON.parse(stdout); } catch { throw new Error("Disposable desktop bootstrap returned invalid JSON."); }
    if (launch.pid !== child.pid || launch.address !== origin || launch.protocol !== 1) throw new Error("Desktop bootstrap did not reuse the owned controller.");
    const parsed = new URL(launch.url);
    if (parsed.origin !== origin || parsed.pathname !== "/auth" || !parsed.searchParams.get("token")) throw new Error("Desktop bootstrap returned an invalid authentication URL.");
    return { child, origin, url: launch.url };
  } catch (error) {
    await stop(child);
    throw error;
  }
}

const providerFixture = String.raw`#!/usr/bin/env python3
import json, os, pathlib, subprocess, sys
args = sys.argv[1:]
root = pathlib.Path(__file__).resolve().parent.parent
provider = pathlib.Path(__file__).name
if args == ["--version"]:
    print("fixture-1.0")
    sys.exit(0)
if args == ["--help"]:
    flags = "--append-system-prompt [PROMPT]"
    if not (root / "disable-auto").exists():
        flags += " --dangerously-skip-permissions --dangerously-bypass-approvals-and-sandbox"
    print(flags)
    sys.exit(0)
if args in (["auth", "status"], ["login", "status"]):
    sys.exit(0)
identity = os.environ["RELAY_SESSION_ID"]
relay_bin, relay_socket = os.environ["RELAY_BIN"], os.environ["RELAY_SOCKET"]
assert pathlib.Path(relay_bin).is_absolute() and os.access(relay_bin, os.X_OK), "missing scoped Relay executable"
assert pathlib.Path(relay_socket) == root / "run" / "daemon.sock", "CLI selected a runtime outside this fixture"
discovery = subprocess.run([relay_bin, "fleet", "list"], stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=10, check=True)
assert len(discovery.stdout) < 131072, "unexpected fleet output size"
fleet = json.loads(discovery.stdout)
capture = {"sessionId":identity,"provider":provider,"mode":os.environ.get("RELAY_AGENT_MODE"),"args":args,"cwd":os.getcwd(),"tty":os.isatty(0) and os.isatty(1),"relayBin":relay_bin,"relaySocket":relay_socket,"fleet":fleet}
(root / (identity + ".json")).write_text(json.dumps(capture))
with (root / "agent-launches").open("a") as launches:
    launches.write(identity + "\n")
print("RELAY_MASTERMIND_FIXTURE_READY", flush=True)
for line in sys.stdin:
    print("RELAY_MASTERMIND_INPUT:" + line.strip(), flush=True)
`;

test("real binary: Mastermind reuses its exact persistent agent and preserves previous configurations", async ({ page, context }, testInfo) => {
  test.skip(!binary, "Set RELAY_LIVE_BINARY to a freshly built Relay executable.");
  test.setTimeout(120000);
  const executable = resolve(binary!);
  const temporary = await mkdtemp(join(tmpdir(), "relay-mastermind-"));
  const runtimeDir = join(temporary, "runtime");
  const stateDir = join(temporary, "controller");
  const projectDir = join(temporary, "literal project ' $()");
  const nextDir = join(temporary, "second project");
  let daemon: ChildProcess | undefined;
  let controller: ChildProcess | undefined;
  const errors: string[] = [];
  const mutations: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  context.on("request", (request) => {
    const path = new URL(request.url()).pathname;
    if (request.method() !== "GET" && path.startsWith("/api/mastermind")) mutations.push(path);
  });
  try {
    await mkdir(join(runtimeDir, "bin"), { recursive: true, mode: 0o700 });
    await mkdir(projectDir, { mode: 0o700 });
    await mkdir(nextDir, { mode: 0o700 });
    // No HOME/configuration inspection or inference: these provider fixtures
    // record only this disposable session's arguments and echo explicit input.

    for (const harness of ["claude", "codex"]) await writeFile(join(runtimeDir, "bin", harness), providerFixture, { mode: 0o700 });
    daemon = spawn(executable, ["daemon", "--state-dir", runtimeDir], { stdio: "ignore" });
    await expect.poll(() => existsSync(join(runtimeDir, "run", "daemon.sock"))).toBeTruthy();
    let launch = await startController(executable, stateDir, runtimeDir);
    controller = launch.child;
    const fleetSource = JSON.parse(await readFile(join(stateDir, "fleet-source.json"), "utf8")).id;
    await expect.poll(() => existsSync(join(runtimeDir, "fleet", `${fleetSource}.json`))).toBeTruthy();
    await page.goto(launch.url);
    const card = page.getByRole("region", { name: "Mastermind", exact: true });
    await expect(card).toBeVisible();
    await card.getByRole("button", { name: "Set up Mastermind", exact: true }).click();
    const setup = page.getByRole("dialog", { name: "Set up Mastermind", exact: true });
    await setup.getByLabel("Machine", { exact: true }).selectOption("local");
    await setup.getByRole("radio", { name: /Claude Code/ }).check();
    await expect(setup.getByRole("radio", { name: "Manual", exact: true })).toBeChecked();
    await setup.getByLabel("Project folder").fill(projectDir);
    const configured = page.waitForResponse((response) => response.url().endsWith("/api/mastermind/config") && response.request().method() === "POST");
    await setup.getByRole("button", { name: "Save and start Mastermind", exact: true }).click();
    expect((await configured).status()).toBe(202);
    const state = async () => (await page.request.get(`${launch.origin}/api/mastermind`)).json();
    const sessions = async () => (await page.request.get(`${launch.origin}/api/hosts/local/runtime/sessions`)).json();
    await expect.poll(async () => (await state()).session?.status).toBe("running");
    const first = (await state()).session;
    expect(first).toMatchObject({ harness: "claude", agentMode: "manual", cwd: projectDir, status: "running" });
    expect(first.mastermindId).toMatch(/^[0-9a-f]{32}$/);
    await expect.poll(() => existsSync(join(runtimeDir, `${first.id}.json`))).toBeTruthy();
    const captured = JSON.parse(await readFile(join(runtimeDir, `${first.id}.json`), "utf8"));
    expect(captured).toMatchObject({ sessionId: first.id, provider: "claude", mode: "manual", cwd: projectDir, tty: true, relayBin: executable, relaySocket: join(runtimeDir, "run", "daemon.sock") });
    expect(captured.fleet.sources).toHaveLength(1);
    expect(captured.fleet.sources[0]).toMatchObject({ controllerId: fleetSource, stale: false, hosts: [] });
    const args = captured.args as string[];
    expect(args.slice(-2)[0]).toBe("--append-system-prompt");
    expect(args.at(-1)).toContain("You are Relay Mastermind");
    expect(args.at(-1)).toContain('"$relay_cli" fleet list');
    expect(args.at(-1)).toContain('relay_cli="${RELAY_BIN:-$HOME/.local/share/relay/bin/relay}"');
    expect(args.at(-1)).toContain("If no task has been provided, wait for the user to describe one.");
    expect(args.filter((arg) => arg.includes("You are Relay Mastermind"))).toHaveLength(1);
    expect(args.some((arg) => arg.includes("dangerously") || arg.includes("Relay session mode: Auto"))).toBeFalsy();
    await expect(card.getByRole("button", { name: "Take control", exact: true })).toBeVisible();
    await card.getByRole("button", { name: "Take control", exact: true }).click();
    await expect(card.getByText("Controlling", { exact: true })).toBeVisible();
    await page.keyboard.type("only-the-user-starts-work");
    await page.keyboard.press("Enter");
    await expect.poll(async () => (await page.request.get(`${launch.origin}/api/hosts/local/runtime/sessions/${first.id}/history`)).text()).toContain("RELAY_MASTERMIND_INPUT:only-the-user-starts-work");
    await page.reload();
    await expect(card.getByRole("button", { name: "Take control", exact: true })).toBeVisible();
    const secondViewer = await context.newPage();
    await secondViewer.goto(launch.origin);
    await expect(secondViewer.getByRole("region", { name: "Mastermind", exact: true })).toBeVisible();
    for (let count = 0; count < 3; count++) expect((await state()).session).toMatchObject({ id: first.id, createdAt: first.createdAt, mastermindId: first.mastermindId });
    expect(await sessions()).toHaveLength(1);
    expect((await readFile(join(runtimeDir, "agent-launches"), "utf8")).trim().split("\n")).toEqual([first.id]);
    expect(mutations).toEqual(["/api/mastermind/config"]);
    await secondViewer.close();

    // Restart only the owned controller, leaving the daemon and PTY untouched.
    await stop(controller);
    expect(daemon.exitCode).toBeNull();
    launch = await startController(executable, stateDir, runtimeDir);
    controller = launch.child;
    await page.goto(launch.url);
    await expect(card.getByRole("button", { name: "Take control", exact: true })).toBeVisible();
    await expect.poll(async () => (await state()).session?.id).toBe(first.id);
    expect((await state()).session).toMatchObject({ id: first.id, createdAt: first.createdAt, mastermindId: first.mastermindId, status: "running" });
    expect((await readFile(join(runtimeDir, "agent-launches"), "utf8")).trim().split("\n")).toEqual([first.id]);
    expect(mutations).toEqual(["/api/mastermind/config"]);

    await card.getByRole("button", { name: "Mastermind settings", exact: true }).click();
    const settings = page.getByRole("dialog", { name: "Mastermind settings", exact: true });
    await settings.getByRole("radio", { name: /Codex/ }).check();
    await settings.getByLabel("Project folder").fill(nextDir);
    const changed = page.waitForResponse((response) => response.url().endsWith("/api/mastermind/config") && response.request().method() === "POST");
    await settings.getByRole("button", { name: "Save and start Mastermind", exact: true }).click();
    expect((await changed).status()).toBe(202);
    await expect.poll(async () => (await state()).session?.harness).toBe("codex");
    const second = (await state()).session;
    expect(second).toMatchObject({ harness: "codex", agentMode: "manual", cwd: nextDir, status: "running" });
    expect(second.id).not.toBe(first.id);
    expect(second.mastermindId).not.toBe(first.mastermindId);
    await expect.poll(() => existsSync(join(runtimeDir, `${second.id}.json`))).toBeTruthy();
    const nextCapture = JSON.parse(await readFile(join(runtimeDir, `${second.id}.json`), "utf8"));
    expect(nextCapture).toMatchObject({ sessionId: second.id, provider: "codex", mode: "manual", cwd: nextDir, tty: true, relayBin: executable, relaySocket: join(runtimeDir, "run", "daemon.sock") });
    expect(nextCapture.fleet.sources).toHaveLength(1);
    expect(nextCapture.fleet.sources[0]).toMatchObject({ controllerId: fleetSource, stale: false, hosts: [] });
    expect(nextCapture.args.slice(-2)[0]).toBe("--");
    expect(nextCapture.args.at(-1)).toContain("You are Relay Mastermind");
    expect(nextCapture.args.some((arg: string) => arg.includes("dangerously"))).toBeFalsy();
    const retained = await sessions();
    expect(retained).toHaveLength(2);
    expect(retained.find((session: { id: string }) => session.id === first.id)).toMatchObject({ id: first.id, createdAt: first.createdAt, mastermindId: first.mastermindId, status: "running" });
    expect((await readFile(join(runtimeDir, "agent-launches"), "utf8")).trim().split("\n")).toEqual([first.id, second.id]);
    expect(mutations).toEqual(["/api/mastermind/config", "/api/mastermind/config"]);
    await page.screenshot({ path: testInfo.outputPath("live-mastermind.png"), fullPage: true });
    expect(errors).toEqual([]);
  } finally {
    await stop(controller);
    await stop(daemon);
    await rm(temporary, { recursive: true, force: true });
  }
});

test("real previous runtime: all Mastermind provider modes preserve existing terminals and recover the exact worker", async ({ page, context }, testInfo) => {
  const previous = process.env.RELAY_LEGACY_BINARY;
  test.skip(!binary || !previous, "Set RELAY_LIVE_BINARY and RELAY_LEGACY_BINARY to the current CLI and verified 50e9 runtime.");
  test.setTimeout(180000);
  const executable = resolve(binary!);
  // This immutable public artifact predates Mastermind. Verify it rather than
  // treating a mock response or a new daemon with a hidden flag as legacy proof.
  const legacyBytes = await readFile(resolve(previous!));
  expect(createHash("sha256").update(legacyBytes).digest("hex")).toBe("f3483628e68655cfae23beaf8f0cfd752ee11035708c8564563d1bf85bbbf31f");
  const temporary = await mkdtemp(join(tmpdir(), "relay-mastermind-previous-"));
  const runtimeDir = join(temporary, "runtime");
  const stateDir = join(temporary, "controller");
  const projectDir = join(temporary, "literal project ' $() [previous]");
  const legacy = join(temporary, "previous-relay");
  const socket = join(runtimeDir, "run", "daemon.sock");
  let daemon: ChildProcess | undefined;
  let controller: ChildProcess | undefined;
  const mutations: string[] = [];
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  context.on("request", (request) => {
    const path = new URL(request.url()).pathname;
    if (request.method() !== "GET" && path.startsWith("/api/mastermind")) mutations.push(path);
  });
  const cli = async (...args: string[]) => (await promisify(execFile)(executable, ["session", ...args, "--socket", socket], { timeout: 20000, maxBuffer: 2 << 20 })).stdout;
  try {
    await copyFile(resolve(previous!), legacy);
    await chmod(legacy, 0o700);
    expect((await promisify(execFile)(legacy, ["--version"])).stdout.trim()).toContain("0.1.0-dev.50e9fc609217");
    await mkdir(join(runtimeDir, "bin"), { recursive: true, mode: 0o700 });
    await mkdir(projectDir, { mode: 0o700 });
    for (const harness of ["claude", "codex"]) await writeFile(join(runtimeDir, "bin", harness), providerFixture, { mode: 0o700 });
    daemon = spawn(legacy, ["daemon", "--state-dir", runtimeDir], { stdio: "ignore" });
    await expect.poll(() => existsSync(socket)).toBeTruthy();
    const daemonPID = daemon.pid;
    const ordinary = JSON.parse(await cli("start", "--harness", "shell", "--cwd", projectDir, "--title", "Ordinary terminal before Mastermind"));
    await cli("send", "--id", ordinary.id, "--text", "printf 'RELAY_ORDINARY_%s\\n' before", "--enter");
    await expect.poll(() => cli("read", "--id", ordinary.id)).toContain("RELAY_ORDINARY_before");
    let launch = await startController(executable, stateDir, runtimeDir);
    controller = launch.child;
    await page.goto(launch.url);
    const card = page.getByRole("region", { name: "Mastermind", exact: true });
    const state = async () => (await page.request.get(`${launch.origin}/api/mastermind`)).json();
    const sessions = async () => JSON.parse(await cli("list")) as Array<{ id: string; createdAt: string; status: string; harness: string; agentMode?: string; mastermindId?: string }>;
    const health = await (await page.request.get(`${launch.origin}/api/hosts/local/runtime/health`)).json();
    expect(health.version).toBe("0.1.0-dev.50e9fc609217");
    expect(health.capabilities?.mastermindV1).not.toBe(true);
    const fleetSource = JSON.parse(await readFile(join(stateDir, "fleet-source.json"), "utf8")).id;
    await expect.poll(() => existsSync(join(runtimeDir, "fleet", `${fleetSource}.json`))).toBeTruthy();

    // Fresh discovery may report an installed provider on an older daemon.
    // Unsupported Auto flags must fail preflight without even creating a shell.
    await writeFile(join(runtimeDir, "disable-auto"), "fixture preflight failure");
    await card.getByRole("button", { name: "Set up Mastermind", exact: true }).click();
    let dialog = page.getByRole("dialog", { name: "Set up Mastermind", exact: true });
    await dialog.getByLabel("Machine", { exact: true }).selectOption("local");
    await dialog.getByRole("radio", { name: "Claude Code", exact: true }).check();
    await dialog.getByRole("radio", { name: "Auto", exact: true }).check();
    await dialog.getByLabel("Project folder").fill(projectDir);
    let configured = page.waitForResponse((response) => response.url().endsWith("/api/mastermind/config") && response.request().method() === "POST");
    await dialog.getByRole("button", { name: "Save and start Mastermind", exact: true }).click();
    expect((await configured).status()).toBe(202);
    await expect.poll(async () => (await state()).status, { timeout: 20000 }).toBe("blocked");
    expect((await state()).blocker).toBe("worker");
    expect(await sessions()).toHaveLength(1);
    expect(existsSync(join(runtimeDir, "agent-launches"))).toBe(false);
    await expect(card.getByRole("button", { name: "Start Mastermind", exact: true })).toHaveCount(0);
    const blockedRevision = (await state()).revision;
    await rm(join(runtimeDir, "disable-auto"));
    const checked = page.waitForResponse((response) => response.url().endsWith("/api/mastermind/start") && response.request().method() === "POST");
    await card.getByRole("button", { name: "Check setup", exact: true }).click();
    expect((await checked).status()).toBe(200);

    const identities: Array<{ id: string; createdAt: string }> = [];
    const modes = [{ harness: "claude", mode: "auto" }, { harness: "claude", mode: "manual" }, { harness: "codex", mode: "manual" }, { harness: "codex", mode: "auto" }] as const;
    for (const [index, selection] of modes.entries()) {
      if (index > 0) {
        await card.getByRole("button", { name: "Mastermind settings", exact: true }).click();
        dialog = page.getByRole("dialog", { name: "Mastermind settings", exact: true });
        await dialog.getByRole("radio", { name: selection.harness === "claude" ? "Claude Code" : "Codex", exact: true }).check();
        await dialog.getByRole("radio", { name: selection.mode === "auto" ? "Auto" : "Manual", exact: true }).check();
        configured = page.waitForResponse((response) => response.url().endsWith("/api/mastermind/config") && response.request().method() === "POST");
        await dialog.getByRole("button", { name: "Save and start Mastermind", exact: true }).click();
        expect((await configured).status()).toBe(202);
      }
      await expect.poll(async () => (await state()).status, { timeout: 20000 }).toBe("running");
      const current = await state();
      expect(current).toMatchObject({ execution: "worker", config: { harness: selection.harness, agentMode: selection.mode }, session: { harness: "shell", cwd: projectDir, status: "running" } });
      expect(current.session.agentMode || "").toBe("");
      expect(current.session.mastermindId || "").toBe("");
      if (index === 0) expect(current.revision).toBe(blockedRevision);
      const session = current.session;
      identities.push({ id: session.id, createdAt: session.createdAt });
      await expect.poll(() => existsSync(join(runtimeDir, `${session.id}.json`))).toBeTruthy();
      const capture = JSON.parse(await readFile(join(runtimeDir, `${session.id}.json`), "utf8"));
      expect(capture).toMatchObject({ sessionId: session.id, provider: selection.harness, mode: selection.mode, cwd: projectDir, tty: true, relayBin: executable, relaySocket: socket });
      expect(capture.fleet.sources).toHaveLength(1);
      expect(capture.fleet.sources[0]).toMatchObject({ controllerId: fleetSource, stale: false, hosts: [] });
      const flags = selection.harness === "claude" ? ["--dangerously-skip-permissions", "--append-system-prompt"] : ["--dangerously-bypass-approvals-and-sandbox", "--"];
      expect(capture.args).toHaveLength(selection.mode === "auto" ? 3 : 2);
      expect(capture.args.slice(0, -1)).toEqual(selection.mode === "auto" ? flags : [flags[1]]);
      expect(capture.args.at(-1)).toContain("You are Relay Mastermind");
      expect(capture.args.at(-1)).toContain('relay_cli="${RELAY_BIN:-$HOME/.local/share/relay/bin/relay}"');
      expect(capture.args.at(-1)).toContain("If no task has been provided, wait for the user to describe one.");
      expect(capture.args.at(-1).includes("Relay session mode: Auto")).toBe(selection.mode === "auto");
      await expect(card.getByRole("region", { name: "Mastermind terminal", exact: true })).toBeVisible();
      await expect(card.getByText("Watching", { exact: true })).toBeVisible();
      await expect(card.getByLabel(selection.mode === "auto" ? "Launch mode: Auto · full access" : "Launch mode: Manual", { exact: true })).toBeVisible();
      if (selection.mode === "manual") await expect(card).toContainText("approval cards are unavailable for this session");
      await card.getByRole("button", { name: "Take control", exact: true }).click();
      await expect(card.getByText("Controlling", { exact: true })).toBeVisible();
      await page.keyboard.type(`explicit-fixture-input-${index}`);
      await page.keyboard.press("Enter");
      await expect.poll(() => cli("read", "--id", session.id)).toContain(`RELAY_MASTERMIND_INPUT:explicit-fixture-input-${index}`);
      await page.reload();
      await expect(card.getByText("Watching", { exact: true })).toBeVisible();
      expect(await sessions()).toHaveLength(index + 2);
      expect(daemon.pid).toBe(daemonPID);
      expect(daemon.exitCode).toBeNull();
    }
    const launchesBeforeRestart = await readFile(join(runtimeDir, "agent-launches"), "utf8");
    expect(launchesBeforeRestart.trim().split("\n")).toEqual(identities.map((item) => item.id));
    // A second view and a new controller must reconcile the exact shell and
    // receipt. Neither is authorization to resend the exec command.
    const secondViewer = await context.newPage();
    await secondViewer.goto(launch.origin);
    await expect(secondViewer.getByRole("region", { name: "Mastermind terminal", exact: true })).toBeVisible();
    await secondViewer.close();
    await stop(controller);
    launch = await startController(executable, stateDir, runtimeDir);
    controller = launch.child;
    await page.goto(launch.url);
    await expect(card.getByText("Watching", { exact: true })).toBeVisible();
    expect((await state()).session).toMatchObject({ ...identities.at(-1), harness: "shell", status: "running" });
    expect(await readFile(join(runtimeDir, "agent-launches"), "utf8")).toBe(launchesBeforeRestart);
    await cli("send", "--id", ordinary.id, "--text", "printf 'RELAY_ORDINARY_%s\\n' after", "--enter");
    await expect.poll(() => cli("read", "--id", ordinary.id)).toContain("RELAY_ORDINARY_after");
    expect(await cli("read", "--id", ordinary.id)).toContain("RELAY_ORDINARY_before");
    const retained = await sessions();
    expect(retained).toHaveLength(5);
    expect(retained.find((item) => item.id === ordinary.id)).toMatchObject({ id: ordinary.id, createdAt: ordinary.createdAt, status: "running", harness: "shell" });
    for (const identity of identities) expect(retained.find((item) => item.id === identity.id)).toMatchObject({ ...identity, status: "running", harness: "shell" });
    expect(daemon.pid).toBe(daemonPID);
    expect(daemon.exitCode).toBeNull();
    expect((await (await page.request.get(`${launch.origin}/api/hosts/local/runtime/health`)).json()).version).toBe("0.1.0-dev.50e9fc609217");
    expect(mutations).toEqual(["/api/mastermind/config", "/api/mastermind/start", "/api/mastermind/config", "/api/mastermind/config", "/api/mastermind/config"]);
    expect(errors).toEqual([]);
    await page.screenshot({ path: testInfo.outputPath("live-mastermind-previous-runtime.png"), fullPage: true });
    await testInfo.attach("legacy-runtime-proof", { body: JSON.stringify({ version: health.version, daemonPID, ordinary: { id: ordinary.id, createdAt: ordinary.createdAt }, workerSessions: identities, modes, controllerRecovered: true }), contentType: "application/json" });
  } finally {
    await stop(controller);
    await stop(daemon);
    await rm(temporary, { recursive: true, force: true });
  }
});
