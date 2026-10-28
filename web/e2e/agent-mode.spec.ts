import { expect, test, type Page } from "@playwright/test";
import { mockFleet, remote, work } from "./fixture";
import type { AgentMode, HarnessId } from "../src/types";

const supported = (): Partial<Record<HarnessId, AgentMode[]>> => ({ codex: ["manual", "auto"] });
async function openAgentForm(page: Page) {
  await page.goto("/#host/dev");
  await page.getByRole("button", { name: "New session", exact: true }).first().click();
  const dialog = page.getByRole("dialog", { name: "Start a session", exact: true });
  await dialog.getByRole("radio", { name: /Codex/ }).check();
  await dialog.getByLabel("Project folder").fill("/home/dev/projects/relay");
  await dialog.getByLabel("Session name").fill("Mode test");
  return dialog;
}

test("Manual is the default and only records a supported launch mode", async ({ page }) => {
  const fixture = await mockFleet(page, { sessionModes: supported() });
  const dialog = await openAgentForm(page);
  await expect(dialog.getByRole("radio", { name: "Manual", exact: true })).toBeChecked();
  await expect(dialog).toContainText("Your existing configuration determines when the agent asks for approval");
  await dialog.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Mode test", exact: true })).toBeVisible();
  expect(fixture.mutations).toEqual([{ method: "POST", path: "/api/hosts/dev/runtime/sessions", csrf: "fixture-csrf", body: { title: "Mode test", workspace: work.workspace, cwd: work.cwd, harness: "codex", agentMode: "manual" } }]);
  await expect(page.getByLabel("Launch mode: Manual", { exact: true })).toBeVisible();
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
});

test("Auto is explicit, explains full access and persists only the runtime-reported launch mode", async ({ page }, testInfo) => {
  const fixture = await mockFleet(page, { sessionModes: supported() });
  const dialog = await openAgentForm(page);
  await dialog.getByRole("radio", { name: "Auto", exact: true }).check();
  await expect(dialog).toContainText("Requests full provider permissions");
  await expect(dialog).toContainText("instructions to work autonomously");
  await expect(dialog).toContainText("does not grant sudo");
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBeTruthy();
  await page.screenshot({ path: testInfo.outputPath("agent-auto-start.png"), fullPage: true });
  await dialog.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Mode test", exact: true })).toBeVisible();
  await expect(page.getByLabel("Launch mode: Auto · full access", { exact: true })).toBeVisible();
  expect(fixture.mutations).toHaveLength(1);
  expect(fixture.mutations[0].body).toEqual({ title: "Mode test", workspace: work.workspace, cwd: work.cwd, harness: "codex", agentMode: "auto" });
  await page.reload();
  await expect(page.getByLabel("Launch mode: Auto · full access", { exact: true })).toBeVisible();
  await expect(page.getByLabel("Launch mode: Auto · full access", { exact: true })).toHaveAttribute("title", /launch setting; permissions may change/);
  await page.goto("/#host/dev");
  await expect(page.locator(".sessions-list").getByLabel("Launch mode: Auto · full access", { exact: true })).toBeVisible();
  await page.goto("/");
  await expect(page.getByRole("region", { name: "Sessions and progress" }).getByRole("article", { name: "Development: Mode test", exact: true }).getByLabel("Launch mode: Auto · full access", { exact: true })).toBeVisible();
  expect(fixture.mutations).toHaveLength(1);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
});

test("older runtime keeps provider defaults and cannot offer Auto", async ({ page }) => {
  const fixture = await mockFleet(page, { legacyRuntime: true });
  const dialog = await openAgentForm(page);
  await expect(dialog.getByRole("radio", { name: "Auto", exact: true })).toBeDisabled();
  await expect(dialog).toContainText("does not advertise Auto support");
  await dialog.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Mode test", exact: true })).toBeVisible();
  expect(fixture.mutations[0].body).toEqual({ title: "Mode test", workspace: work.workspace, cwd: work.cwd, harness: "codex" });
  await expect(page.getByLabel("Launch mode: Provider default", { exact: true })).toBeVisible();
  await expect(page.getByLabel("Launch mode: Manual", { exact: true })).toHaveCount(0);
});

test("switching to a shell strips mode and returning to an agent starts with Manual", async ({ page }) => {
  const fixture = await mockFleet(page, { sessionModes: supported() });
  const dialog = await openAgentForm(page);
  await dialog.getByRole("radio", { name: "Auto", exact: true }).check();
  await dialog.getByRole("radio", { name: /Terminal/ }).check();
  await expect(dialog.getByRole("group", { name: "Agent mode", exact: true })).toHaveCount(0);
  await dialog.getByRole("radio", { name: /Codex/ }).check();
  await expect(dialog.getByRole("radio", { name: "Manual", exact: true })).toBeChecked();
  await dialog.getByRole("radio", { name: /Terminal/ }).check();
  await dialog.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Mode test", exact: true })).toBeVisible();
  expect(fixture.mutations[0].body).toEqual({ title: "Mode test", workspace: work.workspace, cwd: work.cwd, harness: "shell" });
  await expect(page.locator(".agent-mode-badge")).toHaveCount(0);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
});

test("stale capability data disables Auto without silently changing the selected mode", async ({ page }) => {
  const fixture = await mockFleet(page, { sessionModes: supported() });
  const dialog = await openAgentForm(page);
  await dialog.getByRole("radio", { name: "Auto", exact: true }).check();
  await page.route("**/api/hosts/dev/runtime/harnesses", (route) => route.fulfill({ status: 503, json: { error: "Agent capabilities unavailable" } }));
  await expect(dialog.getByRole("radio", { name: "Auto", exact: true })).toBeDisabled();
  await expect(dialog.getByRole("radio", { name: "Auto", exact: true })).toBeChecked();
  await expect(dialog).toContainText("Auto is still selected");
  await expect(dialog.getByRole("button", { name: "Start session", exact: true })).toBeDisabled();
  expect(fixture.mutations).toEqual([]);
  await dialog.getByRole("radio", { name: "Manual", exact: true }).check();
  await dialog.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Mode test", exact: true })).toBeVisible();
  expect(fixture.mutations[0].body).not.toHaveProperty("agentMode");
});

test("capability removal during project preparation prevents an Auto launch", async ({ page }) => {
  const modes = supported();
  const fixture = await mockFleet(page, { sessionModes: modes });
  let release!: () => void;
  const prepared = new Promise<void>((resolve) => { release = resolve; });
  let requests = 0;
  await page.route("**/api/hosts/dev/project-context", async (route) => { requests++; await prepared; await route.fallback(); });
  const dialog = await openAgentForm(page);
  await dialog.getByRole("radio", { name: "Auto", exact: true }).check();
  await dialog.getByRole("checkbox", { name: /Shared instructions and memory/ }).check();
  await dialog.getByRole("button", { name: "Start session", exact: true }).click();
  await expect.poll(() => requests).toBe(1);
  modes.codex = ["manual"];
  await expect(dialog).toContainText("Auto is still selected");
  release();
  await expect(dialog.getByRole("alert")).toContainText("No session was started");
  expect(fixture.mutations.filter((mutation) => mutation.path.endsWith("/sessions"))).toEqual([]);
  expect(fixture.sessions.dev).toEqual([work]);
});

test("unsupported installed CLI rejects Auto once and leaves mode choice inspectable", async ({ page }) => {
  const fixture = await mockFleet(page, { sessionModes: supported() });
  let attempts = 0;
  await page.route("**/api/hosts/dev/runtime/sessions", async (route) => {
    if (route.request().method() === "GET") return route.fallback();
    attempts++;
    expect(route.request().postDataJSON().agentMode).toBe("auto");
    await route.fulfill({ status: 422, json: { error: "Installed Codex does not support Auto. Update or repair this agent." } });
  });
  const dialog = await openAgentForm(page);
  await dialog.getByRole("radio", { name: "Auto", exact: true }).check();
  await dialog.getByRole("button", { name: "Start session", exact: true }).dblclick();
  await expect(dialog.getByRole("alert")).toContainText("Update or repair this agent");
  await expect(dialog.getByRole("radio", { name: "Auto", exact: true })).toBeChecked();
  expect(attempts).toBe(1);
  expect(fixture.sessions.dev).toEqual([work]);
  expect(fixture.attachments).toEqual([]);
});

test("an unconfirmed launch mode is not fabricated or retried", async ({ page }) => {
  const fixture = await mockFleet(page, { sessionModes: supported() });
  let attempts = 0;
  await page.route("**/api/hosts/dev/runtime/sessions", async (route) => {
    if (route.request().method() === "GET") return route.fallback();
    attempts++;
    const body = route.request().postDataJSON(); delete body.agentMode;
    const session = { ...work, ...body, id: "unconfirmed" };
    fixture.sessions.dev.push(session);
    return route.fulfill({ status: 201, json: session });
  });
  const dialog = await openAgentForm(page);
  await dialog.getByRole("radio", { name: "Auto", exact: true }).check();
  await dialog.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Mode test", exact: true })).toBeVisible();
  await expect(page.getByRole("status").filter({ hasText: "did not confirm the requested launch mode" })).toBeVisible();
  await expect(page.getByLabel("Launch mode: Provider default", { exact: true })).toBeVisible();
  await expect(page.getByLabel("Launch mode: Auto · full access", { exact: true })).toHaveCount(0);
  expect(attempts).toBe(1);
});

test("saved launch badges exclude shells and setup jobs", async ({ page }) => {
  await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [
    { ...work, id: "manual", harness: "claude", title: "Manual agent", agentMode: "manual" },
    { ...work, id: "auto", harness: "codex", title: "Auto agent", agentMode: "auto" },
    { ...work, id: "old", harness: "claude", title: "Older agent" },
    { ...work, id: "shell", title: "Plain shell" },
    { ...work, id: "login", harness: "codex", title: "Sign in", purpose: "login" },
  ] } });
  await page.goto("/");
  const progress = page.getByRole("region", { name: "Sessions and progress" });
  await expect(progress.locator(".agent-mode-badge")).toHaveCount(3);
  await expect(progress.getByLabel("Launch mode: Auto · full access", { exact: true })).toBeVisible();
  await expect(progress.getByLabel("Launch mode: Manual", { exact: true })).toBeVisible();
  await expect(progress.getByLabel("Launch mode: Provider default", { exact: true })).toBeVisible();
  await page.reload();
  await expect(progress.locator(".agent-mode-badge")).toHaveCount(3);
});
