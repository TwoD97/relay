import { expect, test, type Page } from "@playwright/test";
import { mockFleet } from "./fixture";

type CopyProbe = { native: string[]; async: string[]; mode: string };
async function probe(page: Page, mode = "native") {
  await page.addInitScript((mode) => {
    const probe = { native: [] as string[], async: [] as string[], mode };
    (window as unknown as { copyProbe: CopyProbe }).copyProbe = probe;
    document.execCommand = (command) => {
      if (command !== "copy") return false;
      const field = document.activeElement;
      if (!(field instanceof HTMLTextAreaElement)) throw new Error("Copy field must have focus");
      probe.native.push(field.value);
      return probe.mode === "native";
    };
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (text: string) => {
      probe.async.push(text);
      if (probe.mode === "denied") throw new Error("Denied");
    } } });
  }, mode);
}
const read = (page: Page) => page.evaluate(() => (window as unknown as { copyProbe: CopyProbe }).copyProbe);
async function select(page: Page, count = 7) {
  const shell = page.locator(".terminal-shell");
  const cols = Number(await shell.getAttribute("data-terminal-cols"));
  const screen = (await shell.locator(".xterm-screen").boundingBox())!;
  const row = (await shell.locator(".xterm-rows > div").first().boundingBox())!;
  await page.mouse.move(screen.x + 1, row.y + row.height / 2);
  await page.mouse.down();
  await page.mouse.move(screen.x + count * screen.width / cols + 1, row.y + row.height / 2, { steps: count });
  expect((await read(page)).native).toEqual([]);
  await page.mouse.up();
}

test("finishing a text selection copies once, keeps focus and never claims an occupied terminal", async ({ page }) => {
  await probe(page);
  const fixture = await mockFleet(page, { terminalOutput: "COPY_ME\r\n$ ", terminalOccupied: true });
  await page.goto("/#host/dev/session/session-1");
  await expect(page.locator(".xterm-rows")).toContainText("COPY_ME");
  await select(page);
  await expect.poll(async () => (await read(page)).native).toEqual(["COPY_ME"]);
  expect((await read(page)).async).toEqual([]);
  await expect(page.getByRole("button", { name: "Copy terminal selection" })).toHaveText("Copied");
  await expect(page.locator(".xterm-helper-textarea")).toBeFocused();
  expect(fixture.terminalMessages).toEqual([]);
  // Explicit copy remains available to repeat a successful auto-copy.
  await page.getByRole("button", { name: "Copy terminal selection" }).click();
  expect((await read(page)).native).toEqual(["COPY_ME", "COPY_ME"]);
  expect(fixture.terminalMessages).toEqual([]);
});

test("native denial falls back to async copy and failures stay visible to a watcher", async ({ page }) => {
  await probe(page, "denied");
  const fixture = await mockFleet(page, { terminalOutput: "COPY_ME\r\n$ ", terminalOccupied: true });
  await page.goto("/#host/dev/session/session-1");
  await expect(page.locator(".xterm-rows")).toContainText("COPY_ME");
  await select(page);
  await expect(page.getByRole("alert").filter({ hasText: "Could not copy the selection" })).toBeVisible();
  expect((await read(page)).async).toEqual(["COPY_ME"]);
  await page.evaluate(() => { (window as unknown as { copyProbe: CopyProbe }).copyProbe.mode = "async"; });
  await page.getByRole("button", { name: "Copy terminal selection" }).click();
  await expect(page.getByRole("alert").filter({ hasText: "Could not copy the selection" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Copy terminal selection" })).toHaveText("Copied");
  expect(fixture.terminalMessages).toEqual([]);
});

test("double-click selects and copies a word, while a plain click and output do not copy", async ({ page }) => {
  await probe(page);
  const fixture = await mockFleet(page, { terminalOutput: "COPY_ME other\r\n$ ", terminalOccupied: true });
  await page.goto("/#host/dev/session/session-1");
  await expect(page.locator(".xterm-rows")).toContainText("COPY_ME");
  const screen = page.locator(".xterm-screen");
  await screen.click({ position: { x: 20, y: 8 } });
  expect((await read(page)).native).toEqual([]);
  await screen.dblclick({ position: { x: 20, y: 8 } });
  await expect.poll(async () => (await read(page)).native).toEqual(["COPY_ME"]);
  await page.getByLabel("Appearance", { exact: true }).selectOption("light");
  expect((await read(page)).native).toEqual(["COPY_ME"]);
  expect(fixture.terminalMessages).toEqual([]);
});

test("Ctrl+C with no selection still interrupts the controlled process", async ({ page }) => {
  await probe(page);
  const fixture = await mockFleet(page, { terminalOutput: "$ " });
  await page.goto("/#host/dev/session/session-1");
  await page.locator(".terminal-canvas").click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await page.keyboard.press("Control+c");
  await expect.poll(() => fixture.terminalMessages.filter((item) => item.type === "input").map((item) => item.data).join("")).toBe("\u0003");
  expect((await read(page)).native).toEqual([]);
});
