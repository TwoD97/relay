import { test, expect } from "@playwright/test";
import { execFile, spawn, type ChildProcess } from "node:child_process";
import { existsSync } from "node:fs";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
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
    const fixture = String.raw`#!/usr/bin/env python3
import json, os, pathlib, subprocess, sys
args = sys.argv[1:]
root = pathlib.Path(__file__).resolve().parent.parent
provider = pathlib.Path(__file__).name
if args == ["--version"]:
    print("fixture-1.0")
    sys.exit(0)
if args == ["--help"]:
    print("--dangerously-skip-permissions --append-system-prompt --dangerously-bypass-approvals-and-sandbox [PROMPT]")
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
    for (const harness of ["claude", "codex"]) await writeFile(join(runtimeDir, "bin", harness), fixture, { mode: 0o700 });
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
