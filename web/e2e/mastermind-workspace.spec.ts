import { expect, test, type Page } from "@playwright/test";
import { local, mockFleet, openNavigation, remote, work } from "./fixture";
import type { MastermindPage, MastermindState, MastermindWorkspaceState, Session } from "../src/types";

const now = "2026-11-03T08:00:00Z";
const id1 = "a".repeat(32), id2 = "b".repeat(32);
function initial() {
  const pages: MastermindPage[] = [{ id: id1, title: "Plan", html: "<h1>Release plan</h1><p>Review and ship.</p>", revision: 2, updatedAt: now }, { id: id2, title: "Architecture", html: "<h1>System map</h1>", revision: 2, updatedAt: now }];
  const workspace: MastermindWorkspaceState = { revision: 2, updatedAt: now, pages: pages.map(({ id, title, updatedAt }) => ({ id, title, updatedAt })), tasks: [] };
  return { pages, workspace };
}
function running() {
  const session: Session = { ...work, id: "coordinator", title: "Mastermind", harness: "codex", agentMode: "manual", mastermindId: "c".repeat(32) };
  const mastermind: MastermindState = { revision: 1, enabled: true, status: "running", stage: "Connected", config: { hostId: "dev", cwd: session.cwd, harness: "codex", agentMode: "manual" }, session, local: { hostId: "local", cwd: "/home/local/relay project" }, updatedAt: now };
  return { mastermind, sessions: { dev: [session], local: [] }, sessionModes: { codex: ["manual" as const, "auto" as const] } };
}
async function workspace(page: Page, view: "overview" | "pages" | "tasks" | "terminal" = "pages") {
  await page.goto(view === "overview" ? "/#mastermind" : `/#mastermind/${view}`);
  await expect(page.getByRole("heading", { name: "Mastermind", exact: true, level: 1 })).toBeVisible();
}
const views = (page: Page) => page.getByRole("navigation", { name: "Mastermind views", exact: true });

test("canvas pages and task changes share revisions without sending terminal commands", async ({ page }) => {
  const fixture = await mockFleet(page, { ...initial(), ...running() });
  await workspace(page);
  await expect(page.frameLocator('iframe[title="Plan canvas"]').getByRole("heading", { name: "Release plan" })).toBeVisible();
  await expect(page.locator(".xterm")).toHaveCount(0);
  expect(fixture.attachments).toEqual([]);
  await page.screenshot({ path: test.info().outputPath("mastermind-workspace.png"), fullPage: true });
  await views(page).getByRole("button", { name: "Tasks", exact: true }).click();
  await page.getByRole("button", { name: "New task", exact: true }).click();
  const task = page.getByRole("dialog", { name: "New task", exact: true });
  await task.getByLabel("Task title", { exact: true }).fill("Verify release");
  await task.getByLabel("Details", { exact: true }).fill("Check the rollback plan before shipping.");
  await task.getByLabel("Canvas page", { exact: true }).selectOption(id1);
  await task.getByRole("button", { name: "Save task", exact: true }).click();
  await expect(task).toHaveCount(0);
  const accessibleStatus = page.locator(".workspace-task .sr-only").filter({ hasText: "Status for Verify release" });
  await expect(accessibleStatus).toHaveCSS("position", "absolute");
  await expect(accessibleStatus).toHaveCSS("clip", "rect(0px, 0px, 0px, 0px)");
  await page.getByLabel("Status for Verify release", { exact: true }).selectOption("doing");
  await expect.poll(() => fixture.workspace.tasks[0]?.status).toBe("doing");
  expect(fixture.mutations[1].body).toMatchObject({ revision: 3, title: "Verify release", detail: "Check the rollback plan before shipping.", status: "doing", pageId: id1 });
  expect(fixture.mutations[1].body).not.toHaveProperty("updatedAt");
  await views(page).getByRole("button", { name: "Pages", exact: true }).click();
  await page.getByRole("tab", { name: "Architecture", exact: true }).click();
  await expect(page.frameLocator('iframe[title="Architecture canvas"]').getByRole("heading", { name: "System map" })).toBeVisible();
  await page.getByRole("tab", { name: "Architecture", exact: true }).press("ArrowLeft");
  await expect(page.getByRole("tab", { name: "Plan", exact: true })).toHaveAttribute("aria-selected", "true");
  await expect(page.locator(".xterm")).toHaveCount(0);
  expect(fixture.attachments).toEqual([]);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
  await page.getByRole("button", { name: "Delete canvas page", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Delete page", exact: true }).click();
  await expect(page.getByRole("tab", { name: "Plan", exact: true })).toHaveCount(0);
  expect(fixture.workspace.tasks[0].pageId).toBeUndefined();
  expect(fixture.sessions.dev).toHaveLength(1);
});

test("new canvas HTML is stored through explicit CAS and safely rendered", async ({ page }) => {
  const fixture = await mockFleet(page);
  await workspace(page);
  await page.getByRole("button", { name: "Create a page", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "New canvas page", exact: true });
  await dialog.getByLabel("Page title", { exact: true }).fill("Milestones");
  await dialog.getByLabel("Page HTML", { exact: true }).fill('<h1 style="color: rgb(20, 80, 30)">Milestone one</h1><table><tbody><tr><td>Ready</td></tr></tbody></table>');
  await dialog.getByRole("button", { name: "Save page", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(page.frameLocator('iframe[title="Milestones canvas"]').getByRole("heading", { name: "Milestone one" })).toHaveCSS("color", "rgb(20, 80, 30)");
  expect(fixture.mutations).toHaveLength(1);
  expect(fixture.mutations[0]).toMatchObject({ method: "POST", path: "/api/mastermind/workspace/pages", body: { revision: 0, title: "Milestones" } });
  await page.reload();
  await expect(page.frameLocator('iframe[title="Milestones canvas"]').getByText("Ready", { exact: true })).toBeVisible();
  expect(fixture.mutations).toHaveLength(1);
});

test("canvas isolates hostile HTML, navigation, native IPC, and every external asset", async ({ page }) => {
  const state = initial();
  state.pages[0].html = `<h1>Safe content</h1><script>parent.window.HACKED=true;parent.__TAURI_INTERNALS__.invoke('frame_action',{action:'Quit'})</script><img src="https://canvas.invalid/image" onerror="parent.window.HACKED=true"><style>@import url('https://canvas.invalid/style'); p {background:url(https://canvas.invalid/css)}</style><p style="background:url(https://canvas.invalid/inline)">Text remains</p><iframe src="https://canvas.invalid/frame"></iframe><object data="https://canvas.invalid/object"></object><meta http-equiv="refresh" content="0;url=https://canvas.invalid/nav"><a href="https://canvas.invalid/link" target="_top">Click me</a><form action="https://canvas.invalid/form"><input name="token"><button>Send</button></form><svg><a href="javascript:alert(1)">svg</a></svg>`;
  const requests: string[] = [];
  const blocked: string[] = [];
  const network: string[] = [];
  await page.route("https://canvas.invalid/**", (route) => { network.push(route.request().url()); return route.abort(); });
  page.on("requestfailed", (request) => { if (request.url().includes("canvas.invalid")) blocked.push(request.failure()?.errorText ?? ""); });
  page.on("request", (request) => { if (request.url().includes("canvas.invalid")) requests.push(request.url()); });
  const fixture = await mockFleet(page, state);
  await workspace(page);
  const iframe = page.locator('iframe[title="Plan canvas"]');
  await expect(iframe).toHaveAttribute("sandbox", "");
  const frame = page.frameLocator('iframe[title="Plan canvas"]');
  await expect(frame.getByRole("heading", { name: "Safe content" })).toBeVisible();
  await expect(frame.locator("script,iframe,object,form,input,a,svg,meta[http-equiv=refresh]")).toHaveCount(0);
  await expect(frame.locator("body")).toContainText("Click me");
  await frame.getByRole("heading", { name: "Safe content", exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`#mastermind/pages(?:/${id1})?$`));
  await expect.poll(() => page.evaluate(() => (window as unknown as { HACKED?: boolean }).HACKED)).toBeUndefined();
  const body = frame.locator("body");
  expect(await body.evaluate(() => { try { return window.parent.document.title; } catch { return "isolated"; } })).toBe("isolated");
  // Chromium reports CSP-denied resource attempts as request events, even
  // though they never reach routing/the network. Every attempt must fail CSP.
  await expect.poll(() => blocked.length).toBe(requests.length);
  expect(blocked.every((reason) => reason === "csp")).toBe(true);
  expect(network).toEqual([]);
  expect(fixture.mutations).toEqual([]);
});

test("unknown workspace write is not replayed after navigation or reload", async ({ page }) => {
  const fixture = await mockFleet(page, initial());
  let writes = 0;
  await page.route("**/api/mastermind/workspace/tasks", (route) => { writes++; return route.abort("failed"); });
  await workspace(page, "tasks");
  await page.getByRole("button", { name: "New task", exact: true }).click();
  await page.getByLabel("Task title", { exact: true }).fill("One uncertain task");
  await page.getByRole("button", { name: "Save task", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("has not been repeated");
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await views(page).getByRole("button", { name: "Pages", exact: true }).click();
  await expect(page.getByRole("button", { name: "New page", exact: true })).toBeDisabled();
  await views(page).getByRole("button", { name: "Tasks", exact: true }).click();
  await page.reload();
  await expect(page.getByText("The previous change is unconfirmed.", { exact: false })).toBeVisible();
  await expect(page.getByRole("button", { name: "New task", exact: true })).toBeDisabled();
  expect(writes).toBe(1);
  expect(fixture.workspace.tasks).toEqual([]);
});

test("a stale editor retains its draft and never overwrites an agent update", async ({ page }) => {
  const fixture = await mockFleet(page, initial());
  await workspace(page);
  await page.getByRole("button", { name: "Edit HTML", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Edit canvas page", exact: true });
  await dialog.getByLabel("Page HTML", { exact: true }).fill("<p>My unsaved draft</p>");
  fixture.workspace.revision++;
  await expect(dialog).toContainText("workspace changed");
  await expect(dialog.getByRole("button", { name: "Save page", exact: true })).toBeDisabled();
  await expect(dialog.getByLabel("Page HTML", { exact: true })).toHaveValue("<p>My unsaved draft</p>");
  expect(fixture.mutations).toEqual([]);
});

test("new Mastermind chooses this computer and never substitutes the first remote", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [structuredClone(remote), structuredClone(local)], mastermind: { revision: 0, enabled: false, status: "unconfigured", stage: "Choose", local: { hostId: "local", cwd: "C:\\Users\\fixture\\Relay project" }, updatedAt: now } });
  await workspace(page, "overview");
  await page.getByRole("button", { name: "Set up Mastermind", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Set up Mastermind", exact: true });
  await expect(dialog.getByLabel("Machine", { exact: true })).toHaveValue("local");
  await expect(dialog.getByLabel("Project folder", { exact: true })).toHaveValue("C:\\Users\\fixture\\Relay project");
  await dialog.getByRole("radio", { name: "Codex", exact: true }).check();
  await dialog.getByRole("button", { name: "Save and start Mastermind", exact: true }).click();
  expect(fixture.mutations[0].body).toMatchObject({ hostId: "local", cwd: "C:\\Users\\fixture\\Relay project" });
});

test("an unavailable local service stays explicit and switching an existing remote preserves its session", async ({ page }) => {
  const fixture = await mockFleet(page, { ...running(), hosts: [structuredClone(remote)] });
  await workspace(page, "overview");
  await page.getByRole("button", { name: "Switch to local", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Mastermind settings", exact: true });
  await expect(dialog.getByLabel("Machine", { exact: true })).toHaveValue("local");
  await expect(dialog).toContainText("agent service is not available");
  await expect(dialog.getByRole("button", { name: "Save and start Mastermind", exact: true })).toBeDisabled();
  expect(fixture.mutations).toEqual([]);
  expect(fixture.sessions.dev).toHaveLength(1);
});

test("native terminal window requests are scoped and a rejected request has no browser replay", async ({ page }) => {
  await page.addInitScript(() => {
    const target = window as unknown as { __TAURI_INTERNALS__: { invoke: (command: string, args: unknown) => Promise<unknown> }; nativeRequests: unknown[]; browserRequests: unknown[] };
    target.nativeRequests = []; target.browserRequests = [];
    target.__TAURI_INTERNALS__ = { invoke: async (command, args) => { if (command === "appearance") return { preference: "system", resolved: "light" }; target.nativeRequests.push({ command, args }); throw new Error("Native window outcome unavailable"); } };
    window.open = (...args) => { target.browserRequests.push(args); return null; };
  });
  const fixture = await mockFleet(page);
  await page.goto("/#host/dev/session/session-1");
  await page.getByRole("button", { name: "Open terminal in new window", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("has not been repeated");
  expect(await page.evaluate(() => (window as unknown as { nativeRequests: unknown[] }).nativeRequests)).toEqual([{ command: "open_session_window", args: { hostId: "dev", sessionId: "session-1" } }]);
  expect(await page.evaluate(() => (window as unknown as { browserRequests: unknown[] }).browserRequests)).toEqual([]);
  expect(fixture.mutations).toEqual([]);
});

test("browser window fallback opens only the same session route without starting a session", async ({ page }) => {
  await page.addInitScript(() => { (window as unknown as { opened: unknown[] }).opened = []; window.open = (...args) => { (window as unknown as { opened: unknown[] }).opened.push(args); return null; }; });
  const fixture = await mockFleet(page);
  await page.goto("/#host/dev/session/session-1");
  await page.getByRole("button", { name: "Open terminal in new window", exact: true }).click();
  const opened = await page.evaluate(() => (window as unknown as { opened: string[][] }).opened);
  expect(new URL(opened[0][0]).hash).toBe("#host/dev/session/session-1/window");
  expect(new URL(opened[0][0]).origin).toBe(new URL(page.url()).origin);
  expect(opened[0].slice(1)).toEqual(["_blank", "noopener,noreferrer"]);
  expect(fixture.mutations).toEqual([]);
});

test("attention arrivals keep the machine tree fixed and all notifications reachable", async ({ page }) => {
  const fixture = await mockFleet(page);
  await page.goto("/");
  await openNavigation(page);
  const tree = page.getByRole("navigation", { name: "Machines and workspaces", exact: true });
  const before = await tree.boundingBox();
  fixture.sessions.dev = Array.from({ length: 6 }, (_, index) => ({ ...work, id: `attention-${index}`, title: `Notification ${index}`, attention: { kind: "notification" as const, source: "claude-hook" as const, updatedAt: now } }));
  await expect(page.getByRole("region", { name: "Needs attention", exact: true }).getByRole("button")).toHaveCount(6);
  const after = await tree.boundingBox();
  expect(after!.y).toBe(before!.y);
  await page.getByRole("region", { name: "Needs attention", exact: true }).getByRole("button").last().scrollIntoViewIfNeeded();
  await expect(page.getByRole("region", { name: "Needs attention", exact: true }).getByRole("button").last()).toBeVisible();
  expect(fixture.mutations).toEqual([]);
});

test("a late page read cannot replace the selected page or attach a terminal", async ({ page }) => {
  const fixture = await mockFleet(page, { ...initial(), ...running() });
  let release!: () => void;
  let requested = false;
  let cancelled = false;
  page.on("requestfailed", (request) => { if (request.url().endsWith(`/pages/${id1}`)) cancelled = true; });
  const paused = new Promise<void>((resolve) => { release = resolve; });
  await page.route(`**/api/mastermind/workspace/pages/${id1}`, async (route) => { requested = true; await paused; await route.fulfill({ json: fixture.pages[0] }).catch(() => {}); });
  await workspace(page);
  await expect.poll(() => requested).toBe(true);
  await page.getByRole("tab", { name: "Architecture", exact: true }).click();
  await expect(page.frameLocator('iframe[title="Architecture canvas"]').getByRole("heading", { name: "System map" })).toBeVisible();
  await expect.poll(() => cancelled).toBe(true);
  release();
  await expect(page.getByRole("tab", { name: "Architecture", exact: true })).toHaveAttribute("aria-selected", "true");
  await expect(page.locator('iframe[title="Plan canvas"]')).toHaveCount(0);
  expect(fixture.attachments).toEqual([]);
  expect(fixture.mutations).toEqual([]);
});

test("a canvas page read times out and only explicit reload retries it", async ({ page }) => {
  const fixture = await mockFleet(page, { ...initial(), ...running() });
  let reads = 0;
  let release!: () => void;
  const paused = new Promise<void>((resolve) => { release = resolve; });
  await page.route(`**/api/mastermind/workspace/pages/${id1}`, async (route) => {
    reads++;
    if (reads !== 1) { await route.fallback(); return; }
    await paused;
    await route.fulfill({ json: { ...fixture.pages[0], html: "<h1>Too late</h1>" } }).catch(() => {});
  });
  await workspace(page);
  await expect(page.getByText("Loading canvas page…", { exact: true })).toBeVisible();
  await expect(page.getByText("The request timed out. Check the machine’s connection and try again.", { exact: true })).toBeVisible({ timeout: 11000 });
  await expect(page.getByText("Loading canvas page…", { exact: true })).toHaveCount(0);
  expect(reads).toBe(1);
  release();
  await page.getByRole("button", { name: "Reload page", exact: true }).click();
  await expect(page.frameLocator('iframe[title="Plan canvas"]').getByRole("heading", { name: "Release plan" })).toBeVisible();
  expect(reads).toBe(2);
  await expect(page.locator(".xterm")).toHaveCount(0);
  expect(fixture.attachments).toEqual([]);
  expect(fixture.mutations).toEqual([]);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
});

test("a late page save cannot reopen its editor or steal a changed page selection", async ({ page }) => {
  const fixture = await mockFleet(page, initial());
  let release!: () => void;
  let requested = false;
  const paused = new Promise<void>((resolve) => { release = resolve; });
  await page.route("**/api/mastermind/workspace/pages", async (route) => {
    requested = true; await paused;
    const saved = { id: "d".repeat(32), title: "Delayed", html: "<p>Saved later</p>", updatedAt: now, revision: 3 };
    fixture.workspace.revision = 3; fixture.workspace.pages.push(saved); fixture.pages.push(saved);
    await route.fulfill({ json: { workspace: fixture.workspace, page: saved } });
  });
  await workspace(page);
  await page.getByRole("button", { name: "New page", exact: true }).click();
  await page.getByLabel("Page title", { exact: true }).fill("Delayed");
  await page.getByRole("button", { name: "Save page", exact: true }).click();
  await expect.poll(() => requested).toBe(true);
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.getByRole("tab", { name: "Architecture", exact: true }).click();
  release();
  await expect(page.getByRole("tab", { name: "Delayed", exact: true })).toBeVisible();
  await expect(page.getByRole("tab", { name: "Architecture", exact: true })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByRole("dialog")).toHaveCount(0);
});

test("switching an existing remote coordinator to local is explicit and preserves its old PTY", async ({ page }) => {
  const fixture = await mockFleet(page, running());
  const previous = structuredClone(fixture.sessions.dev[0]);
  await workspace(page, "overview");
  await page.getByRole("button", { name: "Switch to local", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Mastermind settings", exact: true });
  await expect(dialog.getByLabel("Machine", { exact: true })).toHaveValue("local");
  await expect(dialog.getByLabel("Project folder", { exact: true })).toHaveValue("/home/local/relay project");
  await dialog.getByRole("button", { name: "Save and start Mastermind", exact: true }).click();
  expect(fixture.mutations).toHaveLength(1);
  expect(fixture.mutations[0].body).toMatchObject({ hostId: "local", harness: "codex", agentMode: "manual" });
  fixture.attachMastermind();
  await expect(page.getByRole("button", { name: "Open terminal", exact: true })).toBeVisible();
  expect(fixture.attachments).toEqual([]);
  expect(fixture.sessions.dev).toEqual([previous]);
  expect(fixture.sessions.local).toHaveLength(1);
});

test("resuming after an uncertain write requires a fresh read and explicit acknowledgment, never a retry", async ({ page }) => {
  await mockFleet(page, initial());
  let writes = 0;
  let requestedId = "";
  const failedWrite = async (route: import("@playwright/test").Route) => { writes++; requestedId = route.request().postDataJSON().id; await route.abort("failed"); };
  await page.route("**/api/mastermind/workspace/tasks", failedWrite);
  await workspace(page, "tasks");
  await page.getByRole("button", { name: "New task", exact: true }).click();
  await page.getByLabel("Task title", { exact: true }).fill("Unconfirmed draft");
  const failedRead = async (route: import("@playwright/test").Route) => route.fulfill({ status: 503, json: { error: "Workspace offline" } });
  await page.route("**/api/mastermind/workspace", failedRead);
  await page.getByRole("button", { name: "Save task", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("has not been repeated");
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  expect(requestedId).toMatch(/^[a-f0-9]{32}$/);
  await expect(page.getByText(`Requested task ID: ${requestedId}`, { exact: true })).toBeVisible();
  const resume = page.getByRole("button", { name: "Review and resume editing", exact: true });
  await expect(resume).toBeDisabled();
  await page.unroute("**/api/mastermind/workspace", failedRead);
  await page.getByRole("button", { name: "Refresh Mastermind workspace", exact: true }).click();
  await expect(resume).toBeEnabled();
  await resume.click();
  const confirm = page.getByRole("dialog", { name: "Resume workspace editing?", exact: true });
  await expect(confirm).toContainText("previous write may still complete");
  await confirm.getByRole("button", { name: "Resume editing", exact: true }).click();
  await expect(page.getByRole("button", { name: "New task", exact: true })).toBeEnabled();
  expect(writes).toBe(1);
  await page.reload();
  await expect(page.getByRole("button", { name: "New task", exact: true })).toBeEnabled();
  expect(writes).toBe(1);
});

test("native Windows local service offers Mastermind rather than unsupported ordinary sessions", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [structuredClone(local)], sessions: { local: [] } });
  await page.route("**/api/hosts/local/runtime/health", (route) => route.fulfill({ json: { version: "fixture", protocol: 1, platform: "windows", capabilities: { localMastermindV1: true } } }));
  await page.goto("/#host/local");
  await expect(page.locator(".topbar").getByRole("button", { name: "Open Mastermind", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "New session", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Start a session", exact: true })).toHaveCount(0);
  await expect(page.getByText("This computer runs your local Mastermind.", { exact: false })).toBeVisible();
  await page.locator(".topbar").getByRole("button", { name: "Open Mastermind", exact: true }).click();
  await expect(page).toHaveURL(/#mastermind$/);
  expect(fixture.mutations).toEqual([]);
});
