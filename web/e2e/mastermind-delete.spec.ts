import { expect, test } from "@playwright/test";
import { mockFleet } from "./fixture";
import type { MastermindPage, MastermindWorkspaceState } from "../src/types";

const pageId = "a".repeat(32);
const taskId = "b".repeat(32);
const now = "2026-11-03T08:00:00Z";
function state() {
  const pages: MastermindPage[] = [{ id: pageId, title: "Release plan", html: "<h1>Plan</h1>", revision: 1, updatedAt: now }];
  const workspace: MastermindWorkspaceState = { revision: 1, updatedAt: now, pages, tasks: [{ id: taskId, title: "Review deployment", detail: "Check this exact draft.", status: "todo", pageId, updatedAt: now }] };
  return { pages, workspace };
}

test("a delayed page deletion cannot replace a newer workspace view after its confirmation closes", async ({ page }) => {
  const fixture = await mockFleet(page, state());
  let release!: () => void;
  let dispatched = false;
  const pending = new Promise<void>((resolve) => { release = resolve; });
  await page.route(`**/api/mastermind/workspace/pages/${pageId}`, async (route) => {
    if (route.request().method() !== "DELETE") { await route.fallback(); return; }
    dispatched = true;
    await pending;
    await route.fallback();
  });
  await page.goto(`/#mastermind/pages/${pageId}`);
  await expect(page.frameLocator('iframe[title="Release plan canvas"]').getByRole("heading", { name: "Plan", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Delete canvas page", exact: true }).click();
  const confirm = page.getByRole("dialog", { name: "Delete “Release plan”?", exact: true });
  await confirm.getByRole("button", { name: "Delete page", exact: true }).click();
  await expect.poll(() => dispatched).toBe(true);
  await confirm.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.getByRole("navigation", { name: "Mastermind views", exact: true }).getByRole("button", { name: "Tasks", exact: true }).click();
  await expect(page).toHaveURL(/#mastermind\/tasks$/);
  release();
  await expect.poll(() => fixture.workspace.pages.length).toBe(0);
  await expect(page.getByRole("button", { name: "New task", exact: true })).toBeEnabled();
  await expect(page).toHaveURL(/#mastermind\/tasks$/);
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(page.getByRole("region", { name: "Mastermind pages", exact: true })).toHaveCount(0);
  await expect(page.getByRole("article", { name: "Review deployment", exact: true })).toBeVisible();
  expect(fixture.workspace.tasks[0].pageId).toBeUndefined();
  expect(fixture.mutations).toEqual([{ method: "DELETE", path: `/api/mastermind/workspace/pages/${pageId}`, body: { revision: 1 }, csrf: "fixture-csrf" }]);
  expect(fixture.attachments).toEqual([]);
});

test("a stale task editor cannot delete a newer task until its current state is reviewed", async ({ page }) => {
  const fixture = await mockFleet(page, state());
  await page.goto("/#mastermind/tasks");
  await page.getByRole("button", { name: "Edit task Review deployment", exact: true }).click();
  let editor = page.getByRole("dialog", { name: "Edit task", exact: true });
  await expect(editor.getByLabel("Details", { exact: true })).toHaveValue("Check this exact draft.");
  Object.assign(fixture.workspace.tasks[0], { title: "Review changed deployment", detail: "The coordinator replaced this plan. Review it first.", status: "blocked", updatedAt: "2026-11-03T08:01:00Z" });
  fixture.workspace.revision = 2;
  fixture.workspace.updatedAt = "2026-11-03T08:01:00Z";
  await expect(editor).toContainText("The workspace changed. Reopen this task");
  await expect(editor.getByRole("button", { name: "Save task", exact: true })).toBeDisabled();
  await expect(editor.getByRole("button", { name: "Delete task", exact: true })).toBeDisabled();
  expect(fixture.mutations).toEqual([]);
  await editor.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.getByRole("button", { name: "Edit task Review changed deployment", exact: true }).click();
  editor = page.getByRole("dialog", { name: "Edit task", exact: true });
  await expect(editor.getByLabel("Details", { exact: true })).toHaveValue("The coordinator replaced this plan. Review it first.");
  await expect(editor.getByLabel("Task status", { exact: true })).toHaveValue("blocked");
  await editor.getByRole("button", { name: "Delete task", exact: true }).click();
  await page.getByRole("dialog", { name: "Delete “Review changed deployment”?", exact: true }).getByRole("button", { name: "Delete task", exact: true }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect.poll(() => fixture.workspace.tasks.length).toBe(0);
  expect(fixture.mutations).toEqual([{ method: "DELETE", path: `/api/mastermind/workspace/tasks/${taskId}`, body: { revision: 2 }, csrf: "fixture-csrf" }]);
  expect(fixture.workspace.pages).toHaveLength(1);
  expect(fixture.attachments).toEqual([]);
});
