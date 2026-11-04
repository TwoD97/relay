import { expect, test, type Page } from "@playwright/test";
import { mockFleet, remote, work } from "./fixture";
import type { TerminalGroup } from "../src/terminalGroups";

const source = { sourceId: "a".repeat(48), name: "Other desktop", revision: 9, digest: "b".repeat(64), updatedAt: "2026-11-04T12:00:00Z", pages: 2, tasks: 3, lessons: 1, groups: 1, hosts: ["Development"] };
const group: TerminalGroup = { id: "00000000-0000-4000-8000-000000000001", title: "Shared layout", hostName: remote.name, workspace: work.workspace, collapsed: false, panes: [{ host: remote.id, session: work.id, createdAt: work.createdAt }] };
async function setup(page: Page, options: { alternatives?: boolean; conflict?: string; failed?: number; unknown?: boolean; deferred?: boolean; warnings?: string[] } = {}) {
  const fixture = await mockFleet(page);
  const imports: Record<string, unknown>[] = [], publications: unknown[] = [];
  let revision = 4, reads = 0;
  let finish: (() => void) | undefined;
  const gate = new Promise<void>((resolve) => { finish = resolve; });
  await page.route("**/api/peer-sync**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith("/groups")) { publications.push(route.request().postDataJSON()); return route.fulfill({ json: { ok: true } }); }
    if (path.endsWith("/import")) {
      const body = route.request().postDataJSON(); imports.push(body);
      expect(route.request().headers()["x-relay-csrf"]).toBe("fixture-csrf");
      if (options.unknown) return route.abort("connectionreset");
      if (options.failed) return route.fulfill({ status: options.failed, json: { error: "The shared snapshot or workspace changed. Refresh before importing." } });
      if (options.deferred) await gate;
      revision++;
      return route.fulfill({ json: { workspace: { revision, updatedAt: new Date().toISOString(), pages: [], tasks: [] }, groups: [group], warnings: options.warnings ?? [] } });
    }
    reads++; return route.fulfill({ json: { workspaceRevision: revision, sources: [{ ...source, conflict: options.conflict }, ...(options.alternatives ? [{ ...source, digest: "c".repeat(64), conflict: options.conflict }] : [])], warnings: [] } });
  });
  return { fixture, imports, publications, reads: () => reads, finish: () => finish!() };
}
const shared = (page: Page) => page.getByRole("region", { name: "Shared workspaces", exact: true });
async function open(page: Page) { await page.goto("/#host/dev"); await expect(shared(page).getByRole("heading", { name: source.name, exact: true })).toBeVisible(); }
async function confirm(page: Page) {
  await shared(page).getByRole("button", { name: "Import a copy", exact: true }).click();
  await page.getByRole("dialog", { name: `Import ${source.name}?`, exact: true }).getByRole("button", { name: "Import a separate copy", exact: true }).click();
}

test("shared snapshots are discovered without importing or touching sessions", async ({ page }) => {
  const state = await setup(page); await open(page);
  await expect(shared(page)).toContainText("2 pages · 3 tasks · 1 lessons · 1 terminal groups");
  await expect(shared(page)).toContainText("Imports do not transfer sign-in credentials");
  await shared(page).getByRole("button", { name: "Refresh shared workspaces", exact: true }).click();
  await expect.poll(state.reads).toBe(2);
  expect(state.imports).toEqual([]); expect(state.fixture.mutations).toEqual([]); expect(state.fixture.terminalMessages).toEqual([]);
});

test("explicit copy uses exact digest and workspace revision then saves a separate terminal group", async ({ page }) => {
  const state = await setup(page, { warnings: ["An unavailable machine reference was preserved for review."] }); await open(page);
  await shared(page).getByRole("button", { name: "Import a copy", exact: true }).click(); expect(state.imports).toEqual([]);
  await page.getByRole("dialog").getByRole("button", { name: "Import a separate copy", exact: true }).click();
  await expect(shared(page).getByRole("status").filter({ hasText: "A separate copy was imported" })).toBeVisible();
  expect(state.imports).toEqual([{ sourceId: source.sourceId, digest: source.digest, revision: 4, id: expect.stringMatching(/^[a-f0-9]{32}$/) }]);
  await expect.poll(() => page.evaluate(() => JSON.parse(localStorage.getItem("relay.terminal-groups.v1")!).groups)).toEqual([group]);
  await expect(shared(page)).toContainText("An unavailable machine reference was preserved for review.");
  expect(state.fixture.mutations).toEqual([]); expect(state.fixture.terminalMessages).toEqual([]);
});

test("conflicting snapshots require explicit selection of the exact separate copy", async ({ page }) => {
  const state = await setup(page, { alternatives: true, conflict: "Two connected machines disagree about this source revision." }); await page.goto("/#host/dev");
  await expect(shared(page)).toContainText("Two connected machines disagree");
  expect(state.imports).toEqual([]);
  await shared(page).locator("article").filter({ hasText: "cccccccccccc" }).getByRole("button", { name: "Import a copy", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("Two connected machines disagree");
  await page.getByRole("dialog").getByRole("button", { name: "Import a separate copy", exact: true }).click();
  await expect.poll(() => state.imports.length).toBe(1); expect(state.imports[0].digest).toBe("c".repeat(64));
});

test("a rejected CAS copy requires a fresh read and never overwrites saved layout", async ({ page }) => {
  const state = await setup(page, { failed: 409 }); await open(page); await confirm(page);
  await expect(shared(page)).toContainText("The shared snapshot or workspace changed");
  await expect(shared(page).getByRole("button", { name: "Import a copy", exact: true })).toBeDisabled();
  await shared(page).getByRole("button", { name: "Refresh shared workspaces", exact: true }).click();
  await expect(shared(page).getByRole("button", { name: "Import a copy", exact: true })).toBeEnabled();
  expect(state.imports).toHaveLength(1); expect(await page.evaluate(() => localStorage.getItem("relay.terminal-groups.v1"))).toBeNull();
});

test("unknown import survives reload and is never retried automatically", async ({ page }) => {
  const state = await setup(page, { unknown: true }); await open(page); await confirm(page);
  await expect(shared(page)).toContainText("The last import has not been confirmed.");
  const id = state.imports[0].id as string;
  await page.reload(); await expect(shared(page)).toContainText(id);
  await expect(shared(page).getByRole("button", { name: "Import a copy", exact: true })).toBeDisabled();
  await expect(shared(page).getByRole("button", { name: "Review and resume importing", exact: true })).toBeEnabled();
  expect(state.imports).toHaveLength(1);
  await shared(page).getByRole("button", { name: "Review and resume importing", exact: true }).click();
  await expect(shared(page)).toContainText("the earlier request may still finish"); expect(state.imports).toHaveLength(1);
});

test("an accepted late copy saves its layout without stealing navigation", async ({ page }) => {
  const state = await setup(page, { deferred: true }); await open(page); await confirm(page);
  await expect.poll(() => state.imports.length).toBe(1);
  await page.evaluate(() => { location.hash = "mastermind/tasks"; });
  await expect(page.getByRole("heading", { name: "Mastermind", exact: true, level: 1 })).toBeVisible();
  state.finish();
  await expect.poll(() => page.evaluate(() => JSON.parse(localStorage.getItem("relay.terminal-groups.v1")!).groups)).toEqual([group]);
  await expect(page).toHaveURL(/#mastermind\/tasks$/); expect(state.imports).toHaveLength(1);
});

test("only confirmed primary native groups publish and a lost acknowledgment is not retried", async ({ page }) => {
  const state = await setup(page);
  let finish: (() => void) | undefined;
  const gate = new Promise<void>((resolve) => { finish = resolve; });
  await page.exposeFunction("peerNative", async (name: string) => {
    if (name === "appearance") return { preference: "system", resolved: "dark" };
    if (name === "sidebar_attention") return { collapsed: false };
    if (name === "terminal_groups") { await gate; return { revision: 7, groups: [group] }; }
    throw Error("Unexpected command");
  });
  await page.addInitScript(() => Object.assign(window, { __RELAY_PRIMARY_WINDOW__: true, __TAURI_INTERNALS__: { invoke: (window as unknown as { peerNative: unknown }).peerNative } }));
  const writes: unknown[] = [];
  await page.route("**/api/peer-sync/groups", async (route) => { writes.push(route.request().postDataJSON()); return route.abort("connectionreset"); });
  await open(page); expect(writes).toEqual([]);
  finish!(); await expect.poll(() => writes.length).toBe(1);
  expect(writes).toEqual([{ revision: 7, groups: [group] }]);
  await expect(page.getByRole("status").filter({ hasText: "sharing their latest layout was not confirmed" })).toBeVisible();
  await page.evaluate(() => window.dispatchEvent(new Event("focus"))); await shared(page).getByRole("button", { name: "Refresh shared workspaces", exact: true }).click();
  await expect.poll(state.reads).toBeGreaterThan(1); expect(writes).toHaveLength(1);
});
