import { expect, test, type Page } from "@playwright/test";
import { mockFleet, remote, work } from "./fixture";
import type { MastermindPage, MastermindState, Session } from "../src/types";

const pageId = "a".repeat(32);
const secondId = "b".repeat(32);
const now = "2026-11-03T08:00:00Z";
const authored = '<main class="report" style="width:200px;max-width:200px;margin-inline:auto"><h1>Canvas width proof</h1><section class="card" style="width:80px;max-width:80px">Small card</section></main>';
async function canvasFixture(page: Page) {
  const coordinator: Session = { ...work, id: "coordinator", harness: "codex", agentMode: "manual", mastermindId: "c".repeat(32) };
  const mastermind: MastermindState = { revision: 1, enabled: true, status: "running", stage: "Connected", config: { hostId: "dev", cwd: work.cwd, harness: "codex", agentMode: "manual" }, session: coordinator, updatedAt: now };
  const pages: MastermindPage[] = [
    { id: pageId, title: "Responsive report", html: authored, revision: 1, updatedAt: now },
    { id: secondId, title: "Another report", html: "<h1>Different page</h1>", revision: 1, updatedAt: now },
  ];
  const fixture = await mockFleet(page, { hosts: [structuredClone(remote)], sessions: { dev: [coordinator] }, mastermind, pages, workspace: { revision: 1, updatedAt: now, pages: pages.map(({ id, title, updatedAt }) => ({ id, title, updatedAt })), tasks: [] } });
  await page.goto(`/#mastermind/pages/${pageId}`);
  await expect(page.frameLocator('iframe[title="Responsive report canvas"]').getByRole("heading", { name: "Canvas width proof", exact: true })).toBeVisible();
  return fixture;
}

async function expectExpandedFrame(page: Page) {
  await expect.poll(async () => page.locator('iframe[title="Responsive report canvas"]').evaluate((iframe) => {
    const box = iframe.getBoundingClientRect();
    const parent = iframe.parentElement!.getBoundingClientRect();
    return { fillsWidth: box.width >= innerWidth - 16 && box.left >= -1 && box.right <= innerWidth + 1, fillsHeight: box.height >= innerHeight * 0.8 && box.bottom >= innerHeight - 10 && box.bottom <= innerHeight + 1, fillsContainer: Math.abs(box.width - parent.width) < 3 && Math.abs(box.height - parent.height) < 3 };
  })).toEqual({ fillsWidth: true, fillsHeight: true, fillsContainer: true });
}

test("a canvas expands to the full window on its own page route and restores without mounting a terminal", async ({ page }, testInfo) => {
  const fixture = await canvasFixture(page);
  const iframe = page.locator('iframe[title="Responsive report canvas"]');
  await expect(page.locator(".mastermind")).toHaveCount(0);
  await expect(page.locator(".topbar")).toBeHidden();
  await expect(page.locator(".workspace-heading, .workspace-page-navigation")).toHaveCount(0);
  await expect(page.getByLabel("Canvas page", { exact: true })).toHaveValue(pageId);
  expect((await page.getByRole("toolbar", { name: "Page controls" }).boundingBox())!.height).toBeLessThanOrEqual(48);
  expect((await iframe.boundingBox())!.height).toBeGreaterThan(page.viewportSize()!.height - 64);
  const original = await iframe.elementHandle();
  for (const theme of ["Light", "Dark"]) {
    await page.evaluate((theme) => { localStorage.setItem("relay.appearance.v1", theme.toLowerCase()); window.dispatchEvent(new StorageEvent("storage", { key: "relay.appearance.v1", newValue: theme.toLowerCase(), storageArea: localStorage })); }, theme);
    await expect(page.locator("html")).toHaveAttribute("data-theme", theme.toLowerCase());
    await page.getByRole("button", { name: "Expand page to full window", exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`#mastermind/pages/${pageId}/window$`));
    await expect(page.getByRole("navigation", { name: "Mastermind views", exact: true })).toBeHidden();
    await expect(page.getByRole("tablist", { name: "Canvas pages", exact: true })).toBeHidden();
    await expect(page.locator(".workspace-heading")).toBeHidden();
    await expect(iframe).toHaveAttribute("sandbox", "");
    await expectExpandedFrame(page);

    await expect(page.locator(".xterm")).toHaveCount(0);
    if (testInfo.project.name === "desktop") await page.screenshot({ path: testInfo.outputPath(`canvas-expanded-${theme.toLowerCase()}.png`), animations: "disabled" });
    await page.getByRole("button", { name: "Restore page layout", exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`#mastermind/pages/${pageId}$`));
    await expect(page.getByLabel("Canvas page", { exact: true })).toHaveValue(pageId);
    await expect(page.getByRole("button", { name: "Back to Mastermind overview", exact: true })).toBeVisible();
    expect(await original!.evaluate((element) => element.isConnected && element === document.querySelector("iframe"))).toBe(true);
  }
  expect(fixture.attachments).toEqual([]);
  expect(fixture.mutations).toEqual([]);
});

test("pages always fit only the preview outer wrapper and keep authored HTML and sandbox intact", async ({ page }) => {
  const fixture = await canvasFixture(page);
  const frame = page.frameLocator('iframe[title="Responsive report canvas"]');
  await expect(page.getByRole("button", { name: "Fit page width", exact: true })).toHaveCount(0);
  await expect.poll(() => frame.locator("main.report").evaluate((main) => {
    const width = main.getBoundingClientRect().width;
    return width > 220 && Math.abs(width - document.body.getBoundingClientRect().width) < 2;
  })).toBe(true);
  await expect(frame.locator(".card")).toHaveCSS("width", "80px");
  await page.screenshot({ path: test.info().outputPath("canvas-fit-width.png"), animations: "disabled" });
  await expect(page.locator("iframe")).toHaveAttribute("sandbox", "");
  const isolation = await frame.locator("body").evaluate(() => {
    let canReadParent = true;
    try { void parent.document.body; } catch { canReadParent = false; }
    return { canReadParent, scripts: document.scripts.length, policy: document.querySelector('meta[http-equiv="Content-Security-Policy"]')?.getAttribute("content") };
  });
  expect(isolation.canReadParent).toBe(false);
  expect(isolation.scripts).toBe(0);
  expect(isolation.policy).toContain("script-src 'none'");
  expect(isolation.policy).toContain("connect-src 'none'");
  await page.getByRole("button", { name: "Edit HTML", exact: true }).click();
  await expect(page.getByRole("dialog").getByLabel("Page HTML", { exact: true })).toHaveValue(authored);
  await page.getByRole("dialog").getByRole("button", { name: "Cancel", exact: true }).click();
  expect(fixture.pages[0].html).toBe(authored);
  expect(fixture.mutations).toEqual([]);
  expect(fixture.attachments).toEqual([]);
});

test("expanded canvas deep links survive reload and browser history without selecting Terminal", async ({ page }) => {
  const fixture = await canvasFixture(page);
  await page.getByRole("button", { name: "Expand page to full window", exact: true }).click();
  await page.reload();
  await expect(page.frameLocator('iframe[title="Responsive report canvas"]').getByRole("heading", { name: "Canvas width proof", exact: true })).toBeVisible();
  await expectExpandedFrame(page);
  await page.getByRole("button", { name: "Restore page layout", exact: true }).click();
  await page.getByLabel("Canvas page", { exact: true }).selectOption(secondId);
  await expect(page.frameLocator('iframe[title="Another report canvas"]').getByRole("heading", { name: "Different page", exact: true })).toBeVisible();
  await page.goBack();
  await expect(page.frameLocator('iframe[title="Responsive report canvas"]').getByRole("heading", { name: "Canvas width proof", exact: true })).toBeVisible();
  await page.goBack();
  await expect(page).toHaveURL(new RegExp(`#mastermind/pages/${pageId}/window$`));
  await expectExpandedFrame(page);
  await expect(page.locator(".xterm")).toHaveCount(0);
  expect(fixture.attachments).toEqual([]);
  expect(fixture.mutations).toEqual([]);
});


test("compact page selection keeps live status explicit and editing it requires a deliberate save", async ({ page }) => {
  const fixture = await canvasFixture(page);
  fixture.pages[0].template = "status-v1";
  fixture.workspace.pages[0].template = "status-v1";
  fixture.workspace.pages[0].updatedAt = "2026-11-04T08:00:00Z";
  await page.getByRole("button", { name: "Refresh Mastermind workspace" }).click();
  await expect(page.getByRole("option", { name: "Responsive report · Live status", exact: true })).toHaveCount(1);
  await page.getByRole("button", { name: "Edit HTML", exact: true }).click();
  const editor = page.getByRole("dialog", { name: "Edit canvas page" });
  await expect(editor).toContainText("Saving HTML turns it into a regular page");
  await editor.getByRole("button", { name: "Cancel", exact: true }).click();
  expect(fixture.mutations).toEqual([]);
  await page.getByRole("button", { name: "Edit HTML", exact: true }).click();
  await editor.getByLabel("Page HTML", { exact: true }).fill("<h1>My static report</h1>");
  await editor.getByRole("button", { name: "Save page", exact: true }).click();
  await expect(editor).toHaveCount(0);
  await expect(page.getByRole("option", { name: "Responsive report", exact: true })).toHaveCount(1);
  expect(fixture.mutations).toHaveLength(1);
  expect(fixture.mutations[0].body).not.toHaveProperty("template");
  await page.getByRole("button", { name: "Expand page to full window", exact: true }).click();
  await page.getByRole("button", { name: "Back to Mastermind overview", exact: true }).click();
  await expect(page.getByRole("navigation", { name: "Mastermind views", exact: true }).getByRole("button", { name: "Overview", exact: true })).toHaveAttribute("aria-current", "page");
  expect(fixture.attachments).toEqual([]);
});
