import { expect, test, type Page } from "@playwright/test";
import { mockFleet, remote, work } from "./fixture";
import type { MastermindPage, MastermindState, MastermindWorkspaceState, Session } from "../src/types";

const now = "2026-11-03T08:00:00Z";
const firstPage = "a".repeat(32);
const secondPage = "b".repeat(32);
const coordinator: Session = { ...work, id: "coordinator", title: "Mastermind", harness: "codex", agentMode: "manual", mastermindId: "c".repeat(32) };

async function routingFixture(page: Page) {
  const pages: MastermindPage[] = [
    { id: firstPage, title: "Plan", html: "<h1>Release plan</h1>", updatedAt: now, revision: 1 },
    { id: secondPage, title: "Architecture", html: "<h1>System map</h1>", updatedAt: now, revision: 1 },
  ];
  const workspace: MastermindWorkspaceState = { revision: 1, updatedAt: now, pages: pages.map(({ id, title, updatedAt }) => ({ id, title, updatedAt })), tasks: [{ id: "d".repeat(32), title: "Review release", detail: "Routing fixture", status: "todo", pageId: secondPage, updatedAt: now }] };
  const mastermind: MastermindState = { revision: 1, enabled: true, status: "running", stage: "Connected", config: { hostId: remote.id, cwd: coordinator.cwd, harness: "codex", agentMode: "manual" }, session: coordinator, updatedAt: now };
  return mockFleet(page, { hosts: [structuredClone(remote)], pages, workspace, mastermind, sessions: { dev: [structuredClone(coordinator), structuredClone(work)] } });
}

async function activeSection(page: Page, section: string) {
  await expect(page.getByRole("navigation", { name: "Mastermind views", exact: true, includeHidden: true }).getByRole("button", { name: section, exact: true, includeHidden: true })).toHaveAttribute("aria-current", "page");
  await expect(page.locator(".topbar-breadcrumb")).toContainText(`Mastermind/${section}`);
}

test("Mastermind section deep links select the matching view without starting another session", async ({ page }) => {
  const fixture = await routingFixture(page);
  for (const [hash, section] of [["mastermind", "Overview"], ["mastermind/pages", "Pages"], [`mastermind/pages/${secondPage}`, "Pages"], ["mastermind/tasks", "Tasks"], ["mastermind/terminal", "Terminal"]]) {
    await page.goto(`/#${hash}`);
    await activeSection(page, section);
    await expect(page).toHaveURL(new RegExp(`#${hash}$`));
    if (hash.endsWith(secondPage)) await expect(page.frameLocator('iframe[title="Architecture canvas"]').getByRole("heading", { name: "System map" })).toBeVisible();
    if (section !== "Terminal") await expect(page.locator(".terminal-shell")).toHaveCount(0);
  }
  await expect(page.locator(".terminal-shell")).toBeVisible();
  expect(fixture.attachments).toHaveLength(1);
  expect(fixture.mutations).toEqual([]);
  expect(fixture.sessions.dev.map((session) => session.id)).toEqual([coordinator.id, work.id]);
});

test("legacy Mastermind page links retain the page and expanded state with canonical navigation", async ({ page }) => {
  const fixture = await routingFixture(page);
  await page.goto(`/#mastermind/page/${secondPage}/window`);
  await activeSection(page, "Pages");
  await expect(page).toHaveURL(new RegExp(`#mastermind/pages/${secondPage}/window$`));
  await expect(page.locator(".app-shell")).toHaveClass(/terminal-expanded/);
  await expect(page.frameLocator('iframe[title="Architecture canvas"]').getByRole("heading", { name: "System map" })).toBeVisible();
  await page.getByRole("button", { name: "Restore page layout", exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`#mastermind/pages/${secondPage}$`));
  await expect(page.locator(".app-shell")).not.toHaveClass(/terminal-expanded/);
  await page.getByRole("tab", { name: "Plan", exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`#mastermind/pages/${firstPage}$`));
  await page.goBack();
  await activeSection(page, "Pages");
  await expect(page).toHaveURL(new RegExp(`#mastermind/pages/${secondPage}$`));
  expect(fixture.mutations).toEqual([]);
  expect(fixture.attachments).toEqual([]);
});

test("legacy expanded Mastermind terminal restores to its dedicated section", async ({ page }) => {
  const fixture = await routingFixture(page);
  await page.goto("/#mastermind/window");
  await activeSection(page, "Terminal");
  await expect(page).toHaveURL(/#mastermind\/terminal\/window$/);
  await expect(page.locator(".terminal-shell")).toBeVisible();
  await page.getByRole("button", { name: "Restore workspace", exact: true }).click();
  await expect(page).toHaveURL(/#mastermind\/terminal$/);
  await activeSection(page, "Terminal");
  const navigation = page.getByRole("navigation", { name: "Mastermind views", exact: true });
  await navigation.getByRole("button", { name: "Tasks", exact: true }).click();
  await expect(page).toHaveURL(/#mastermind\/tasks$/);
  await activeSection(page, "Tasks");
  await navigation.getByRole("button", { name: "Overview", exact: true }).click();
  await expect(page).toHaveURL(/#mastermind$/);
  await expect(page.locator(".terminal-shell")).toHaveCount(0);
  await page.goBack();
  await activeSection(page, "Tasks");
  await page.goForward();
  await activeSection(page, "Overview");
  expect(fixture.mutations).toEqual([]);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
});

test("linked task pages use the canonical page section and survive reload", async ({ page }) => {
  const fixture = await routingFixture(page);
  await page.goto("/#mastermind/tasks/window");
  await activeSection(page, "Tasks");
  await page.getByRole("article", { name: "Review release", exact: true }).getByRole("button", { name: "Open page", exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`#mastermind/pages/${secondPage}/window$`));
  await activeSection(page, "Pages");
  await page.reload();
  await activeSection(page, "Pages");
  await expect(page.frameLocator('iframe[title="Architecture canvas"]').getByRole("heading", { name: "System map" })).toBeVisible();
  expect(fixture.mutations).toEqual([]);
});

test("Mastermind malformed links fall back safely and ordinary session window links remain intact", async ({ page }) => {
  const fixture = await routingFixture(page);
  await page.goto("/#mastermind/pages/%E0%A4%A");
  await expect(page.getByRole("heading", { name: "Fleet overview", exact: true })).toBeVisible();
  await page.goto(`/#host/dev/session/${work.id}/window`);
  await expect(page.locator(".terminal-shell")).toBeVisible();
  await expect(page.locator(".topbar-breadcrumb")).toContainText(work.title);
  await page.getByRole("button", { name: "Restore workspace", exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`#host/dev/session/${work.id}$`));
  expect(fixture.mutations).toEqual([]);
  expect(fixture.sessions.dev).toHaveLength(2);
});
