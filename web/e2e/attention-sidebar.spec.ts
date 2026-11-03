import { expect, test, type Page } from "@playwright/test";
import { mockFleet, openNavigation, remote, work } from "./fixture";
import type { Session } from "../src/types";

const key = "relay.attention-collapsed.v1";
const attention = (page: Page) => page.getByRole("region", { name: "Needs attention", exact: true });
const toggle = (page: Page) => attention(page).getByRole("button", { name: /^(Expand|Collapse) needs attention$/ });
const notice = (id: number): Session => ({ ...work, id: `notice-${id}`, title: `Notification ${id}`, attention: { kind: "notification", source: "claude-hook", updatedAt: "2026-11-03T08:00:00Z" } });
async function open(page: Page) { await page.goto("/"); await openNavigation(page); await expect(toggle(page)).toBeVisible(); }

test("collapsing Needs attention recovers machine space and preserves the choice across reload", async ({ page }, testInfo) => {
  const fixture = await mockFleet(page, { hosts: [structuredClone(remote)], sessions: { dev: [] } });
  await open(page);
  const tree = page.getByRole("navigation", { name: "Machines and workspaces", exact: true });
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "true");
  const expanded = await tree.boundingBox();
  const controls = await toggle(page).getAttribute("aria-controls");
  expect(controls).toBeTruthy();
  await expect(attention(page).locator(".attention-list")).toHaveAttribute("id", controls!);
  await toggle(page).click();
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "false");
  await expect(attention(page).locator(".attention-list")).toBeHidden();
  await expect(attention(page).getByText("0", { exact: true })).toBeVisible();
  await expect.poll(async () => expanded!.y - (await tree.boundingBox())!.y).toBeGreaterThan(80);
  await expect.poll(() => page.evaluate((name) => localStorage.getItem(name), key)).toBe("true");
  await page.screenshot({ path: testInfo.outputPath("attention-collapsed.png"), animations: "disabled" });
  await page.reload(); await openNavigation(page);
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "false");
  await toggle(page).click();
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "true");
  await expect.poll(async () => Math.abs(expanded!.y - (await tree.boundingBox())!.y)).toBeLessThan(1);
  await expect.poll(() => page.evaluate((name) => localStorage.getItem(name), key)).toBe("false");
  await page.reload(); await openNavigation(page);
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "true");
  expect(fixture.mutations).toEqual([]);
});

test("new notices update the collapsed count without expanding or moving the machine tree", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [structuredClone(remote)], sessions: { dev: [notice(1)] } });
  await open(page);
  await expect(attention(page).getByText("1", { exact: true })).toBeVisible();
  await toggle(page).click();
  const tree = page.getByRole("navigation", { name: "Machines and workspaces", exact: true });
  const before = await tree.boundingBox();
  fixture.sessions.dev.push(notice(2), notice(3));
  await expect(attention(page).getByText("3", { exact: true })).toBeVisible();
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "false");
  await expect(attention(page).locator(".attention-list")).toBeHidden();
  expect((await tree.boundingBox())!.y).toBe(before!.y);
  await page.getByRole("textbox", { name: "Filter machines and sessions", exact: true }).fill("no matching project");
  await expect(attention(page).getByText("3", { exact: true })).toBeVisible();
  await toggle(page).click();
  const items = attention(page).locator(".attention-list").getByRole("button");
  await expect(items).toHaveCount(3);
  await items.last().click();
  await expect(page).toHaveURL(/#host\/dev\/session\/notice-3$/);
  expect(fixture.mutations).toEqual([]);
});

test("Needs attention is keyboard operable and hidden notices are removed from keyboard navigation", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [structuredClone(remote)], sessions: { dev: [notice(1)] } });
  await open(page);
  await toggle(page).focus();
  await page.keyboard.press("Space");
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "false");
  await expect(toggle(page)).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(page.getByRole("button", { name: "Add machine", exact: true })).toBeFocused();
  await page.keyboard.press("Shift+Tab");
  await expect(toggle(page)).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "true");
  await page.keyboard.press("Tab");
  await expect(attention(page).locator(".attention-list")).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(attention(page).locator(".attention-list").getByRole("button")).toBeFocused();
  expect(fixture.mutations).toEqual([]);
});

test("invalid saved attention state uses the expanded default and can be replaced", async ({ page }) => {
  await page.addInitScript(({ name }) => localStorage.setItem(name, '{"collapsed":true}'), { name: key });
  await mockFleet(page);
  await open(page);
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "true");
  await toggle(page).click();
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "false");
  expect(await page.evaluate((name) => localStorage.getItem(name), key)).toBe("true");
});

test("blocked attention storage leaves the toggle usable and visibly reports that persistence failed", async ({ page }) => {
  await page.addInitScript(({ name }) => {
    const read = Storage.prototype.getItem, write = Storage.prototype.setItem;
    Storage.prototype.getItem = function (key) { if (key === name) throw new DOMException("Fixture storage unavailable", "SecurityError"); return read.call(this, key); };
    Storage.prototype.setItem = function (key, value) { if (key === name) throw new DOMException("Fixture storage unavailable", "SecurityError"); return write.call(this, key, value); };
  }, { name: key });
  const fixture = await mockFleet(page);
  await open(page);
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "true");
  await toggle(page).click();
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "false");
  await expect(attention(page).getByRole("status")).toBeVisible();
  await expect(attention(page).getByRole("status")).toContainText(/sav|remember/i);
  await toggle(page).click();
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "true");
  await expect(attention(page).locator(".attention-list")).toBeVisible();
  expect(fixture.mutations).toEqual([]);
});

test("native initial state is immediate and serialized saves cannot be overwritten by a stale read", async ({ page }) => {
  await page.addInitScript(() => {
    const native = window as unknown as {
      __RELAY_SIDEBAR_ATTENTION__: { collapsed: boolean };
      __TAURI_INTERNALS__: { invoke: (command: string, args?: { collapsed?: boolean }) => Promise<unknown> };
      attentionWrites: boolean[]; finishAttentionRead: () => void; finishAttentionWrite: (() => void)[];
    };
    native.__RELAY_SIDEBAR_ATTENTION__ = { collapsed: true };
    native.attentionWrites = []; native.finishAttentionWrite = [];
    native.__TAURI_INTERNALS__ = { invoke: async (command, args) => {
      if (command === "appearance") return { preference: "system", resolved: "light" };
      if (command !== "sidebar_attention") throw new Error("Unexpected fixture command");
      if (args?.collapsed === undefined) return new Promise((resolve) => { native.finishAttentionRead = () => resolve({ collapsed: false }); });
      native.attentionWrites.push(args.collapsed);
      return new Promise((resolve) => { native.finishAttentionWrite.push(() => resolve({ collapsed: args.collapsed })); });
    } };
  });
  await mockFleet(page);
  await open(page);
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "false");
  await toggle(page).click();
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "true");
  await toggle(page).click();
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "false");
  expect(await page.evaluate(() => (window as unknown as { attentionWrites: boolean[] }).attentionWrites)).toEqual([false]);
  await page.evaluate(() => (window as unknown as { finishAttentionRead: () => void }).finishAttentionRead());
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "false");
  await expect(attention(page).getByRole("status")).toContainText("taking longer", { timeout: 6500 });
  await page.evaluate(() => (window as unknown as { finishAttentionWrite: (() => void)[] }).finishAttentionWrite[0]());
  await expect.poll(() => page.evaluate(() => (window as unknown as { attentionWrites: boolean[] }).attentionWrites)).toEqual([false, true]);
  await page.evaluate(() => (window as unknown as { finishAttentionWrite: (() => void)[] }).finishAttentionWrite[1]());
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "false");
  await expect(attention(page).getByRole("status")).toHaveCount(0);
});

test("a rejected native preference save stays visible and a later deliberate toggle can recover", async ({ page }) => {
  await page.addInitScript(() => {
    const native = window as unknown as {
      __RELAY_SIDEBAR_ATTENTION__: { collapsed: boolean };
      __TAURI_INTERNALS__: { invoke: (command: string, args?: { collapsed?: boolean }) => Promise<unknown> };
      attentionWrites: boolean[]; rejectAttentionSave: boolean;
    };
    native.__RELAY_SIDEBAR_ATTENTION__ = { collapsed: false };
    native.attentionWrites = []; native.rejectAttentionSave = true;
    native.__TAURI_INTERNALS__ = { invoke: async (command, args) => {
      if (command === "appearance") return { preference: "system", resolved: "light" };
      if (command !== "sidebar_attention") throw new Error("Unexpected fixture command");
      if (args?.collapsed === undefined) return { collapsed: false };
      native.attentionWrites.push(args.collapsed);
      if (native.rejectAttentionSave) throw new Error("Fixture write failed");
      return { collapsed: args.collapsed };
    } };
  });
  const fixture = await mockFleet(page);
  await open(page);
  await toggle(page).click();
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "false");
  await expect(attention(page).getByRole("status")).toContainText("Could not save");
  const polls = fixture.stateRequests();
  await expect.poll(() => fixture.stateRequests()).toBeGreaterThan(polls);
  expect(await page.evaluate(() => (window as unknown as { attentionWrites: boolean[] }).attentionWrites)).toEqual([true]);
  await page.evaluate(() => { (window as unknown as { rejectAttentionSave: boolean }).rejectAttentionSave = false; });
  await toggle(page).click();
  await expect(toggle(page)).toHaveAttribute("aria-expanded", "true");
  await expect(attention(page).getByRole("status")).toHaveCount(0);
  expect(await page.evaluate(() => (window as unknown as { attentionWrites: boolean[] }).attentionWrites)).toEqual([true, false]);
  expect(fixture.mutations).toEqual([]);
});
