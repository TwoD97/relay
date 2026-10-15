import { expect, test, type Page } from "@playwright/test";
import { mockFleet } from "./fixture";

const ansiCodes = [...Array.from({ length: 8 }, (_, i) => 30 + i), ...Array.from({ length: 8 }, (_, i) => 90 + i)];
const coloredOutput = "KEEP_ME\r\n" + ansiCodes.map((code, index) => `\u001b[${code}m${index.toString().padStart(2, "0")}\u001b[0m `).join("") + "\r\n\u001b[38;2;18;96;144mTRUE_COLOR\u001b[0m\r\n$ ";

async function palette(page: Page) {
  return page.locator(".xterm-rows").evaluate((rows) => Array.from({ length: 16 }, (_, index) => {
    const cell = rows.querySelector(`.xterm-fg-${index}`);
    if (!cell) throw new Error(`Missing rendered ANSI color ${index}`);
    return getComputedStyle(cell).color;
  }));
}

async function selectFirstWord(page: Page, columns: number) {
  const screen = await page.locator(".xterm-screen").boundingBox();
  const row = await page.locator(".xterm-rows > div").first().boundingBox();
  if (!screen || !row) throw new Error("Terminal output has not rendered");
  await page.mouse.move(screen.x + 1, row.y + row.height / 2);
  await page.mouse.down();
  await page.mouse.move(screen.x + 7 * screen.width / columns + 1, row.y + row.height / 2, { steps: 7 });
  await page.mouse.up();
}

test("system appearance repaints all ANSI colors without replacing output, selection, focus, or the writer lease", async ({ page }, testInfo) => {
  await page.emulateMedia({ colorScheme: "dark" });
  const fixture = await mockFleet(page, { terminalOutput: coloredOutput });
  await page.goto("/#host/dev/session/session-1");
  await expect(page.getByLabel("Appearance", { exact: true })).toHaveValue("system");
  await expect(page.locator(".xterm-rows")).toContainText("KEEP_ME");
  await expect(page.locator(".xterm-scrollable-element")).toHaveCSS("background-color", "rgb(13, 17, 16)");
  const originalTerminal = await page.locator(".xterm").elementHandle();
  const originalInput = await page.locator(".xterm-helper-textarea").elementHandle();
  if (!originalTerminal || !originalInput) throw new Error("Terminal has not mounted");
  await page.evaluate(() => {
    const state = window as unknown as { copied: string[] };
    state.copied = [];
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (value: string) => { state.copied.push(value); } } });
  });
  await page.locator(".terminal-canvas").click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await page.keyboard.type("partial command");
  await expect(page.locator(".xterm-rows")).toContainText("partial command");
  const initialPalette = await palette(page);
  const before = structuredClone(fixture.terminalMessages);
  const columns = before.filter((message) => message.type === "resize").at(-1)!.cols!;
  await selectFirstWord(page, columns);
  await expect(page.getByRole("button", { name: "Copy terminal selection" })).toBeEnabled();
  await expect(page.locator(".xterm-helper-textarea")).toBeFocused();

  await page.emulateMedia({ colorScheme: "light" });
  await expect(page.locator(".xterm-scrollable-element")).toHaveCSS("background-color", "rgb(251, 252, 249)");
  await expect(page.locator(".xterm-rows")).toHaveCSS("color", "rgb(38, 51, 40)");
  await expect(page.locator(".xterm-selection > div").first()).toHaveCSS("background-color", "rgb(199, 221, 183)");
  const lightPalette = await palette(page);
  expect(lightPalette.every((color, index) => color !== initialPalette[index])).toBeTruthy();
  expect(lightPalette[1]).toBe("rgb(170, 52, 52)");
  expect(lightPalette[14]).toBe("rgb(40, 119, 126)");
  await expect(page.locator(".xterm-rows span").filter({ hasText: /^TRUE_COLOR$/ })).toHaveCSS("color", "rgb(18, 96, 144)");
  expect(await originalTerminal.evaluate((node) => node.isConnected && node === document.querySelector(".xterm"))).toBeTruthy();
  expect(await originalInput.evaluate((node) => node === document.activeElement)).toBeTruthy();
  await expect(page.locator(".xterm-rows")).toContainText("partial command");
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  expect(fixture.attachments).toHaveLength(1);
  expect(fixture.terminalMessages).toEqual(before);
  await page.screenshot({ path: testInfo.outputPath("terminal-light.png") });

  await page.emulateMedia({ colorScheme: "dark" });
  await expect(page.locator(".xterm-scrollable-element")).toHaveCSS("background-color", "rgb(13, 17, 16)");
  expect(await palette(page)).toEqual(initialPalette);
  await expect(page.locator(".xterm-selection > div").first()).toHaveCSS("background-color", "rgb(71, 86, 58)");
  await expect(page.locator(".xterm-helper-textarea")).toBeFocused();
  await page.keyboard.press("Control+c");
  await expect.poll(() => page.evaluate(() => (window as unknown as { copied: string[] }).copied)).toEqual(["KEEP_ME"]);
  expect(fixture.terminalMessages).toEqual(before);
  await page.keyboard.press("ArrowLeft");
  await page.keyboard.press("Backspace");
  await page.keyboard.type("!");
  await expect.poll(() => fixture.terminalMessages.filter((message) => message.type === "input").map((message) => message.data).join("")).toBe("partial command\u001b[D\u007f!");
  expect(fixture.terminalMessages.filter((message) => message.type !== "input")).toEqual(before.filter((message) => message.type !== "input"));
  expect(fixture.attachments).toHaveLength(1);
  await page.screenshot({ path: testInfo.outputPath("terminal-dark.png") });
});

test("explicit appearance changes repaint a watcher without claiming or resizing the terminal", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "dark" });
  const fixture = await mockFleet(page, { terminalOccupied: true, terminalOutput: coloredOutput });
  await page.goto("/#host/dev/session/session-1");
  await expect(page.getByText("Watching", { exact: true })).toBeVisible();
  const originalTerminal = await page.locator(".xterm").elementHandle();
  if (!originalTerminal) throw new Error("Terminal has not mounted");
  const width = await page.locator(".xterm-screen").evaluate((node) => node.getBoundingClientRect().width);
  const appearance = page.getByLabel("Appearance", { exact: true });
  await appearance.selectOption("light");
  await expect(page.locator(".xterm-scrollable-element")).toHaveCSS("background-color", "rgb(251, 252, 249)");
  await appearance.selectOption("dark");
  await expect(page.locator(".xterm-scrollable-element")).toHaveCSS("background-color", "rgb(13, 17, 16)");
  await page.emulateMedia({ colorScheme: "light" });
  await expect(page.locator(".xterm-scrollable-element")).toHaveCSS("background-color", "rgb(13, 17, 16)");
  await appearance.selectOption("system");
  await expect(page.locator(".xterm-scrollable-element")).toHaveCSS("background-color", "rgb(251, 252, 249)");
  expect(await originalTerminal.evaluate((node) => node.isConnected && node === document.querySelector(".xterm"))).toBeTruthy();
  expect(await page.locator(".xterm-screen").evaluate((node) => node.getBoundingClientRect().width)).toBe(width);
  await expect(page.locator(".xterm-rows")).toContainText("KEEP_ME");
  await expect(page.getByRole("button", { name: "Take control", exact: true })).toBeDisabled();
  expect(fixture.attachments).toHaveLength(1);
  expect(fixture.terminalMessages).toEqual([]);
});

test("SSH setup adopts live system appearance without reconnecting or replaying typed input", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "light" });
  const fixture = await mockFleet(page, { hosts: [], sessions: {} });
  await page.goto("/");
  await page.getByRole("button", { name: "Connect your first machine" }).click();
  await page.getByLabel("SSH destination").fill("deploy@example.test");
  await page.getByRole("dialog").getByRole("button", { name: "Connect machine", exact: true }).click();
  await expect(page.getByRole("region", { name: "SSH setup terminal" })).toBeVisible();
  await expect(page.locator(".xterm-scrollable-element")).toHaveCSS("background-color", "rgb(251, 252, 249)");
  await page.getByLabel("SSH setup terminal input", { exact: true }).focus();
  await page.keyboard.type("fixture input");
  await expect(page.locator(".xterm-rows")).toContainText("fixture input");
  const before = structuredClone(fixture.terminalMessages);
  await page.emulateMedia({ colorScheme: "dark" });
  await expect(page.locator(".xterm-scrollable-element")).toHaveCSS("background-color", "rgb(13, 17, 16)");
  await expect(page.getByLabel("SSH setup terminal input", { exact: true })).toBeFocused();
  await expect(page.locator(".xterm-rows")).toContainText("fixture input");
  expect(fixture.attachments).toHaveLength(1);
  expect(fixture.terminalMessages).toEqual(before);
});
