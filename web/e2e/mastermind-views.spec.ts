import { expect, test, type Page } from "@playwright/test";
import { mockFleet, remote, work } from "./fixture";
import type { MastermindPage, MastermindState, MastermindTask, MastermindWorkspaceState, Session } from "../src/types";

const now = "2026-11-03T08:00:00Z";
const pageId = "a".repeat(32);
const coordinator: Session = { ...work, id: "coordinator", title: "Mastermind", harness: "codex", agentMode: "manual", mastermindId: "c".repeat(32) };
const navigation = (page: Page) => page.getByRole("navigation", { name: "Mastermind views", exact: true });
async function fixtureFor(page: Page) {
  const pages: MastermindPage[] = [{ id: pageId, title: "Release plan", html: "<h1>Ready for review</h1><p>A page with space for the whole plan.</p>", updatedAt: now, revision: 1 }];
  const tasks: MastermindTask[] = [
    { id: "1".repeat(32), title: "Check deployment", detail: "Review the rollback sequence.", status: "todo", pageId, updatedAt: now },
    { id: "2".repeat(32), title: "Test uploads", detail: "Run checks on the staging machine.", status: "doing", hostId: "dev", sessionId: coordinator.id, updatedAt: now },
    { id: "3".repeat(32), title: "Confirm permissions", detail: "Waiting for the owner to review access. ".repeat(12), status: "blocked", updatedAt: now },
    { id: "4".repeat(32), title: "Archive notes", detail: "Saved the previous report.", status: "done", updatedAt: now },
  ];
  const workspace: MastermindWorkspaceState = { revision: 1, updatedAt: now, pages, tasks };
  const mastermind: MastermindState = { revision: 1, enabled: true, status: "running", stage: "Connected", config: { hostId: "dev", cwd: coordinator.cwd, harness: "codex", agentMode: "manual" }, session: structuredClone(coordinator), updatedAt: now };
  return mockFleet(page, { hosts: [structuredClone(remote)], sessions: { dev: [structuredClone(coordinator)] }, pages, workspace, mastermind });
}

async function onlyView(page: Page, view: "Overview" | "Pages" | "Tasks" | "Terminal") {
  await expect(navigation(page).getByRole("button", { name: view, exact: true })).toHaveAttribute("aria-current", "page");
  await expect(page.getByRole("region", { name: "Mastermind pages", exact: true })).toHaveCount(view === "Pages" ? 1 : 0);
  await expect(page.getByRole("region", { name: "Mastermind tasks", exact: true })).toHaveCount(view === "Tasks" ? 1 : 0);
  await expect(page.getByRole("region", { name: "Mastermind terminal", exact: true })).toHaveCount(view === "Terminal" ? 1 : 0);
  await expect(page.locator(".mastermind")).toHaveAttribute("data-presentation", view === "Terminal" ? "terminal" : "summary");
  if (view !== "Pages") await expect(page.locator("iframe")).toHaveCount(0);
  if (view !== "Terminal") await expect(page.locator(".xterm")).toHaveCount(0);
}

test("overview and dedicated views detach only the viewer and preserve the running coordinator", async ({ page }) => {
  const fixture = await fixtureFor(page);
  await page.goto("/#mastermind");
  await onlyView(page, "Overview");
  const next = page.getByRole("region", { name: "Up next", exact: true });
  await expect(next).toContainText("Confirm permissions");
  await expect(next).not.toContainText("Archive notes");
  expect(fixture.attachments).toEqual([]);
  await next.getByRole("button", { name: /Confirm permissions/ }).click();
  await expect(page.getByRole("dialog", { name: "Edit task", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.getByRole("button", { name: "View all pages", exact: true }).click();
  await onlyView(page, "Pages");
  await expect(page.frameLocator('iframe[title="Release plan canvas"]').getByRole("heading", { name: "Ready for review" })).toBeVisible();
  await navigation(page).getByRole("button", { name: "Tasks", exact: true }).click();
  await onlyView(page, "Tasks");
  expect(fixture.attachments).toEqual([]);
  await page.getByRole("button", { name: "Open terminal", exact: true }).click();
  await onlyView(page, "Terminal");
  await page.getByRole("button", { name: "Take control", exact: true }).click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  const original = await page.locator(".xterm").elementHandle();
  await navigation(page).getByRole("button", { name: "Overview", exact: true }).click();
  await onlyView(page, "Overview");
  expect(await original!.evaluate((element) => element.isConnected)).toBe(false);
  expect(fixture.sessions.dev).toEqual([coordinator]);
  await page.goBack();
  await onlyView(page, "Terminal");
  await expect(page.getByText("Watching", { exact: true })).toBeVisible();
  await expect.poll(() => fixture.attachments.length).toBe(2);
  expect(fixture.attachments.every((path) => path === "/api/hosts/dev/runtime/sessions/coordinator/terminal")).toBe(true);
  expect(fixture.terminalMessages.filter((message) => message.type === "claim")).toHaveLength(1);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
  expect(fixture.mutations).toEqual([]);
});

test("task filters and search narrow the board without writes and status changes use current revision", async ({ page }) => {
  const fixture = await fixtureFor(page);
  await page.goto("/#mastermind/tasks");
  await onlyView(page, "Tasks");
  const board = page.getByRole("region", { name: "Mastermind tasks", exact: true });
  const filters = page.getByRole("navigation", { name: "Task filters", exact: true });
  await expect(board.getByRole("article")).toHaveCount(4);
  await filters.getByRole("button", { name: "Blocked", exact: true }).click();
  await expect(filters.getByRole("button", { name: "Blocked", exact: true })).toHaveAttribute("aria-pressed", "true");
  await expect(board.getByRole("article")).toHaveCount(1);
  const blocked = board.getByRole("article", { name: "Confirm permissions", exact: true });
  await blocked.getByRole("button", { name: "Show details for Confirm permissions", exact: true }).click();
  await expect(blocked.getByRole("button", { name: "Hide details for Confirm permissions", exact: true })).toHaveAttribute("aria-expanded", "true");
  await page.getByLabel("Search tasks", { exact: true }).fill("not present");
  await expect(board.getByRole("heading", { name: "No matching tasks", exact: true })).toBeVisible();
  await board.getByRole("button", { name: "Clear filters", exact: true }).click();
  await expect(board.getByRole("article")).toHaveCount(4);
  await page.getByLabel("Search tasks", { exact: true }).fill(" DEVELOPMENT ");
  await expect(board.getByRole("article")).toHaveCount(1);
  await expect(board.getByRole("article", { name: "Test uploads", exact: true })).toBeVisible();
  expect(fixture.mutations).toEqual([]);
  await page.getByLabel("Search tasks", { exact: true }).fill("rollback");
  const task = board.getByRole("article", { name: "Check deployment", exact: true });
  await expect(task).toBeVisible();
  await task.getByLabel("Status for Check deployment", { exact: true }).selectOption("done");
  await expect.poll(() => fixture.workspace.tasks[0].status).toBe("done");
  expect(fixture.mutations).toHaveLength(1);
  expect(fixture.mutations[0]).toMatchObject({ method: "PUT", path: `/api/mastermind/workspace/tasks/${"1".repeat(32)}`, body: { revision: 1, title: "Check deployment", status: "done", pageId } });
  await filters.getByRole("button", { name: "To do", exact: true }).click();
  await expect(board.getByRole("heading", { name: "No matching tasks", exact: true })).toBeVisible();
  expect(fixture.attachments).toEqual([]);
});

for (const appearance of ["light", "dark"] as const) test(`separate Mastermind views fit the viewport in ${appearance} appearance`, async ({ page }, testInfo) => {
  const fixture = await fixtureFor(page);
  await page.goto("/#mastermind");
  await page.getByLabel("Appearance", { exact: true }).selectOption(appearance);
  for (const view of ["Overview", "Pages", "Tasks", "Terminal"] as const) {
    await navigation(page).getByRole("button", { name: view, exact: true }).click();
    await onlyView(page, view);
    await expect.poll(() => page.locator(".mastermind-workspace").evaluate((element) => {
      const box = element.getBoundingClientRect();
      const nav = element.querySelector(".workspace-view-navigation")!.getBoundingClientRect();
      return { inside: box.left >= -1 && box.right <= innerWidth + 1, navigation: nav.left >= -1 && nav.right <= innerWidth + 1, document: document.documentElement.scrollWidth <= innerWidth + 1 };
    })).toEqual({ inside: true, navigation: true, document: true });
    if (view === "Pages") {
      const canvas = page.locator('iframe[title="Release plan canvas"]');
      await expect(canvas).toBeVisible();
      const bounds = await canvas.boundingBox();
      expect(bounds!.width).toBeGreaterThan(240);
      expect(bounds!.height).toBeGreaterThan(240);
    }
    if (view === "Terminal") {
      await page.getByRole("button", { name: "Take control", exact: true }).click();
      await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
      await expect.poll(() => page.locator(".terminal-toolbar").evaluate((element) => {
        const toolbar = element.getBoundingClientRect();
        return [...element.querySelectorAll("button")].filter((button) => button.getClientRects().length > 0).every((button) => {
          const box = button.getBoundingClientRect();
          return box.left >= toolbar.left - 1 && box.right <= Math.min(toolbar.left + element.clientWidth, innerWidth) + 1 && box.top >= toolbar.top - 1 && box.bottom <= toolbar.bottom + 1;
        });
      })).toBe(true);
      await expect.poll(() => page.locator(".mastermind-terminal").evaluate((element) => {
        const terminal = element.getBoundingClientRect();
        const container = element.closest(".mastermind-workspace")!.getBoundingClientRect();
        return { fills: terminal.height > 240, bottom: terminal.bottom <= Math.min(container.bottom, innerHeight) + 1 };
      })).toEqual({ fills: true, bottom: true });
    }
    await page.screenshot({ path: testInfo.outputPath(`mastermind-${view.toLowerCase()}-${appearance}.png`), animations: "disabled" });
  }
  expect(fixture.mutations).toEqual([]);
});
