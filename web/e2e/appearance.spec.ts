import { expect, test } from "@playwright/test";
import { mockFleet } from "./fixture";

const key = "relay.appearance.v1";

test("System follows live OS changes; manual choice persists and returning to System resumes following", async ({ page }, testInfo) => {
  const fixture = await mockFleet(page);
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto("/");
  await expect(page.getByLabel("Appearance", { exact: true })).toHaveValue("system");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  const light = await page.locator(".sidebar").evaluate((node) => getComputedStyle(node).backgroundColor);
  await page.screenshot({ path: testInfo.outputPath("fleet-light.png"), fullPage: true });
  await page.emulateMedia({ colorScheme: "dark" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  const dark = await page.locator(".sidebar").evaluate((node) => getComputedStyle(node).backgroundColor);
  expect(dark).not.toBe(light);
  await page.getByLabel("Appearance", { exact: true }).selectOption("light");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  await page.reload();
  await expect(page.getByLabel("Appearance", { exact: true })).toHaveValue("light");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  await page.emulateMedia({ colorScheme: "light" });
  await page.emulateMedia({ colorScheme: "dark" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  await page.getByLabel("Appearance", { exact: true }).selectOption("system");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await page.screenshot({ path: testInfo.outputPath("fleet-dark.png"), fullPage: true });
  expect(fixture.mutations).toEqual([]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBeTruthy();
});

test("invalid saved values use System and failed storage keeps the visible choice usable", async ({ page }) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.addInitScript((key) => {
    localStorage.setItem(key, 'not-a-theme');
    const write = Storage.prototype.setItem;
    Storage.prototype.setItem = function (name, value) { if (name === key) throw new Error("fixture storage denied"); return write.call(this, name, value); };
  }, key);
  await mockFleet(page);
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto("/");
  await expect(page.getByLabel("Appearance", { exact: true })).toHaveValue("system");
  await page.getByLabel("Appearance", { exact: true }).selectOption("dark");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expect(page.getByRole("status")).toContainText("could not be saved");
  await page.getByLabel("Dismiss appearance notice").click();
  await expect(page.getByText("could not be saved", { exact: false })).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "Fleet overview", exact: true })).toBeVisible();
  expect(errors).toEqual([]);
});

test("desktop preference is authoritative and rejected saves do not falsely change the mode", async ({ page }) => {
  await page.addInitScript(() => {
    const state = window as unknown as { __RELAY_APPEARANCE__: unknown; __TAURI_INTERNALS__: unknown; calls: unknown[] };
    state.__RELAY_APPEARANCE__ = { preference: "dark", resolved: "dark" };
    state.calls = [];
    state.__TAURI_INTERNALS__ = { invoke: async (command: string, args: { preference?: string }) => {
      state.calls.push({ command, args });
      if (args.preference === "light") throw new Error("fixture disk denied");
      return { preference: "dark", resolved: "dark" };
    } };
  });
  await page.emulateMedia({ colorScheme: "light" });
  const fixture = await mockFleet(page);
  await page.goto("/");
  await expect(page.getByLabel("Appearance", { exact: true })).toBeEnabled();
  await expect(page.getByLabel("Appearance", { exact: true })).toHaveValue("dark");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await page.getByLabel("Appearance", { exact: true }).selectOption("light");
  await expect(page.getByRole("status")).toContainText("Could not confirm");
  await expect(page.getByLabel("Appearance", { exact: true })).toHaveValue("dark");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  const calls = await page.evaluate(() => (window as unknown as { calls: unknown[] }).calls);
  expect(calls).toEqual([{ command: "appearance", args: {} }, { command: "appearance", args: { preference: "light" } }]);
  expect(fixture.mutations).toEqual([]);
});

test("new native theme events beat an earlier delayed read and reject malformed values", async ({ page }) => {
  await page.addInitScript(() => {
    const state = window as unknown as { __TAURI_INTERNALS__: unknown; finishAppearance: () => void };
    state.__TAURI_INTERNALS__ = { invoke: () => new Promise((resolve) => { state.finishAppearance = () => resolve({ preference: "system", resolved: "light" }); }) };
  });
  await mockFleet(page);
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto("/");
  await expect(page.getByLabel("Appearance", { exact: true })).toBeDisabled();
  await page.evaluate(() => {
    window.dispatchEvent(new CustomEvent("relay-appearance", { detail: { preference: "system", resolved: "dark" } }));
    (window as unknown as { finishAppearance: () => void }).finishAppearance();
  });
  await expect(page.getByLabel("Appearance", { exact: true })).toBeEnabled();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  // Embedded browser media can lag the native OS event. The desktop's shared
  // preference keeps content, title bar, and menu on the same resolved theme.
  await page.emulateMedia({ colorScheme: "dark" });
  await page.emulateMedia({ colorScheme: "light" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await page.evaluate(() => {
    window.dispatchEvent(new CustomEvent("relay-appearance", { detail: { preference: "light", resolved: "dark" } }));
    window.dispatchEvent(new CustomEvent("relay-appearance", { detail: { preference: "external", resolved: "light" } }));
  });
  await expect(page.getByLabel("Appearance", { exact: true })).toHaveValue("system");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
});

test("appearance remains available while the app needs to reconnect", async ({ page }) => {
  await mockFleet(page, { unauthorized: true });
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Reconnect Relay." })).toBeVisible();
  await page.getByLabel("Appearance", { exact: true }).selectOption("light");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  await page.getByLabel("Appearance", { exact: true }).selectOption("dark");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
});
