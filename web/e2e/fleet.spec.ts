import { expect, test } from "@playwright/test";
import { mockFleet, remote, work } from "./fixture";
import type { Approval, ObserverState, Session, SessionSummary } from "../src/types";

const agent: Session = { ...work, harness: "codex" };
const observer = (enabled = false): ObserverState => ({ config: { enabled, provider: "claude", model: "haiku", intervalSeconds: 300 }, running: false, summaries: [], limits: { requestsPerHour: 60, remaining: 59 }, providers: [{ id: "claude", supported: true }, { id: "codex", supported: false }] });
const summary = (overrides: Partial<SessionSummary> = {}): SessionSummary => ({ sessionId: agent.id, sessionCreatedAt: agent.createdAt, provider: "claude", model: "haiku", status: "ready", summary: "The parser change has tests in progress.", steps: ["Reviewed the parser entry point"], nextSteps: ["Run the integration checks"], blockers: ["A fixture may need updating"], sampledAt: "2026-10-12T08:00:00Z", generatedAt: "2026-10-12T08:00:03Z", updatedAt: "2026-10-12T08:00:03Z", sourceStatus: "running", stale: false, ...overrides });
const approval = (overrides: Partial<Approval> = {}): Approval => ({ id: "request-1", sessionId: agent.id, sessionCreatedAt: agent.createdAt, provider: "codex", toolName: "Bash", input: { command: "npm test" }, cwd: agent.cwd, createdAt: new Date().toISOString(), expiresAt: new Date(Date.now() + 600000).toISOString(), status: "pending", ...overrides });
const progress = (page: import("@playwright/test").Page) => page.getByRole("region", { name: "Sessions and progress", exact: true });
const metric = (page: import("@playwright/test").Page, label: string) => page.locator(".metric").filter({ hasText: label }).locator(".metric-value");

test("three connected machines with no sessions offer real next actions without inventing work", async ({ page }, testInfo) => {
  const fixture = await mockFleet(page, { hosts: [{ ...remote }, { ...remote, id: "build", name: "Build machine" }, { ...remote, id: "lab", name: "Lab" }], sessions: { dev: [], build: [], lab: [] }, approvals: { dev: [], build: [], lab: [] }, observers: { dev: observer(), build: observer(), lab: observer() } });
  await page.goto("/");
  await expect(progress(page).getByRole("heading", { name: "No running sessions", exact: true })).toBeVisible();
  await expect(metric(page, "Machines online")).toContainText("03");
  await expect(metric(page, "Processes running")).toContainText("00");
  await expect(page.getByText("Summaries off", { exact: true })).toHaveCount(3);
  await expect(page.getByRole("region", { name: "Your machines", exact: true }).getByRole("button", { name: "New session", exact: true })).toHaveCount(3);
  await expect(page.getByText("Codex signed in", { exact: true })).toHaveCount(3);
  expect(fixture.mutations).toEqual([]);
  expect(fixture.attachments).toEqual([]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBeTruthy();
  await page.screenshot({ path: testInfo.outputPath("fleet-three-empty.png"), fullPage: true });
  const machine = page.getByRole("article", { name: "Build machine machine", exact: true });
  await machine.getByRole("button", { name: "New session", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("Build machine");
  expect(fixture.mutations).toEqual([]);
});

test("fleet shows scoped advisory context with steps, blockers and direct terminal navigation", async ({ page }, testInfo) => {
  const first = observer(true); first.summaries = [summary()];
  const second = observer(true); second.summaries = [summary({ summary: "Other machine's distinct context", nextSteps: ["Do not mix hosts"], blockers: [], stale: true })];
  const fixture = await mockFleet(page, { hosts: [{ ...remote }, { ...remote, id: "other", name: "Other machine" }], sessions: { dev: [{ ...agent }], other: [{ ...agent }] }, observers: { dev: first, other: second }, approvals: { dev: [], other: [] } });
  await page.goto("/");
  const card = progress(page).getByRole("article", { name: `Development: ${agent.title}`, exact: true });
  await expect(card).toContainText("Process running");
  await expect(card).toContainText("Model-generated snapshot");
  await expect(card).toContainText("Possible blockers");
  await expect(card).toContainText("Suggested next steps");
  await expect(card).toContainText("Run the integration checks");
  await card.getByText("Reported steps · 1", { exact: true }).click();
  await expect(card).toContainText("Reviewed the parser entry point");
  await expect(card).toContainText("Source: running");
  await expect(card).not.toContainText("Do not mix hosts");
  await expect(progress(page).getByRole("article", { name: `Other machine: ${agent.title}`, exact: true })).toContainText("Earlier snapshot · stale");
  expect(await card.getByRole("checkbox").count()).toBe(0);
  expect(fixture.mutations).toEqual([]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBeTruthy();
  await page.screenshot({ path: testInfo.outputPath("fleet-work-context.png"), fullPage: true });
  await card.getByRole("button", { name: "Open terminal", exact: true }).click();
  await expect(page).toHaveURL(/#host\/dev\/session\/session-1$/);
  expect(fixture.mutations).toEqual([]);
});

test("only current exact permission requests enter the inbox and reported turn completion is not task completion", async ({ page }) => {
  const completed = { ...agent, id: "turn-finished", title: "Another agent", attention: { kind: "completed" as const, source: "claude-hook" as const, updatedAt: new Date().toISOString() } };
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent }, completed] }, approvals: { dev: [approval(), approval({ id: "expired", expiresAt: "2020-01-01T00:00:00Z" }), approval({ id: "reused", sessionCreatedAt: "2020-01-01T00:00:00Z" })] } });
  await page.goto("/");
  const inbox = page.getByRole("region", { name: "Needs review", exact: true });
  await expect(inbox).toContainText("Bash needs approval");
  await expect(inbox).toContainText("Agent turn finished (reported)");
  await expect(metric(page, "Needs review")).toContainText("02");
  await expect(progress(page).getByRole("article", { name: `Development: ${agent.title}`, exact: true })).toContainText("Waiting for approval");
  await expect(inbox.getByRole("button", { name: "Review request", exact: true })).toHaveCount(1);
  await inbox.getByRole("button", { name: "Review request", exact: true }).click();
  await expect(page).toHaveURL(/#activity$/);
  expect(fixture.mutations).toEqual([]);
  expect(fixture.terminalMessages).toEqual([]);
});

test("unknown and partial machine data never appear as a confirmed empty fleet", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [{ ...remote }, { ...remote, id: "offline", name: "Offline machine", status: "disconnected" }], sessions: { dev: [] }, failHosts: ["dev"] });
  await page.goto("/");
  await expect(metric(page, "Processes running")).toContainText("—");
  await expect(progress(page)).toContainText("Waiting for session state");
  await expect(progress(page)).not.toContainText("No running sessions");
  await page.unroute("**/api/**");
  await mockFleet(page, { hosts: fixture.hosts, sessions: { dev: [] } });
  await expect(progress(page)).toContainText("No sessions found on connected machines");
  await expect(metric(page, "Processes running")).toContainText("00");
  await expect(metric(page, "Processes running")).toContainText("1 machine unverified");
  await expect(page.getByRole("article", { name: "Offline machine machine", exact: true }).getByRole("button", { name: "New session", exact: true })).toBeDisabled();
});

test("offline and failed controller reads retain context explicitly as stale", async ({ page }) => {
  const snapshots = observer(true); snapshots.summaries = [summary()];
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent }] }, observers: { dev: snapshots }, approvals: { dev: [] } });
  await page.goto("/");
  const card = progress(page).getByRole("article");
  await expect(card).toContainText("Model-generated snapshot");
  await page.route("**/api/state", (route) => route.fulfill({ status: 503, json: { error: "Controller unavailable" } }));
  await expect(metric(page, "Machines online")).toContainText("—");
  await expect(card).toContainText("Status unavailable");
  await expect(card).toContainText("Earlier snapshot · stale");
  await expect(page.getByRole("article", { name: "Development machine", exact: true }).getByRole("button", { name: "New session", exact: true })).toBeDisabled();
  await page.unroute("**/api/state");
  fixture.hosts[0].status = "disconnected";
  await expect(page.getByText("Summary state unavailable while offline", { exact: true })).toBeVisible();
  await expect(card).toContainText("The parser change has tests in progress");
  expect(fixture.mutations).toEqual([]);
});

test("summary configuration requires explicit opt-in and preserves machine identity", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent }] }, observers: { dev: observer() } });
  await page.goto("/");
  await progress(page).getByRole("button", { name: "Set up summaries", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Summary settings", exact: true });
  await expect(dialog.getByLabel("Enable session summaries on this machine")).not.toBeChecked();
  expect(fixture.mutations).toEqual([]);
  await dialog.getByLabel("Enable session summaries on this machine").check();
  await dialog.getByLabel("Summary model").fill("haiku");
  await dialog.getByRole("button", { name: "Save settings", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(fixture.mutations).toEqual([{ method: "POST", path: "/api/hosts/dev/runtime/observer/config", body: { enabled: true, provider: "claude", model: "haiku", intervalSeconds: 300 }, csrf: "fixture-csrf" }]);
  await expect(progress(page)).toContainText("No summary is available yet");
  expect(fixture.terminalMessages).toEqual([]);
});

test("saved session identity, setup exclusion and summary budget remain honest", async ({ page }) => {
  const snapshots = observer(true); snapshots.limits!.remaining = 0;
  snapshots.summaries = [summary({ sessionCreatedAt: "2020-01-01T00:00:00Z", summary: "Wrong lifetime must stay hidden" })];
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent, status: "exited", exitCode: 0 }, { ...work, id: "shell", title: "Inspect logs" }, { ...agent, id: "setup", title: "Private sign-in", purpose: "login" }] }, observers: { dev: snapshots } });
  await page.goto("/");
  await expect(progress(page).getByRole("article")).toHaveCount(2);
  await expect(progress(page)).not.toContainText("Private sign-in");
  await expect(progress(page)).not.toContainText("Wrong lifetime must stay hidden");
  await expect(progress(page)).toContainText("0 of 60 summary requests available this hour");
  await progress(page).getByRole("button", { name: "Saved output", exact: true }).click();
  await expect(progress(page).getByRole("article")).toHaveCount(1);
  await expect(progress(page)).toContainText("Exited · code 0");
  await progress(page).getByRole("button", { name: "Running", exact: true }).click();
  await expect(progress(page)).toContainText("Shell session");
  await expect(progress(page)).not.toContainText("Exited · code 0");
  expect(fixture.mutations).toEqual([]);
});

test("unsupported summaries and terminal-only permissions provide actionable context without inventing a task list", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent }] }, legacyRuntime: true });
  await page.goto("/");
  await expect(progress(page)).toContainText("Summaries need a runtime upgrade");
  await expect(progress(page)).toContainText("Its current sessions are preserved");
  await expect(page.getByRole("region", { name: "Needs review", exact: true })).toContainText("Development (terminal only)");
  await expect(progress(page).getByRole("button", { name: "Set up summaries", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Summary settings for Development", exact: true })).toHaveCount(0);
  expect(fixture.mutations).toEqual([]);
});

test("cached summary generation is not presented as live after scheduler or connection failure", async ({ page }) => {
  const snapshots = observer(true); snapshots.running = true;
  snapshots.summaries = [summary({ status: "running", summary: "", steps: [], nextSteps: [], blockers: [] })];
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent }] }, observers: { dev: snapshots } });
  await page.goto("/");
  const card = progress(page).getByRole("article");
  await expect(card).toContainText("Summarizing…");
  fixture.observers.dev.error = "Summary scheduler unavailable";
  await expect(card).toContainText("Summaries need attention");
  await expect(card).not.toContainText("Summarizing…");
  await expect(card).toContainText("Summary scheduler unavailable");
  fixture.hosts[0].status = "disconnected";
  await expect(card).toContainText("Summary state unavailable");
  await expect(card).not.toContainText("Summarizing…");
  expect(fixture.mutations).toEqual([]);
});
