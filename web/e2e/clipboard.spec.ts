import { expect, test, type Page } from "@playwright/test";
import { mockFleet } from "./fixture";

async function selectFirstWord(page: Page, columns: number, length: number) {
  const screen = await page.locator(".xterm-screen").boundingBox();
  const row = await page.locator(".xterm-rows > div").first().boundingBox();
  if (!screen || !row) throw new Error("The terminal renderer is unavailable");
  await page.mouse.move(screen.x + 1, row.y + row.height / 2);
  await page.mouse.down();
  await page.mouse.move(screen.x + length * screen.width / columns + 1, row.y + row.height / 2, { steps: length });
  await page.mouse.up();
}

test("standard paste shortcuts send clipboard text with bracketed paste, never the agent image shortcut", async ({ page }) => {
  const fixture = await mockFleet(page, { terminalOutput: "\u001b[?2004hReady\r\n$ " });
  await page.goto("/#host/dev/session/session-1");
  const text = "printf '%s' '$HOME; `literal`'\nnext\tline ✓";
  await page.evaluate((value) => {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { readText: async () => value } });
  }, text);
  await page.locator(".terminal-canvas").click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  for (const shortcut of ["Control+v", "Control+Shift+V", "Meta+v"]) await page.keyboard.press(shortcut);
  await page.getByRole("button", { name: "Paste text into terminal" }).click();
  await expect.poll(() => fixture.terminalMessages.filter((message) => message.type === "input").map((message) => message.data).join("")).toBe(`\u001b[200~${text.replace(/\n/g, "\r")}\u001b[201~`.repeat(4));
  await expect(page.locator(".xterm-helper-textarea")).toBeFocused();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBeTruthy();
});

test("Ctrl+C copies a terminal selection and still interrupts when nothing is selected", async ({ page }) => {
  const fixture = await mockFleet(page, { terminalOutput: "COPY_ME\r\n$ " });
  await page.goto("/#host/dev/session/session-1");
  await page.evaluate(() => {
    const state = window as unknown as { copied: string[] };
    state.copied = [];
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (text: string) => { state.copied.push(text); } } });
  });
  await page.locator(".terminal-canvas").click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await page.keyboard.press("Control+c");
  await expect.poll(() => fixture.terminalMessages.filter((message) => message.type === "input").map((message) => message.data).join("")).toBe("\u0003");
  const columns = fixture.terminalMessages.filter((message) => message.type === "resize").at(-1)!.cols!;
  await selectFirstWord(page, columns, 7);
  await expect(page.getByRole("button", { name: "Copy terminal selection" })).toBeEnabled();
  await page.keyboard.press("Control+c");
  await page.getByRole("button", { name: "Copy terminal selection" }).click();
  await expect.poll(() => page.evaluate(() => (window as unknown as { copied: string[] }).copied)).toEqual(["COPY_ME", "COPY_ME"]);
  expect(fixture.terminalMessages.filter((message) => message.type === "input").map((message) => message.data).join("")).toBe("\u0003");
});

test("copy fallback selects the exact terminal text without sending input", async ({ page }) => {
  const fixture = await mockFleet(page, { terminalOutput: "COPY_ME\r\n$ " });
  await page.goto("/#host/dev/session/session-1");
  await page.evaluate(() => {
    const state = window as unknown as { copied: string[] };
    state.copied = [];
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
    document.execCommand = (command) => {
      if (command !== "copy") return false;
      const selection = document.activeElement as HTMLTextAreaElement;
      state.copied.push(selection.value.slice(selection.selectionStart, selection.selectionEnd));
      return true;
    };
  });
  await page.locator(".terminal-canvas").click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  const columns = fixture.terminalMessages.filter((message) => message.type === "resize").at(-1)!.cols!;
  await selectFirstWord(page, columns, 7);
  await page.keyboard.press("Control+c");
  await expect.poll(() => page.evaluate(() => (window as unknown as { copied: string[] }).copied)).toEqual(["COPY_ME"]);
  await expect(page.locator(".xterm-helper-textarea")).toBeFocused();
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
});

test("an occupied terminal cannot read or paste clipboard contents", async ({ page }) => {
  const fixture = await mockFleet(page, { terminalOccupied: true });
  await page.goto("/#host/dev/session/session-1");
  await page.evaluate(() => {
    const state = window as unknown as { reads: number };
    state.reads = 0;
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { readText: async () => { state.reads++; return "must not leave this browser"; } } });
  });
  await expect(page.getByRole("button", { name: "Paste text into terminal" })).toBeDisabled();
  await page.locator(".terminal-canvas").click();
  await page.keyboard.press("Control+v");
  await page.keyboard.press("Control+Shift+V");
  expect(await page.evaluate(() => (window as unknown as { reads: number }).reads)).toBe(0);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
});

test("the real browser clipboard supports text paste and copying terminal output", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  const fixture = await mockFleet(page, { terminalOutput: "COPY_ME\r\n$ " });
  await page.goto("/#host/dev/session/session-1");
  await page.evaluate(() => navigator.clipboard.writeText("clipboard text ✓"));
  await page.locator(".terminal-canvas").click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await page.keyboard.press("Control+v");
  await expect.poll(() => fixture.terminalMessages.filter((message) => message.type === "input").map((message) => message.data).join("")).toBe("clipboard text ✓");
  const columns = fixture.terminalMessages.filter((message) => message.type === "resize").at(-1)!.cols!;
  await selectFirstWord(page, columns, 7);
  await page.keyboard.press("Control+c");
  await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe("COPY_ME");
});
