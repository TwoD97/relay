import { expect, test, type Locator, type Page } from "@playwright/test";
import { mockFleet, openNavigation, remote, work } from "./fixture";
import type { MastermindPage, MastermindWorkspaceState } from "../src/types";

const thumb = { light: "rgb(120, 137, 107)", dark: "rgb(99, 116, 90)" };
const color = (mode: "light" | "dark") => `${thumb[mode]} rgba(0, 0, 0, 0)`;
async function scrollToBottom(element: Locator) {
  await expect.poll(() => element.evaluate((node) => node.scrollHeight > node.clientHeight + 10)).toBe(true);
  await element.evaluate((node) => { node.scrollTop = node.scrollHeight; });
  await expect.poll(() => element.evaluate((node) => node.scrollTop)).toBeGreaterThan(0);
}
async function closeNavigation(page: Page) {
  const close = page.getByRole("complementary", { name: "Fleet navigation", exact: true }).getByRole("button", { name: "Close navigation", exact: true });
  if (await close.isVisible()) await close.click();
}

test("sidebar and attention scrollbars follow Light and Dark without hiding overflow content", async ({ page }, testInfo) => {
  const sessions = Array.from({ length: 24 }, (_, index) => ({ ...work, id: `notice-${index}`, title: `Scrollable notice ${index}`, attention: { kind: "notification" as const, source: "claude-hook" as const, updatedAt: "2026-11-03T08:00:00Z" } }));
  const fixture = await mockFleet(page, { hosts: [structuredClone(remote)], sessions: { dev: sessions } });
  await page.goto("/");
  for (const mode of ["light", "dark"] as const) {
    await page.getByLabel("Appearance", { exact: true }).selectOption(mode);
    await openNavigation(page);
    const main = page.locator(".sidebar-main"), list = page.locator(".attention-list");
    await main.evaluate((node) => { node.scrollTop = 0; });
    await expect(main).toHaveCSS("scrollbar-color", color(mode));
    await expect(list).toHaveCSS("scrollbar-color", color(mode));
    await scrollToBottom(list);
    await expect.poll(() => list.evaluate((node) => node.lastElementChild!.getBoundingClientRect().bottom <= node.getBoundingClientRect().bottom + 1)).toBe(true);
    await page.screenshot({ path: testInfo.outputPath(`attention-expanded-${mode}.png`), animations: "disabled" });
    await scrollToBottom(main);
    await expect.poll(() => page.locator(".session-row").last().evaluate((node) => node.getBoundingClientRect().bottom <= node.closest(".sidebar-main")!.getBoundingClientRect().bottom + 1)).toBe(true);
    await main.evaluate((node) => { node.scrollTop = 0; });
    await page.getByRole("button", { name: "Collapse needs attention", exact: true }).click();
    await page.screenshot({ path: testInfo.outputPath(`attention-collapsed-${mode}.png`), animations: "disabled" });
    await page.getByRole("button", { name: "Expand needs attention", exact: true }).click();
    await closeNavigation(page);
  }
  expect(fixture.mutations).toEqual([]);
});

test("task board and sandboxed canvas retain themed native scrolling", async ({ page }) => {
  const now = "2026-11-03T08:00:00Z", id = "a".repeat(32);
  const pages: MastermindPage[] = [{ id, title: "Long plan", html: `${Array.from({ length: 70 }, (_, i) => `<p>Plan paragraph ${i}: a useful step with more detail.</p>`).join("")}<h2>End of plan</h2>`, revision: 1, updatedAt: now }];
  const workspace: MastermindWorkspaceState = { revision: 1, updatedAt: now, pages, tasks: Array.from({ length: 40 }, (_, i) => ({ id: i.toString(16).padStart(32, "0"), title: `Task ${i}`, detail: "A real scrollable task card", status: "todo", updatedAt: now })) };
  const fixture = await mockFleet(page, { pages, workspace });
  await page.goto("/#mastermind/tasks");
  for (const mode of ["light", "dark"] as const) {
    await page.getByLabel("Appearance", { exact: true }).selectOption(mode);
    const board = page.locator(".workspace-task-board");
    await expect(board).toHaveCSS("scrollbar-color", color(mode));
    await scrollToBottom(board);
    await expect.poll(() => page.getByRole("article", { name: "Task 39", exact: true }).evaluate((node) => node.getBoundingClientRect().bottom <= node.closest(".workspace-task-board")!.getBoundingClientRect().bottom + 1)).toBe(true);
  }
  await page.getByRole("navigation", { name: "Mastermind views", exact: true }).getByRole("button", { name: "Pages", exact: true }).click();
  const frame = page.frameLocator('iframe[title="Long plan canvas"]');
  for (const mode of ["light", "dark"] as const) {
    // Pages intentionally uses the whole content area. Change appearance from
    // the overview, then return to the same page through normal navigation.
    await page.getByRole("button", { name: "Back to Mastermind overview", exact: true }).click();
    await page.getByLabel("Appearance", { exact: true }).selectOption(mode);
    await page.getByRole("navigation", { name: "Mastermind views", exact: true }).getByRole("button", { name: "Pages", exact: true }).click();
    await expect(frame.locator("html")).toHaveCSS("scrollbar-color", color(mode));
    await expect(page.locator('iframe[title="Long plan canvas"]')).toHaveAttribute("sandbox", "");
    await frame.locator("body").evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    await expect.poll(() => frame.locator("body").evaluate(() => window.scrollY)).toBeGreaterThan(0);
    await expect.poll(() => frame.getByRole("heading", { name: "End of plan", exact: true }).evaluate((node) => node.getBoundingClientRect().bottom <= innerHeight + 1)).toBe(true);
    expect(await frame.locator("body").evaluate(() => { try { return window.parent.document.title; } catch { return "isolated"; } })).toBe("isolated");
  }
  expect(fixture.mutations).toEqual([]);
});

test("xterm scrollback slider follows the theme and high contrast without resizing or sending input", async ({ page }, testInfo) => {
  const output = Array.from({ length: 120 }, (_, index) => `SCROLL_LINE_${index}`).join("\r\n");
  const fixture = await mockFleet(page, { terminalOutput: output, terminalGeometry: { cols: 32, rows: 12 }, terminalOccupied: true });
  await page.goto("/#host/dev/session/session-1");
  const slider = page.locator(".terminal-canvas .xterm-scrollable-element > .scrollbar.vertical > .slider");
  await expect(slider).toBeAttached();
  for (const mode of ["light", "dark"] as const) {
    await page.getByLabel("Appearance", { exact: true }).selectOption(mode);
    await page.mouse.move(5, 5);
    await expect(slider).toHaveCSS("background-color", thumb[mode]);
    await expect(page.locator(".terminal-canvas")).toHaveCSS("scrollbar-color", color(mode));
    await expect(page.locator(".xterm-rows")).toContainText("SCROLL_LINE_119");
  }
  await page.locator(".xterm-screen").hover();
  await page.mouse.wheel(0, -400);
  await expect(page.locator(".xterm-rows")).not.toContainText("SCROLL_LINE_119");
  await expect(page.locator(".terminal-canvas .xterm-scrollable-element > .scrollbar.vertical")).not.toHaveClass(/\binvisible\b/);
  // Xterm normalizes one wheel event to a small number of lines. Use the
  // actual slider for an end-to-end check of its full drag range.
  const dragSlider = async (edge: "top" | "bottom") => {
    const handle = (await slider.boundingBox())!;
    const track = (await page.locator(".terminal-canvas .xterm-scrollable-element > .scrollbar.vertical").boundingBox())!;
    await page.mouse.move(handle.x + handle.width / 2, handle.y + handle.height / 2);
    await page.mouse.down();
    await page.mouse.move(handle.x + handle.width / 2, edge === "top" ? track.y + 1 : track.y + track.height - 1, { steps: 8 });
    await page.mouse.up();
  };
  await dragSlider("top");
  await expect(page.locator(".xterm-rows")).toContainText("SCROLL_LINE_0");
  await dragSlider("bottom");
  await expect(page.locator(".xterm-rows")).toContainText("SCROLL_LINE_119");
  await page.emulateMedia({ forcedColors: "active" });
  await expect(page.locator(".terminal-canvas")).toHaveCSS("scrollbar-color", "auto");
  const systemColor = await page.evaluate(() => { const probe = document.createElement("div"); probe.style.backgroundColor = "ButtonText"; probe.style.forcedColorAdjust = "none"; document.body.appendChild(probe); const value = getComputedStyle(probe).backgroundColor; probe.remove(); return value; });
  await page.mouse.move(5, 5);
  await expect(slider).toHaveCSS("background-color", systemColor);
  await page.screenshot({ path: testInfo.outputPath("terminal-scrollbar-high-contrast.png"), animations: "disabled" });
  expect(fixture.terminalMessages).toEqual([]);
  expect(fixture.attachments).toHaveLength(1);
});
