import { expect, test, type Page } from "@playwright/test";
import { mockFleet, openNavigation, remote, work } from "./fixture";
import type { TerminalGroup } from "../src/terminalGroups";
const second = { ...remote, id: "production", name: "Production", target: "ops@fixture.example.test" };
const alpha = { ...work, title: "Organization Switcher", workspace: "IQ" };
const beta = { ...work, id: "beta", title: "ThrowTerminal", workspace: "IQ" };
const gamma = { ...work, id: "gamma", title: "Other task", workspace: "Other" };
const delta = { ...work, id: "delta", title: "Build log", workspace: "Other" };
const key = "relay.terminal-groups.v1";
const pane = (page: Page, host: string, session: string) => page.locator(`.terminal-pane[data-pane-host="${host}"][data-pane-session="${session}"]`);
async function setup(page: Page) { return mockFleet(page, { hosts: [structuredClone(remote), structuredClone(second)], sessions: { dev: [structuredClone(alpha), structuredClone(gamma)], production: [structuredClone(beta), structuredClone(delta)] } }); }
async function split(page: Page, title = beta.title) {
  await page.getByRole("button", { name: "Add terminal pane", exact: true }).first().click();
  await page.getByRole("dialog", { name: "Add terminal pane" }).getByRole("button", { name: `${title} on ${second.name}`, exact: true }).click();
  await expect(page.locator(".terminal-pane")).toHaveCount(2);
}
async function saved(page: Page): Promise<{ revision: number; groups: TerminalGroup[] }> { return page.evaluate((key) => JSON.parse(localStorage.getItem(key)!), key); }
async function groupButton(page: Page, title = alpha.title) { await openNavigation(page); return page.getByRole("button", { name: `Open terminal group ${title}`, exact: true }); }

test("saved subgroup nests cross-host members without duplicate rows and member opens alone", async ({ page }) => {
  const fixture = await setup(page); await page.goto("/#host/dev/session/session-1"); await split(page);
  await expect.poll(async () => (await saved(page))?.groups.length).toBe(1);
  const snapshot = await saved(page);
  expect(snapshot.groups[0].panes).toEqual([{ host: "dev", session: alpha.id, createdAt: alpha.createdAt }, { host: "production", session: beta.id, createdAt: beta.createdAt }]);
  await openNavigation(page);
  const group = page.locator(".terminal-tree-group"); await expect(group).toHaveCount(1);
  await expect(group.locator(".terminal-group-members .session-row")).toHaveCount(2);
  await expect(page.locator(".host-tree .session-row").filter({ hasText: beta.title })).toHaveCount(1);
  await expect(group.locator("xpath=..").locator("xpath=..").locator(".workspace-row")).toContainText("IQ");
  await group.locator(".session-row").filter({ hasText: beta.title }).click();
  await expect(page.locator(".terminal-pane")).toHaveCount(1); await expect(pane(page, "production", beta.id).locator(".xterm")).toBeVisible();
  expect(await saved(page)).toEqual(snapshot);
  await (await groupButton(page)).click(); await expect(page.locator(".terminal-pane")).toHaveCount(2);
  expect(fixture.mutations).toEqual([]); expect(fixture.terminalMessages.filter((m) => m.type === "input")).toEqual([]);
});

test("multiple groups survive fleet navigation, unrelated single sessions and reload", async ({ page }) => {
  const fixture = await setup(page); await page.goto("/#host/dev/session/session-1"); await split(page);
  await openNavigation(page); await page.locator(".host-tree .session-row").filter({ hasText: gamma.title }).click(); await expect(page.locator(".terminal-pane")).toHaveCount(1);
  await split(page, delta.title); await expect.poll(async () => (await saved(page)).groups.length).toBe(2);
  const before = await saved(page);
  await openNavigation(page); await page.getByRole("button", { name: "Fleet overview", exact: false }).click();
  await page.reload(); await expect(page.getByRole("heading", { name: "Fleet overview", exact: true })).toBeVisible();
  await (await groupButton(page)).click(); await expect(pane(page, "dev", alpha.id).locator(".xterm")).toBeVisible(); await expect(pane(page, "production", beta.id).locator(".xterm")).toBeVisible();
  await (await groupButton(page, gamma.title)).click(); await expect(pane(page, "dev", gamma.id).locator(".xterm")).toBeVisible(); await expect(pane(page, "production", delta.id).locator(".xterm")).toBeVisible();
  expect(await saved(page)).toEqual(before); expect(fixture.mutations).toEqual([]);
});

test("closing a pane updates only the saved group and forgetting never removes sessions", async ({ page }) => {
  const fixture = await setup(page); await page.goto("/#host/dev/session/session-1"); await split(page);
  await pane(page, "production", beta.id).getByRole("button", { name: "Close pane", exact: true }).click();
  await expect.poll(async () => (await saved(page)).groups[0].panes.length).toBe(1);
  await page.reload(); await expect(page.locator(".terminal-pane")).toHaveCount(1);
  await (await groupButton(page)).click(); await expect(page.locator(".terminal-pane")).toHaveCount(1);
  await openNavigation(page); await page.getByRole("button", { name: `Forget terminal group ${alpha.title}`, exact: true }).click();
  await expect(page.locator(".terminal-tree-group")).toHaveCount(0); expect((await saved(page)).groups).toEqual([]);
  expect(fixture.sessions.production).toHaveLength(2); expect(fixture.sessions.dev).toHaveLength(2); expect(fixture.mutations).toEqual([]);
});

test("collapse is keyboard accessible and persists while search temporarily reveals a matching child", async ({ page }) => {
  await setup(page); await page.goto("/#host/dev/session/session-1"); await split(page); await openNavigation(page);
  await page.getByRole("button", { name: `Collapse terminal group ${alpha.title}`, exact: true }).focus(); await page.keyboard.press("Enter");
  await expect(page.locator(".terminal-group-members")).toBeHidden(); await page.reload(); await openNavigation(page);
  await expect(page.getByRole("button", { name: `Expand terminal group ${alpha.title}`, exact: true })).toHaveAttribute("aria-expanded", "false");
  await page.getByRole("textbox", { name: "Filter machines and sessions" }).fill(beta.title);
  await expect(page.locator(".terminal-group-members .session-row").filter({ hasText: beta.title })).toBeVisible(); expect((await saved(page)).groups[0].collapsed).toBe(true);
  await page.getByRole("button", { name: "Clear filter" }).click(); await expect(page.locator(".terminal-group-members")).toBeHidden();
});

test("renamed anchor follows its identity but a reused member ID remains unavailable", async ({ page }) => {
  const fixture = await setup(page); await page.goto("/#host/dev/session/session-1"); await split(page); fixture.sessions.dev[0].title = "Renamed anchor";
  await openNavigation(page); await expect(page.getByRole("button", { name: "Open terminal group Renamed anchor", exact: true })).toBeVisible();
  fixture.sessions.production[0] = { ...beta, title: "Unrelated replacement", createdAt: "2026-11-04T20:00:00Z" };
  await expect(page.locator(".host-tree .session-row").filter({ hasText: "Unrelated replacement" })).toHaveCount(1);
  await page.getByRole("button", { name: "Open terminal group Renamed anchor", exact: true }).click();
  await expect(pane(page, "production", beta.id)).toContainText("The session identity has changed."); await expect(pane(page, "production", beta.id).locator(".xterm")).toHaveCount(0);
  expect((await saved(page)).groups[0].panes[1].createdAt).toBe(beta.createdAt); expect(fixture.terminalMessages.filter((m) => m.type === "input")).toEqual([]);
});

test("missing anchor host retains a reachable saved group and never attaches its pane", async ({ page }) => {
  const fixture = await setup(page); await page.goto("/#host/dev/session/session-1"); await split(page);
  fixture.hosts.splice(fixture.hosts.findIndex((h) => h.id === "dev"), 1); await openNavigation(page);
  const orphan = page.getByRole("region", { name: "Saved terminal groups", exact: true }); await expect(orphan).toBeVisible();
  await orphan.getByRole("button", { name: `Open terminal group ${alpha.title}`, exact: true }).click();
  await expect(pane(page, "dev", alpha.id)).toContainText("This machine is unavailable."); await expect(pane(page, "production", beta.id).locator(".xterm")).toBeVisible();
  await expect(pane(page, "dev", alpha.id).locator(".xterm")).toHaveCount(0); expect(fixture.mutations).toEqual([]);
});

test("native storage is authoritative with blocked localStorage and survives document reload", async ({ page }) => {
  const fixture = await setup(page); let store: { revision: number; groups: TerminalGroup[] } = { revision: 0, groups: [] }; const writes: unknown[] = [];
  await page.exposeFunction("groupNative", async (name: string, args?: { change?: { revision: number; group?: TerminalGroup; remove?: string } }) => {
    if (name === "sidebar_attention") return { collapsed: false }; if (name === "appearance") return { preference: "system", resolved: "dark" };
    if (name !== "terminal_groups") throw Error("Unexpected command");
    if (args?.change) { const c = args.change; writes.push(c); if (c.revision !== store.revision) throw Error("Conflict"); store = { revision: store.revision + 1, groups: c.group ? [...store.groups.filter((g) => g.id !== c.group!.id), c.group] : store.groups.filter((g) => g.id !== c.remove) }; } return structuredClone(store);
  });
  await page.addInitScript(() => { Object.defineProperty(window, "localStorage", { get: () => { throw Error("Disabled"); } }); Object.assign(window, { __TAURI_INTERNALS__: { invoke: (window as unknown as { groupNative: unknown }).groupNative } }); });
  await page.goto("/#host/dev/session/session-1"); await split(page); await expect.poll(() => store.groups.length).toBe(1);
  await page.goto("/"); await page.reload(); await (await groupButton(page)).click(); await expect(page.locator(".terminal-pane")).toHaveCount(2);
  expect(writes).toHaveLength(1); expect(fixture.mutations).toEqual([]);
});

test("rejected native save keeps the view open without retrying or overwriting another window", async ({ page }) => {
  await setup(page); let writes = 0;
  await page.exposeFunction("groupNative", async (name: string, args?: { change?: unknown }) => {
    if (name === "sidebar_attention") return { collapsed: false }; if (name === "appearance") return { preference: "system", resolved: "dark" }; if (name !== "terminal_groups") throw Error("Unexpected command");
    if (args?.change) { writes++; throw Error("Terminal groups changed in another window. Reload saved groups before editing."); } return { revision: 4, groups: [] };
  });
  await page.addInitScript(() => Object.assign(window, { __TAURI_INTERNALS__: { invoke: (window as unknown as { groupNative: unknown }).groupNative } }));
  await page.goto("/#host/dev/session/session-1"); await split(page);
  await expect(page.getByRole("status").filter({ hasText: "saving was not confirmed" })).toBeVisible(); expect(writes).toBe(1);
  await page.getByRole("button", { name: "Reload saved groups", exact: true }).click(); await expect(page.locator(".terminal-tree-group")).toHaveCount(0); await expect(page.locator(".terminal-pane")).toHaveCount(2); expect(writes).toBe(1);
});


test("forgetting the open split does not resurrect it when one viewer is closed", async ({ page }) => {
  const fixture = await setup(page); await page.goto("/#host/dev/session/session-1"); await split(page);
  await openNavigation(page); await page.getByRole("button", { name: `Forget terminal group ${alpha.title}`, exact: true }).click();
  expect((await saved(page)).groups).toEqual([]);
  const closeNavigation = page.getByRole("button", { name: "Close navigation", exact: true });
  if (await closeNavigation.last().isVisible()) await closeNavigation.last().click();
  await pane(page, "production", beta.id).getByRole("button", { name: "Close pane", exact: true }).click();
  await expect(page.locator(".terminal-pane")).toHaveCount(1); expect((await saved(page)).groups).toEqual([]); expect(fixture.mutations).toEqual([]);
});

test("saved subgroup stays within the themed sidebar at desktop and phone widths", async ({ page }, info) => {
  await setup(page); await page.goto("/#host/dev/session/session-1"); await split(page); await openNavigation(page);
  for (const theme of ["light", "dark"] as const) {
    const closeNavigation = page.getByRole("button", { name: "Close navigation", exact: true });
    if (await closeNavigation.last().isVisible()) await closeNavigation.last().click();
    await page.getByRole("combobox", { name: "Appearance", exact: true }).selectOption(theme);
    await expect(page.locator("html")).toHaveAttribute("data-theme", theme);
    await openNavigation(page);
    const group = page.locator(".terminal-tree-group");
    expect(await group.evaluate((node) => { const b = node.getBoundingClientRect(), owner = node.closest(".sidebar")!.getBoundingClientRect(); return b.left >= owner.left && b.right <= owner.right && [...node.querySelectorAll("button")].every((button) => { const r = button.getBoundingClientRect(); return r.left >= b.left && r.right <= b.right + 1; }); })).toBe(true);
    await page.screenshot({ path: info.outputPath(`terminal-group-${theme}.png`) });
  }
});
