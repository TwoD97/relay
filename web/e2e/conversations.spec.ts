import { expect, test, type Page } from "@playwright/test";
import { mockFleet, work } from "./fixture";

const id = "1d42ea81-e24d-42a3-8bfd-c6db405741d1";
async function fixture(page: Page, supported = true) {
  const fleet = await mockFleet(page);
  let title = "Refactor the scheduler";
  const writes: unknown[] = [];
  await page.route("**/api/hosts/dev/runtime/harnesses", route => route.fulfill({ json: [{ id: "shell", installed: true, name: "Shell" }, { id: "codex", installed: true, name: "Codex", conversationResume: supported }, { id: "claude", installed: true, name: "Claude Code", conversationResume: supported }] }));
  await page.route("**/api/hosts/dev/conversations**", route => {
    const request = route.request(); const url = new URL(request.url());
    const harness = request.method() === "PATCH" ? request.postDataJSON().harness : url.searchParams.get("harness");
    const cwd = request.method() === "PATCH" ? request.postDataJSON().cwd : url.searchParams.get("cwd");
    if (request.method() === "PATCH") { writes.push(request.postDataJSON()); title = request.postDataJSON().title; return route.fulfill({ json: { id, harness, title, titleSource: harness === "claude" ? "relay" : "provider", cwd, updatedAt: "2026-11-04T10:00:00Z" } }); }
    return route.fulfill({ json: { cwd, harness, resumeSupported: supported, renameMode: harness === "claude" ? "relay" : "provider", truncated: false, conversations: cwd === "/other" ? [] : [{ id, harness, title, titleSource: "provider", cwd, updatedAt: "2026-11-04T10:00:00Z" }] } });
  });
  return { ...fleet, writes };
}
async function open(page: Page) {
  await page.goto("/#host/dev"); await page.getByRole("button", { name: "New session", exact: true }).first().click();
  const dialog = page.getByRole("dialog", { name: "Start a session" });
  await dialog.getByRole("radio", { name: /Codex/ }).check();
  await expect(dialog.locator(".conversation-select")).toContainText("Refactor the scheduler");
  return dialog;
}
test("resume selects a literal conversation identity and rename persists without starting work", async ({ page }) => {
  const model = await fixture(page); const dialog = await open(page);
  await dialog.getByRole("button", { name: /Refactor the scheduler/, exact: false }).filter({ has: page.locator("strong") }).click();
  await dialog.getByRole("button", { name: "Rename conversation Refactor the scheduler" }).click();
  await dialog.getByLabel("Conversation name", { exact: true }).fill("Scheduler follow-up");
  await dialog.getByRole("button", { name: "Save name", exact: true }).click();
  await expect(dialog.getByRole("button", { name: /Scheduler follow-up/ }).filter({ has: page.locator("strong") })).toHaveAttribute("aria-pressed", "true");
  expect(model.mutations).toEqual([]);
  expect(model.writes).toEqual([{ harness: "codex", cwd: work.cwd, title: "Scheduler follow-up" }]);
  await dialog.getByRole("button", { name: "Resume conversation", exact: true }).click();
  expect(model.mutations).toHaveLength(1);
  expect(model.mutations[0].body).toEqual({ harness: "codex", cwd: work.cwd, workspace: work.workspace, title: "Scheduler follow-up", resumeConversationId: id });
  expect(model.terminalMessages.filter(message => message.type === "input")).toEqual([]);
});
test("changing project or provider clears the selected saved conversation", async ({ page }) => {
  const model = await fixture(page); const dialog = await open(page);
  await dialog.locator(".conversation-select").click();
  await expect(dialog.getByRole("button", { name: "Resume conversation", exact: true })).toBeEnabled();
  await dialog.getByLabel("Project folder").fill("/other");
  await expect(dialog.getByRole("button", { name: "Start session", exact: true })).toBeEnabled();
  await expect(dialog).toContainText("No saved conversations");
  await dialog.getByLabel("Project folder").fill(work.cwd);
  await dialog.locator(".conversation-select").click();
  await dialog.getByRole("radio", { name: /Claude Code/ }).check();
  await expect(dialog.getByRole("button", { name: "Start session", exact: true })).toBeEnabled();
  await dialog.getByRole("button", { name: "Rename conversation Refactor the scheduler" }).click();
  await expect(dialog).toContainText("Rename in Relay");
  await expect(dialog).toContainText("Provider history stays unchanged");
  expect(model.mutations).toEqual([]);
});
test("old runtimes show history but cannot accidentally start a fresh chat as resume", async ({ page }) => {
  const model = await fixture(page, false); const dialog = await open(page);
  await expect(dialog.locator(".conversation-select")).toBeDisabled();
  await expect(dialog).toContainText("Update and restart this runtime");
  await expect(dialog.getByRole("button", { name: "Resume conversation", exact: true })).toHaveCount(0);
  await dialog.getByRole("button", { name: "Start session", exact: true }).click();
  expect(model.mutations[0].body).not.toHaveProperty("resumeConversationId");
});
test("a failed list does not block a fresh session or initiate a provider login", async ({ page }) => {
  const model = await fixture(page);
  await page.route("**/api/hosts/dev/conversations**", route => route.fulfill({ status: 503, json: { error: "Conversation metadata unavailable" } }));
  await page.goto("/#host/dev"); await page.getByRole("button", { name: "New session", exact: true }).first().click();
  const dialog = page.getByRole("dialog", { name: "Start a session" }); await dialog.getByRole("radio", { name: /Codex/ }).check();
  await expect(dialog.getByRole("alert")).toContainText("Conversation metadata unavailable");
  await expect(dialog.getByRole("button", { name: "Start session", exact: true })).toBeEnabled();
  expect(model.mutations).toEqual([]);
});
