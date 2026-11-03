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
  const originalHeight = (await iframe.boundingBox())!.height;
  const original = await iframe.elementHandle();
  for (const theme of ["Light", "Dark"]) {
    await page.getByLabel("Appearance", { exact: true }).selectOption(theme.toLowerCase());
    await page.getByRole("button", { name: "Expand page to full window", exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`#mastermind/pages/${pageId}/window$`));
    await expect(page.getByRole("navigation", { name: "Mastermind views", exact: true })).toBeHidden();
    await expect(page.getByRole("tablist", { name: "Canvas pages", exact: true })).toBeHidden();
    await expect(page.locator(".workspace-heading")).toBeHidden();
    await expect(iframe).toHaveAttribute("sandbox", "");
    await expectExpandedFrame(page);
    expect((await iframe.boundingBox())!.height).toBeGreaterThan(originalHeight + 100);
    await expect(page.locator(".xterm")).toHaveCount(0);
    if (testInfo.project.name === "desktop") await page.screenshot({ path: testInfo.outputPath(`canvas-expanded-${theme.toLowerCase()}.png`), animations: "disabled" });
    await page.getByRole("button", { name: "Restore page layout", exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`#mastermind/pages/${pageId}$`));
    await expect(page.getByRole("tab", { name: "Responsive report", exact: true })).toHaveAttribute("aria-selected", "true");
    await expect(page.getByRole("navigation", { name: "Mastermind views", exact: true })).toBeVisible();
    expect(await original!.evaluate((element) => element.isConnected && element === document.querySelector("iframe"))).toBe(true);
  }
  expect(fixture.attachments).toEqual([]);
  expect(fixture.mutations).toEqual([]);
});

test("Fit page width changes only the preview's outer wrapper and keeps authored HTML and sandbox intact", async ({ page }) => {
  const fixture = await canvasFixture(page);
  const frame = page.frameLocator('iframe[title="Responsive report canvas"]');
  const fit = page.getByRole("button", { name: "Fit page width", exact: true });
  await expect(fit).toHaveAttribute("aria-pressed", "false");
  await expect(frame.locator("main.report")).toHaveCSS("width", "200px");
  await fit.click();
  await expect(fit).toHaveAttribute("aria-pressed", "true");
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
  await fit.click();
  await expect(fit).toHaveAttribute("aria-pressed", "false");
  await expect(frame.locator("main.report")).toHaveCSS("width", "200px");
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
  await page.getByRole("tab", { name: "Another report", exact: true }).click();
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
