import { expect, test, type Page } from "@playwright/test";
import { mockFleet, remote } from "./fixture";

const route = "/#host/dev/session/session-1";
const history = Array.from({ length: 160 }, (_, index) => `SCROLL_${String(index).padStart(3, "0")}\r\n`).join("") + "LIVE_PROMPT";

async function dragHistoryToTop(page: Page) {
  const bar = page.locator(".xterm-scrollable-element > .scrollbar.vertical");
  const slider = bar.locator(".slider");
  // xterm auto-hides its scrollbar until the pointer enters the screen.
  await page.locator(".xterm-screen").hover();
  await bar.hover();
  const track = (await bar.boundingBox())!;
  const thumb = (await slider.boundingBox())!;
  expect(thumb.height).toBeGreaterThan(0);
  await page.mouse.move(thumb.x + thumb.width / 2, thumb.y + thumb.height / 2);
  await page.mouse.down();
  await page.mouse.move(thumb.x + thumb.width / 2, track.y + thumb.height / 2, { steps: 8 });
  await page.mouse.up();
}

test("dragging a watcher scrollbar reads history without claiming control or resizing", async ({ page }) => {
  const fixture = await mockFleet(page, { terminalOutput: history, terminalGeometry: { cols: 28, rows: 12 } });
  await page.goto(route);
  await expect(page.locator(".xterm-rows")).toContainText("LIVE_PROMPT");
  await dragHistoryToTop(page);
  await expect(page.locator(".xterm-rows")).toContainText("SCROLL_000");
  await expect(page.getByText("Watching", { exact: true })).toBeVisible();
  expect(fixture.terminalMessages).toEqual([]);
  expect(fixture.attachments).toHaveLength(1);
});

test("claiming from scrolled output preserves the viewed history through the resize acknowledgement", async ({ page }) => {
  const fixture = await mockFleet(page, { terminalOutput: history, terminalGeometry: { cols: 28, rows: 12 }, terminalOccupied: true });
  await page.goto(route);
  await expect(page.locator(".xterm-rows")).toContainText("LIVE_PROMPT");
  await dragHistoryToTop(page);
  await expect(page.locator(".xterm-rows")).toContainText("SCROLL_000");
  fixture.setTerminalOccupied(false);
  await expect(page.getByRole("button", { name: "Take control", exact: true })).toBeEnabled();
  // xterm paints rows with pointer-events:none; its screen owns real clicks.
  await page.locator(".xterm-screen").click({ position: { x: 10, y: 8 } });
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await expect.poll(() => fixture.terminalMessages.filter((item) => item.type === "resize").length).toBeGreaterThan(0);
  await expect(page.locator(".xterm-rows > div").first()).toContainText("SCROLL_000");
  expect(fixture.terminalMessages.filter((item) => item.type === "input")).toEqual([]);
  expect(fixture.terminalMessages.filter((item) => item.type === "claim")).toHaveLength(1);
  expect(fixture.attachments).toHaveLength(1);
});

for (const mouseReporting of [false, true]) {
  test(`an oversized alternate-screen watcher scrolls its viewport without input (${mouseReporting ? "mouse reporting" : "cursor mode"})`, async ({ page }) => {
    const cols = 150, rows = 70;
    const fixture = await mockFleet(page, { terminalOccupied: true, terminalGeometry: { cols, rows }, terminalOutput: `\u001b[?1049h${mouseReporting ? "\u001b[?1000h\u001b[?1006h" : ""}\u001b[2J\u001b[1;1HALT_TOP\u001b[${rows};1HALT_BOTTOM` });
    await page.goto(route);
    await expect(page.locator(".xterm-rows")).toContainText("ALT_BOTTOM");
    await page.locator(".terminal-canvas").hover();
    await page.mouse.wheel(0, 10000);
    await expect.poll(() => page.locator(".terminal-canvas").evaluate((canvas) => {
      const last = canvas.querySelector(".xterm-rows > div:last-child")!.getBoundingClientRect();
      const viewport = canvas.getBoundingClientRect();
      return canvas.scrollTop > 0 && last.bottom <= viewport.top + canvas.clientHeight + 1;
    })).toBe(true);
    await page.mouse.wheel(0, -10000);
    await expect.poll(() => page.locator(".terminal-canvas").evaluate((canvas) => canvas.scrollTop)).toBe(0);
    expect(fixture.terminalMessages).toEqual([]);
    expect(fixture.attachments).toHaveLength(1);
  });
}

test("an SSH setup reconnect does not steal focus, and explicit terminal input still works", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [{ ...remote, status: "disconnected" }], autoReconnect: true, deferSetupAttachments: 1 });
  await page.goto("/#host/dev");
  await page.getByRole("button", { name: "Open setup", exact: true }).click();
  const background = page.getByRole("button", { name: "Continue in background", exact: true });
  await background.focus();
  await expect.poll(() => fixture.attachments.filter((path) => path.endsWith("setup-terminal")).length).toBe(2);
  await expect(page.getByRole("region", { name: "SSH setup terminal", exact: true })).toContainText("Password:");
  await expect(background).toBeFocused();
  expect(fixture.terminalMessages.filter((item) => item.type === "input")).toEqual([]);
  await page.getByRole("region", { name: "SSH setup terminal", exact: true }).click();
  await page.keyboard.type("fixture input");
  await expect.poll(() => fixture.terminalMessages.filter((item) => item.type === "input").map((item) => item.data).join("")).toBe("fixture input");
});

test("a hidden owned pane ignores zero geometry and revealing it never steals focus", async ({ page }) => {
  const fixture = await mockFleet(page);
  await page.goto(route);
  await page.getByRole("button", { name: "Take control", exact: true }).click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await expect.poll(() => fixture.terminalMessages.filter((item) => item.type === "resize").length).toBeGreaterThan(0);
  const original = await page.locator(".xterm").elementHandle();
  await page.locator(".session-view").evaluate((node) => { node.hidden = true; node.style.display = "none"; node.inert = true; });
  const appearance = page.getByLabel("Appearance", { exact: true });
  await appearance.focus();
  const resizes = fixture.terminalMessages.filter((item) => item.type === "resize").length;
  await page.setViewportSize({ width: 710, height: 570 });
  await page.evaluate(() => new Promise<void>((resolve) => { let left = 6; const step = () => --left ? requestAnimationFrame(step) : resolve(); requestAnimationFrame(step); }));
  expect(fixture.terminalMessages.filter((item) => item.type === "resize")).toHaveLength(resizes);
  await page.locator(".session-view").evaluate((node) => { node.hidden = false; node.style.removeProperty("display"); node.inert = false; });
  await expect.poll(() => fixture.terminalMessages.filter((item) => item.type === "resize").length).toBeGreaterThan(resizes);
  await expect(appearance).toBeFocused();
  expect(await original!.evaluate((element) => element === document.querySelector(".xterm") && element.isConnected)).toBe(true);
  expect(fixture.terminalMessages.filter((item) => item.type === "claim")).toHaveLength(1);
  expect(fixture.terminalMessages.filter((item) => item.type === "input")).toEqual([]);
  expect(fixture.attachments).toHaveLength(1);
});
