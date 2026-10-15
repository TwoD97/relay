import { expect, test, type Page } from "@playwright/test";
import { mockFleet, openNavigation, remote, work } from "./fixture";
import type { MastermindState, Session } from "../src/types";

const folder = "/home/dev/projects/api service/$literal [draft]";
const config = { hostId: "dev", harness: "codex" as const, agentMode: "manual" as const, cwd: folder };
const coordinator: Session = { ...work, id: "coordinator", title: "Mastermind", cwd: folder, harness: "codex", agentMode: "manual", mastermindId: "0123456789abcdef0123456789abcdef" };
const configured = (): MastermindState => ({ revision: 3, enabled: true, config, session: structuredClone(coordinator), status: "running", stage: "Mastermind connected", updatedAt: "2026-10-15T08:00:00Z" });
const region = (page: Page) => page.getByRole("region", { name: "Mastermind", exact: true });
async function setup(page: Page) {
  await page.goto("/");
  await region(page).getByRole("button", { name: "Set up Mastermind", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Set up Mastermind", exact: true });
  await dialog.getByLabel("Machine", { exact: true }).selectOption("dev");
  await dialog.getByRole("radio", { name: "Codex", exact: true }).check();
  await dialog.getByLabel("Project folder").fill(folder);
  return dialog;
}
const fixtureOptions = () => ({ hosts: [structuredClone(remote)], sessions: { dev: [] as Session[] }, sessionModes: { codex: ["manual", "auto"] as ("manual" | "auto")[] } });

test("Mastermind setup is explicit and cancelling never starts an agent or summary", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "dark" });
  const fixture = await mockFleet(page, fixtureOptions());
  const dialog = await setup(page);
  await expect(dialog.getByRole("radio", { name: "Manual", exact: true })).toBeChecked();
  await expect(dialog.getByRole("radio", { name: "Terminal", exact: true })).toHaveCount(0);
  await expect(dialog).toContainText("normal permission settings");
  await expect(dialog).toContainText("waits for your instructions");
  await page.screenshot({ path: test.info().outputPath("mastermind-setup-dark.png"), fullPage: true });
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.reload();
  await expect(region(page).getByRole("button", { name: "Set up Mastermind", exact: true })).toBeVisible();
  expect(fixture.mutations).toEqual([]);
  expect(fixture.attachments).toEqual([]);
});

test("Manual setup attaches one exact persistent coordinator and reload only reads", async ({ page }) => {
  const fixture = await mockFleet(page, fixtureOptions());
  const dialog = await setup(page);
  await dialog.getByRole("button", { name: "Save and start Mastermind", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(region(page)).toContainText("Starting your coordinator");
  expect(fixture.mutations).toEqual([{ method: "POST", path: "/api/mastermind/config", body: { revision: 0, ...config }, csrf: "fixture-csrf" }]);
  const session = fixture.attachMastermind();
  await expect(region(page).getByRole("region", { name: "Mastermind terminal", exact: true })).toBeVisible();
  await expect(region(page)).toContainText("Watching");
  await expect(region(page).getByLabel("Launch mode: Manual", { exact: true })).toBeVisible();
  await page.getByLabel("Appearance", { exact: true }).selectOption("light");
  await page.screenshot({ path: test.info().outputPath("mastermind-running-light.png"), fullPage: true });
  await page.reload();
  await expect(region(page)).toContainText("Watching");
  expect(fixture.sessions.dev).toHaveLength(1);
  expect(fixture.mutations).toHaveLength(1);
  expect(fixture.attachments.every((path) => path.includes(`/sessions/${session.id}/terminal`))).toBe(true);
  expect(fixture.terminalMessages.filter((item) => item.type === "input")).toEqual([]);
  await region(page).getByRole("button", { name: "Open full terminal", exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`#host/dev/session/${session.id}$`));
  await page.getByRole("button", { name: "Workspace", exact: true }).click();
  await expect(region(page)).toContainText("Watching");
  expect(fixture.mutations).toHaveLength(1);
});

test("Auto is explicit and remains selected but blocked when fresh capability disappears", async ({ page }) => {
  const fixture = await mockFleet(page, fixtureOptions());
  const dialog = await setup(page);
  await dialog.getByRole("radio", { name: "Auto", exact: true }).check();
  await expect(dialog).toContainText("Auto does not grant sudo");
  const fail = async (route: import("@playwright/test").Route) => route.fulfill({ status: 503, json: { error: "Agent capabilities unavailable" } });
  await page.route("**/api/hosts/dev/runtime/harnesses", fail);
  await expect(dialog.getByRole("radio", { name: "Auto", exact: true })).toBeDisabled();
  await expect(dialog.getByRole("radio", { name: "Auto", exact: true })).toBeChecked();
  await expect(dialog.getByRole("button", { name: "Save and start Mastermind", exact: true })).toBeDisabled();
  expect(fixture.mutations).toEqual([]);
  await page.unroute("**/api/hosts/dev/runtime/harnesses", fail);
  await expect(dialog.getByRole("radio", { name: "Auto", exact: true })).toBeEnabled();
  await dialog.getByRole("button", { name: "Save and start Mastermind", exact: true }).click();
  expect(fixture.mutations[0].body).toEqual({ revision: 0, ...config, agentMode: "auto" });
  fixture.attachMastermind();
  await expect(region(page).getByLabel("Launch mode: Auto · full access", { exact: true })).toBeVisible();
});

test("older runtime blocks Auto and reports Mastermind upgrade without ordinary session fallback", async ({ page }) => {
  const fixture = await mockFleet(page, { ...fixtureOptions(), sessionModes: {} });
  const dialog = await setup(page);
  await expect(dialog.getByRole("radio", { name: "Auto", exact: true })).toBeDisabled();
  await dialog.getByRole("button", { name: "Save and start Mastermind", exact: true }).click();
  Object.assign(fixture.mastermind, { status: "blocked", stage: "Mastermind needs a runtime upgrade", error: "The running runtime does not support Mastermind. Existing sessions have been preserved.", updatedAt: new Date().toISOString() });
  await expect(region(page)).toContainText("Mastermind needs a runtime upgrade");
  await expect(region(page)).toContainText("Existing sessions have been preserved");
  expect(fixture.mutations.map((item) => item.path)).toEqual(["/api/mastermind/config"]);
  expect(fixture.sessions.dev).toEqual([]);
  expect(fixture.attachments).toEqual([]);
});

test("settings replace the coordinator without deleting old output; pause and resume keep the same session", async ({ page }) => {
  const fixture = await mockFleet(page, { ...fixtureOptions(), sessions: { dev: [structuredClone(coordinator)] }, mastermind: configured() });
  await page.goto("/");
  await expect(region(page)).toContainText("Watching");
  await region(page).getByRole("button", { name: "Mastermind settings", exact: true }).click();
  let dialog = page.getByRole("dialog", { name: "Mastermind settings", exact: true });
  await dialog.getByRole("button", { name: "Pause Mastermind", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(region(page)).toContainText("Its session and output are kept");
  await expect(region(page).getByRole("region", { name: "Mastermind terminal", exact: true })).toHaveCount(0);
  await expect(region(page).getByRole("button", { name: "Open full terminal", exact: true })).toHaveCount(0);
  expect(fixture.sessions.dev).toHaveLength(1);
  await region(page).getByRole("button", { name: "Resume Mastermind", exact: true }).click();
  await expect(region(page)).toContainText("Session running");
  expect(fixture.mastermind.session?.id).toBe(coordinator.id);
  await region(page).getByRole("button", { name: "Mastermind settings", exact: true }).click();
  dialog = page.getByRole("dialog", { name: "Mastermind settings", exact: true });
  await dialog.getByLabel("Project folder").fill("/home/dev/projects/relay");
  await dialog.getByRole("button", { name: "Save and start Mastermind", exact: true }).click();
  fixture.attachMastermind();
  await expect(region(page)).toContainText("/home/dev/projects/relay");
  expect(fixture.sessions.dev[0]).toEqual(coordinator);
  expect(fixture.sessions.dev).toHaveLength(2);
  expect(fixture.mutations.map((item) => item.path)).toEqual(["/api/mastermind/disable", "/api/mastermind/start", "/api/mastermind/config"]);
});

test("lost setup response stays locked until authoritative revision reconciliation; reads never replay", async ({ page }) => {
  const fixture = await mockFleet(page, fixtureOptions());
  let attempts = 0;
  await page.route("**/api/mastermind/config", async (route) => { attempts++; await route.abort("connectionreset"); });
  const dialog = await setup(page);
  await dialog.getByRole("button", { name: "Save and start Mastermind", exact: true }).click();
  await expect(dialog.getByRole("alert")).toContainText("action may have completed");
  await expect(dialog.getByRole("button", { name: "Save and start Mastermind", exact: true })).toBeDisabled();
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  await region(page).getByRole("button", { name: "Check Mastermind status", exact: true }).click();
  await expect(region(page)).toContainText("Relay has not repeated it");
  expect(attempts).toBe(1);
  Object.assign(fixture.mastermind, { revision: 1, config, enabled: true, status: "starting", updatedAt: new Date().toISOString() });
  fixture.attachMastermind();
  await expect(region(page)).toContainText("Watching");
  await expect(region(page).getByRole("button", { name: "Check Mastermind status", exact: true })).toHaveCount(0);
  await page.reload();
  await expect(region(page)).toContainText("Watching");
  expect(attempts).toBe(1);
});

test("explicit setup rejection can be corrected, while stale revision cannot overwrite settings", async ({ page }) => {
  const fixture = await mockFleet(page, fixtureOptions());
  let attempts = 0;
  await page.route("**/api/mastermind/config", async (route) => { if (++attempts === 1) return route.fulfill({ status: 422, json: { error: "Choose an accessible project folder." } }); await route.fallback(); });
  const dialog = await setup(page);
  await dialog.getByRole("button", { name: "Save and start Mastermind", exact: true }).click();
  await expect(dialog.getByRole("alert")).toContainText("accessible project folder");
  await expect(dialog.getByRole("button", { name: "Save and start Mastermind", exact: true })).toBeEnabled();
  Object.assign(fixture.mastermind, { revision: 2, config: { ...config, cwd: "/srv/other" }, enabled: true, status: "waiting", updatedAt: new Date().toISOString() });
  await expect(page.getByRole("dialog").getByRole("alert").filter({ hasText: "settings changed" })).toBeVisible();
  await expect(page.getByRole("dialog").getByRole("button", { name: "Save and start Mastermind", exact: true })).toBeDisabled();
  expect(attempts).toBe(1);
  expect(fixture.mutations).toEqual([]);
});

test("offline and mismatched session identity cannot acquire the coordinator terminal", async ({ page }) => {
  const fixture = await mockFleet(page, { ...fixtureOptions(), hosts: [{ ...remote, status: "disconnected" }], sessions: { dev: [structuredClone(coordinator)] }, mastermind: configured() });
  await page.goto("/");
  await expect(region(page)).toContainText("Machine offline");
  expect(fixture.attachments).toEqual([]);
  fixture.sessions.dev[0].createdAt = "2026-10-15T09:00:00Z";
  fixture.hosts[0].status = "online";
  await expect(region(page)).toContainText("does not match a current session");
  await expect(region(page).getByRole("button", { name: "Open full terminal", exact: true })).toHaveCount(0);
  expect(fixture.attachments).toEqual([]);
  fixture.sessions.dev[0] = structuredClone(coordinator);
  Object.assign(fixture.mastermind, { status: "uncertain", stage: "Checking the previous session identity", updatedAt: new Date().toISOString() });
  await expect(region(page)).toContainText("Waiting for the controller to verify");
  expect(fixture.attachments).toEqual([]);
  Object.assign(fixture.mastermind, { status: "running", updatedAt: new Date().toISOString() });
  await expect(region(page)).toContainText("Watching");
  expect(fixture.attachments).toHaveLength(1);
  expect(fixture.mutations).toEqual([]);
  expect(fixture.terminalMessages.filter((item) => item.type === "input" || item.type === "claim")).toEqual([]);
});

test("Mastermind state advances within the same RFC3339 fractional second", async ({ page }) => {
  const state = configured();
  state.status = "blocked"; state.session = undefined; state.stage = "Previous blocked state"; state.updatedAt = "2026-10-15T08:00:00.9Z";
  const fixture = await mockFleet(page, { ...fixtureOptions(), mastermind: state });
  // A mutation result is retained while the next fleet read is already in flight.
  await page.route("**/api/mastermind/start", async (route) => {
    Object.assign(state, { revision: 4, status: "starting", stage: "Starting your coordinator", updatedAt: "2026-10-15T08:00:00.9Z" });
    await route.fulfill({ json: state });
  });
  await page.goto("/");
  await region(page).getByRole("button", { name: "Start Mastermind", exact: true }).click();
  await expect(region(page)).toContainText("Starting your coordinator");
  const session = fixture.attachMastermind();
  Object.assign(state, { updatedAt: "2026-10-15T08:00:00.91Z" });
  await expect(region(page)).toContainText("Watching");
  expect(state.session?.id).toBe(session.id);
});

test("late setup response does not steal navigation or reopen its dialog", async ({ page }) => {
  const fixture = await mockFleet(page, fixtureOptions());
  let answer: (() => void) | undefined;
  await page.route("**/api/mastermind/config", async (route) => { await new Promise<void>((resolve) => { answer = resolve; }); await route.fallback(); });
  const dialog = await setup(page);
  await dialog.getByRole("button", { name: "Save and start Mastermind", exact: true }).click();
  await expect(dialog.getByRole("button", { name: "Saving…", exact: true })).toBeDisabled();
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  await openNavigation(page);
  await page.getByRole("button", { name: /Activity/, exact: false }).first().click();
  await expect(page).toHaveURL(/#activity$/);
  answer!();
  await expect.poll(() => fixture.mutations.length).toBe(1);
  await expect(page).toHaveURL(/#activity$/);
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await page.getByRole("button", { name: "Workspace", exact: true }).click();
  await expect(region(page)).toContainText("Starting your coordinator");
  expect(fixture.mutations).toHaveLength(1);
});

test("no machines offers setup prerequisites, not a launch or summary opt-in", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [], sessions: {} });
  await page.goto("/");
  await region(page).getByRole("button", { name: "Set up Mastermind", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Set up Mastermind", exact: true });
  await expect(dialog.getByRole("button", { name: "Save and start Mastermind", exact: true })).toBeDisabled();
  await dialog.getByRole("button", { name: "Connect a machine", exact: true }).click();
  await expect(page.getByRole("dialog", { name: "Connect a machine", exact: true })).toBeVisible();
  expect(fixture.mutations).toEqual([]);
});

test("uncertain startup is not offered a restart and finished output needs an explicit start", async ({ page }) => {
  const state = configured();
  state.status = "uncertain"; state.session = undefined; state.stage = "Checking a previous startup";
  const fixture = await mockFleet(page, { ...fixtureOptions(), mastermind: state });
  await page.goto("/");
  await expect(region(page)).toContainText("will not launch another coordinator");
  await expect(region(page).getByRole("button", { name: "Start Mastermind", exact: true })).toHaveCount(0);
  const finished: Session = { ...coordinator, status: "exited", exitCode: 0 };
  fixture.sessions.dev = [finished];
  Object.assign(state, { session: structuredClone(finished), status: "exited", stage: "The coordinator finished", updatedAt: new Date().toISOString() });
  await expect(region(page)).toContainText("Saved output");
  expect(fixture.mutations).toEqual([]);
  await region(page).getByRole("button", { name: "Start Mastermind", exact: true }).click();
  expect(fixture.mutations).toEqual([{ method: "POST", path: "/api/mastermind/start", body: { revision: 3 }, csrf: "fixture-csrf" }]);
  expect(fixture.sessions.dev).toEqual([finished]);
});
