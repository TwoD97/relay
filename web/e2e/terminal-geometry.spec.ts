import { expect, test, type Page } from "@playwright/test";
import { mockFleet } from "./fixture";

const route = "/#host/dev/session/session-1";

async function geometry(page: Page) {
  return page.locator(".terminal-canvas").evaluate((canvas) => {
    const screen = canvas.querySelector(".xterm-screen") as HTMLElement;
    const style = getComputedStyle(canvas);
    const box = canvas.getBoundingClientRect();
    const grid = screen.getBoundingClientRect();
    return {
      right: grid.right, bottom: grid.bottom, width: grid.width, height: grid.height,
      availableRight: box.left + canvas.clientWidth - parseFloat(style.paddingRight),
      availableBottom: box.top + canvas.clientHeight - parseFloat(style.paddingBottom),
      visibleRight: box.left + canvas.clientWidth, visibleBottom: box.top + canvas.clientHeight,
      clientWidth: canvas.clientWidth, clientHeight: canvas.clientHeight,
      scrollWidth: canvas.scrollWidth, scrollHeight: canvas.scrollHeight,
      rows: canvas.querySelectorAll(".xterm-rows > div").length,
    };
  });
}

async function expectGridFits(page: Page) {
  await expect.poll(async () => {
    const box = await geometry(page);
    return { right: box.right <= box.availableRight + 1, bottom: box.bottom <= box.availableBottom + 1 };
  }).toEqual({ right: true, bottom: true });
}

async function settleFrames(page: Page) {
  await page.evaluate(() => new Promise<void>((resolve) => {
    let frames = 6;
    const next = () => --frames ? requestAnimationFrame(next) : resolve();
    requestAnimationFrame(next);
  }));
}

test("a controlling grid fits every cell inside the padded terminal at narrow and wide sizes", async ({ page }, testInfo) => {
  const fixture = await mockFleet(page, { terminalOutput: "GEOMETRY_PRESERVED\r\n" });
  await page.goto(route);
  await expect(page.locator(".xterm-rows")).toContainText("GEOMETRY_PRESERVED");
  await page.getByRole("button", { name: "Take control", exact: true }).click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  const background = await page.locator(".xterm-scrollable-element").evaluate((element) => getComputedStyle(element).backgroundColor);
  // The unused fraction of a row is painted by xterm's full-height viewport,
  // which must not retain the library's default black background in Light mode.
  await expect(page.locator(".xterm-viewport")).toHaveCSS("background-color", background);
  for (const size of [{ width: 1280, height: 900 }, { width: 730, height: 520 }, { width: 390, height: 844 }]) {
    await page.setViewportSize(size);
    await expectGridFits(page);
    const resize = fixture.terminalMessages.filter((message) => message.type === "resize").at(-1)!;
    expect(resize.cols).toBeGreaterThan(10);
    expect(resize.rows).toBeGreaterThan(5);
    if (testInfo.project.name === "desktop" && [1280, 390].includes(size.width)) await page.screenshot({ path: testInfo.outputPath(`terminal-${size.width}.png`), animations: "disabled" });
  }
  expect(fixture.attachments).toHaveLength(1);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
  await expect(page.locator(".xterm-rows")).toContainText("GEOMETRY_PRESERVED");
});

test("an oversized observer can reach the canonical bottom-right cell without resizing the PTY", async ({ page }) => {
  const cols = 150, rows = 65;
  const fixture = await mockFleet(page, { terminalOccupied: true, terminalGeometry: { cols, rows }, terminalOutput: `\u001b[2J\u001b[${rows};${cols - 7}HBOTTOM_R` });
  await page.goto(route);
  await expect(page.locator(".xterm-rows")).toContainText("BOTTOM_R");
  await page.locator(".terminal-canvas").hover();
  await page.mouse.wheel(10000, 10000);
  // At the scroll boundary, the last cell must be visible; the container's
  // trailing decorative padding need not remain in the viewport.
  await expect.poll(async () => {
    const box = await geometry(page);
    return { right: box.right <= box.visibleRight + 1, bottom: box.bottom <= box.visibleBottom + 1 };
  }).toEqual({ right: true, bottom: true });
  expect((await geometry(page)).rows).toBe(rows);
  expect(fixture.terminalMessages).toEqual([]);
  expect(fixture.attachments).toHaveLength(1);
});

test("canonical geometry is applied before snapshot text and wide Unicode stays on its intended row", async ({ page }) => {
  const fixture = await mockFleet(page, { terminalOccupied: true, terminalOutput: "INITIAL" });
  await page.goto(route);
  await expect(page.locator(".xterm-rows")).toContainText("INITIAL");
  for (const [cols, rows] of [[40, 12], [73, 23], [32, 16]]) {
    fixture.sendTerminalGeometry(cols, rows);
    fixture.sendTerminalOutput(`\u001b[2J\u001b[H${"A".repeat(cols - 2)}界\r\nSECOND_e\u0301\u001b[${rows};1HLAST_ROW`);
    await expect(page.locator(".xterm-rows > div")).toHaveCount(rows);
    await expect(page.locator(".xterm-rows > div").nth(0)).toHaveText(`${"A".repeat(cols - 2)}界`);
    await expect(page.locator(".xterm-rows > div").nth(1)).toContainText("SECOND_e\u0301");
    await expect(page.locator(".xterm-rows > div").nth(rows - 1)).toContainText("LAST_ROW");
  }
  expect(fixture.terminalMessages).toEqual([]);
  expect(fixture.attachments).toHaveLength(1);
});

test("hide, resize, and reveal refits the same owned terminal without losing output or replaying input", async ({ page }) => {
  const fixture = await mockFleet(page, { terminalOutput: "VISIBLE_AGAIN\r\n" });
  await page.goto(route);
  await page.getByRole("button", { name: "Take control", exact: true }).click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  const original = await page.locator(".xterm").elementHandle();
  await page.getByLabel("Build the dashboard terminal input", { exact: true }).focus();
  await page.keyboard.type("unfinished command");
  const inputs = fixture.terminalMessages.filter((message) => message.type === "input");
  await page.locator(".session-view").evaluate((element) => { element.style.display = "none"; });
  await page.setViewportSize({ width: 840, height: 610 });
  await settleFrames(page);
  await page.locator(".session-view").evaluate((element) => element.style.removeProperty("display"));
  await expectGridFits(page);
  await expect(page.locator(".xterm-rows")).toContainText("VISIBLE_AGAIN");
  await expect(page.locator(".xterm-rows")).toContainText("unfinished command");
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  expect(await original!.evaluate((element) => element === document.querySelector(".xterm") && element.isConnected)).toBe(true);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual(inputs);
  expect(fixture.terminalMessages.filter((message) => message.type === "claim")).toHaveLength(1);
  expect(fixture.attachments).toHaveLength(1);
});

test("fractional display scaling refits without dropping the writer lease or repainting away output", async ({ page, context }) => {
  const fixture = await mockFleet(page, { terminalOutput: "DPR_RENDERING_界_e\u0301\r\n" });
  await page.goto(route);
  await page.getByRole("button", { name: "Take control", exact: true }).click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  const original = await page.locator(".xterm").elementHandle();
  const viewport = page.viewportSize()!;
  const cdp = await context.newCDPSession(page);
  for (const scale of [1.25, 1.5, 2, 1]) {
    await cdp.send("Emulation.setDeviceMetricsOverride", { ...viewport, deviceScaleFactor: scale, mobile: false });
    await expect.poll(() => page.evaluate(() => window.devicePixelRatio)).toBe(scale);
    await settleFrames(page);
    await expectGridFits(page);
    await expect(page.locator(".xterm-rows")).toContainText("DPR_RENDERING_界_e\u0301");
    await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  }
  expect(await original!.evaluate((element) => element === document.querySelector(".xterm") && element.isConnected)).toBe(true);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
  expect(fixture.attachments).toHaveLength(1);
});

test("a second narrow viewer keeps the owner's canonical geometry through its own viewport changes", async ({ page, context }) => {
  const owner = await mockFleet(page, { terminalOutput: "SHARED_SCREEN\r\n" });
  await page.goto(route);
  await page.getByRole("button", { name: "Take control", exact: true }).click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await expect.poll(() => owner.terminalMessages.filter((message) => message.type === "resize").length).toBeGreaterThan(0);
  const dimensions = owner.terminalMessages.filter((message) => message.type === "resize").at(-1)!;
  const watcher = await context.newPage();
  try {
    await watcher.setViewportSize({ width: 390, height: 650 });
    const observed = await mockFleet(watcher, { terminalOccupied: true, terminalGeometry: { cols: dimensions.cols!, rows: dimensions.rows! }, terminalOutput: "SHARED_SCREEN\r\n" });
    await watcher.goto(route);
    await expect(watcher.locator(".xterm-rows")).toContainText("SHARED_SCREEN");
    const original = await geometry(watcher);
    await watcher.setViewportSize({ width: 1024, height: 760 });
    await settleFrames(watcher);
    expect((await geometry(watcher)).width).toBe(original.width);
    expect((await geometry(watcher)).rows).toBe(original.rows);
    expect(observed.terminalMessages).toEqual([]);
    await page.setViewportSize({ width: 950, height: 600 });
    await expectGridFits(page);
    await settleFrames(page);
    const changed = owner.terminalMessages.filter((message) => message.type === "resize").at(-1)!;
    observed.sendTerminalGeometry(changed.cols!, changed.rows!);
    await expect(watcher.locator(".xterm-rows > div")).toHaveCount(changed.rows!);
    expect(observed.terminalMessages).toEqual([]);
    expect(observed.attachments).toHaveLength(1);
  } finally { await watcher.close(); }
});

test("scrollback remains reachable after observer geometry changes", async ({ page }) => {
  const history = Array.from({ length: 140 }, (_, index) => `HISTORY_${String(index).padStart(3, "0")} 界\r\n`).join("") + "CURRENT_PROMPT";
  const fixture = await mockFleet(page, { terminalOccupied: true, terminalGeometry: { cols: 48, rows: 12 }, terminalOutput: history });
  await page.goto(route);
  await expect(page.locator(".xterm-rows")).toContainText("CURRENT_PROMPT");
  await page.locator(".xterm-screen").hover();
  // xterm normalizes each wheel gesture to a small number of rows, regardless
  // of a very large pixel delta. Exercise repeated user scrolling, not internals.
  for (let i = 0; i < 150; i++) await page.mouse.wheel(0, -200);
  await expect(page.locator(".xterm-rows")).toContainText("HISTORY_000");
  fixture.sendTerminalGeometry(65, 18);
  await expect(page.locator(".xterm-rows > div")).toHaveCount(18);
  for (let i = 0; i < 150; i++) await page.mouse.wheel(0, 200);
  await expect(page.locator(".xterm-rows")).toContainText("CURRENT_PROMPT");
  for (let i = 0; i < 150; i++) await page.mouse.wheel(0, -200);
  await expect(page.locator(".xterm-rows")).toContainText("HISTORY_000");
  expect(fixture.terminalMessages).toEqual([]);
  expect(fixture.attachments).toHaveLength(1);
});

test("font controls persist and repaint a watcher without claiming or resizing remote sessions", async ({ page }) => {
  const fixture = await mockFleet(page, { terminalOccupied: true, terminalOutput: "FONT_PRESERVED_界\r\n" });
  await page.goto(route);
  const original = await page.locator(".xterm").elementHandle();
  const before = await geometry(page);
  await page.getByRole("button", { name: "Increase terminal font size", exact: true }).click();
  await page.getByRole("button", { name: "Increase terminal font size", exact: true }).click();
  await expect(page.locator(".xterm-rows")).toHaveCSS("font-size", "16px");
  expect((await geometry(page)).width).toBeGreaterThan(before.width);
  await expect(page.locator(".xterm-rows")).toContainText("FONT_PRESERVED_界");
  expect(await original!.evaluate((element) => element === document.querySelector(".xterm") && element.isConnected)).toBe(true);
  expect(fixture.terminalMessages).toEqual([]);
  expect(await page.evaluate(() => localStorage.getItem("relay.terminal.fontSize.v1"))).toBe("16");
  await page.reload();
  await expect(page.locator(".xterm-rows")).toHaveCSS("font-size", "16px");
  await page.getByRole("button", { name: "Reset terminal font size", exact: true }).click();
  await expect(page.locator(".xterm-rows")).toHaveCSS("font-size", "14px");
  expect(fixture.terminalMessages).toEqual([]);
});

test("font controls keep an owner fitted and bounded while preserving partial input", async ({ page }) => {
  const fixture = await mockFleet(page, { terminalOutput: "FONT_OWNER\r\n" });
  await page.goto(route);
  await page.getByRole("button", { name: "Take control", exact: true }).click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await page.keyboard.type("not submitted");
  const input = fixture.terminalMessages.filter((message) => message.type === "input");
  const increase = page.getByRole("button", { name: "Increase terminal font size", exact: true });
  for (let size = 14; size < 24; size++) await increase.click();
  await expect(increase).toBeDisabled();
  await expect(page.locator(".xterm-rows")).toHaveCSS("font-size", "24px");
  await expectGridFits(page);
  const decrease = page.getByRole("button", { name: "Decrease terminal font size", exact: true });
  for (let size = 24; size > 10; size--) await decrease.click();
  await expect(decrease).toBeDisabled();
  await expect(page.locator(".xterm-rows")).toHaveCSS("font-size", "10px");
  await expectGridFits(page);
  await expect(page.locator(".xterm-rows")).toContainText("not submitted");
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual(input);
  expect(fixture.terminalMessages.filter((message) => message.type === "claim")).toHaveLength(1);
  expect(fixture.attachments).toHaveLength(1);
});

test("font preferences remain usable when local storage cannot save", async ({ page }) => {
  await page.addInitScript(() => { Storage.prototype.setItem = () => { throw new DOMException("Fixture storage unavailable", "QuotaExceededError"); }; });
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const fixture = await mockFleet(page, { terminalOccupied: true });
  await page.goto(route);
  await page.getByRole("button", { name: "Increase terminal font size", exact: true }).click();
  await expect(page.locator(".xterm-rows")).toHaveCSS("font-size", "15px");
  expect(errors).toEqual([]);
  expect(fixture.terminalMessages).toEqual([]);
});

test("an owner waits for confirmed resize geometry and parses intervening output at the old size", async ({ page }) => {
  const initial = { cols: 120, rows: 32 };
  const fixture = await mockFleet(page, { terminalGeometry: initial, terminalOutput: "WAIT_FOR_ACK", deferTerminalResize: true });
  await page.goto(route);
  await expect(page.locator(".xterm-rows")).toContainText("WAIT_FOR_ACK");
  const before = await geometry(page);
  await page.getByRole("button", { name: "Take control", exact: true }).click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await expect.poll(() => fixture.terminalMessages.filter((message) => message.type === "resize").length).toBe(1);
  const proposal = fixture.terminalMessages.find((message) => message.type === "resize")!;
  await settleFrames(page);
  expect((await geometry(page)).width).toBe(before.width);
  expect((await geometry(page)).rows).toBe(initial.rows);
  fixture.sendTerminalOutput(`\u001b[2J\u001b[H${"X".repeat(initial.cols - 2)}界\r\nOLD_GEOMETRY`);
  await expect(page.locator(".xterm-rows > div").first()).toHaveText(`${"X".repeat(initial.cols - 2)}界`);
  await expect(page.locator(".xterm-rows > div").nth(1)).toContainText("OLD_GEOMETRY");
  fixture.sendTerminalGeometry(proposal.cols!, proposal.rows!);
  await expectGridFits(page);
  await expect(page.locator(".xterm-rows > div")).toHaveCount(proposal.rows!);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
  expect(fixture.attachments).toHaveLength(1);
});

test("a rejected resize keeps confirmed geometry without resending forever", async ({ page }) => {
  const initial = { cols: 83, rows: 21 };
  const fixture = await mockFleet(page, { terminalGeometry: initial, terminalOutput: "RESIZE_REJECTED", deferTerminalResize: true });
  await page.goto(route);
  await expect(page.locator(".xterm-rows")).toContainText("RESIZE_REJECTED");
  const before = await geometry(page);
  await page.getByRole("button", { name: "Take control", exact: true }).click();
  await expect.poll(() => fixture.terminalMessages.filter((message) => message.type === "resize").length).toBe(1);
  // A failed ioctl produces the actual unchanged PTY dimensions, never the
  // requested dimensions. Repeating that metadata must not form a retry loop.
  fixture.sendTerminalGeometry(initial.cols, initial.rows, "The terminal rejected the requested size.");
  await settleFrames(page);
  fixture.sendTerminalGeometry(initial.cols, initial.rows, "The terminal rejected the requested size.");
  await settleFrames(page);
  expect((await geometry(page)).width).toBe(before.width);
  expect((await geometry(page)).rows).toBe(initial.rows);
  expect(fixture.terminalMessages.filter((message) => message.type === "resize")).toHaveLength(1);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
  await expect(page.locator(".xterm-rows")).toContainText("RESIZE_REJECTED");
  await expect(page.getByText("The terminal rejected the requested size.", { exact: true })).toBeVisible();
});

test("an extreme viewport and minimum font stay within the runtime resize limits", async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem("relay.terminal.fontSize.v1", "10"));
  await page.setViewportSize({ width: 5000, height: 4200 });
  const fixture = await mockFleet(page, { terminalOutput: "LARGE_CANVAS" });
  await page.goto(route);
  await page.getByRole("button", { name: "Take control", exact: true }).click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await expect(page.locator(".terminal-shell")).toHaveAttribute("data-terminal-cols", "500");
  await expect(page.locator(".terminal-shell")).toHaveAttribute("data-terminal-rows", "300");
  await expect(page.getByRole("status", { name: "Terminal dimensions" })).toHaveText("500 × 300");
  await expectGridFits(page);
  await expect(page.locator(".xterm-rows")).toContainText("LARGE_CANVAS");
  expect(fixture.terminalMessages.filter((message) => message.type === "resize").every((message) => message.cols! >= 2 && message.cols! <= 500 && message.rows! >= 2 && message.rows! <= 300)).toBe(true);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
});
