import { expect, test, type Page } from "@playwright/test";
import { mockFleet, remote, work } from "./fixture";

const session = { ...work, id: "a".repeat(32) };
const queued = (id = "b".repeat(32)) => ({ id, sessionId: session.id, sessionCreatedAt: session.createdAt, createdAt: new Date().toISOString(), expiresAt: new Date(Date.now() + 120000).toISOString(), status: "pending" });
async function setup(page: Page, options: { native?: boolean; primary?: boolean; supported?: boolean; rejectOpen?: boolean; rejectClaim?: boolean; loseClaim?: boolean; staleAfterClaim?: boolean; count?: number } = {}) {
  const fixture = await mockFleet(page, { hosts: [structuredClone(remote)], sessions: { dev: [structuredClone(session)] } });
  const requests = Array.from({ length: options.count ?? 1 }, (_, i) => queued((i + 11).toString(16).repeat(32)));
  const events: { type: string; value?: unknown }[] = [];
  let reads = 0, claims = 0, browserOpens = 0;
  if (options.native !== false) {
    await page.exposeFunction("requestNative", async (name: string, args: unknown) => {
      if (name === "appearance") return { preference: "system", resolved: "dark" };
      if (name === "sidebar_attention") return { collapsed: false };
      if (name === "terminal_groups") return { revision: 0, groups: [] };
      if (name !== "open_session_window") throw Error("Unexpected native command");
      events.push({ type: "open", value: args });
      if (options.rejectOpen) throw Error("Native acknowledgment lost");
    });
    await page.addInitScript(({ primary }) => {
      Object.defineProperty(window, "__RELAY_PRIMARY_WINDOW__", { value: primary, writable: false, configurable: false });
      Object.assign(window, { __TAURI_INTERNALS__: { invoke: (window as unknown as { requestNative: unknown }).requestNative } });
    }, { primary: options.primary !== false });
  }
  await page.exposeFunction("requestBrowserOpen", () => { browserOpens++; });
  await page.addInitScript(() => { window.open = () => { void (window as unknown as { requestBrowserOpen: () => Promise<void> }).requestBrowserOpen(); return null; }; });
  await page.route("**/api/hosts/dev/runtime/health", (route) => route.fulfill({ json: { protocol: 1, capabilities: { viewRequestsV1: options.supported !== false } } }));
  await page.route("**/api/hosts/dev/runtime/views**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    const request = requests.find((item) => path.includes(item.id));
    if (path.endsWith("/claim")) {
      claims++; events.push({ type: "claim", value: route.request().postDataJSON() });
      expect(route.request().headers()["x-relay-csrf"]).toBe("fixture-csrf");
      if (options.rejectClaim) return route.fulfill({ status: 409, json: { error: "Already claimed" } });
      if (options.loseClaim) return route.abort("connectionreset");
      request!.status = "claimed";
      if (options.staleAfterClaim) fixture.sessions.dev[0].createdAt = "2026-11-04T20:00:00Z";
      return route.fulfill({ json: request });
    }
    if (path.endsWith("/complete")) {
      const body = route.request().postDataJSON(); events.push({ type: "complete", value: body }); request!.status = body.status;
      return route.fulfill({ json: request });
    }
    reads++; return route.fulfill({ json: { requests } });
  });
  return { fixture, requests, events, reads: () => reads, claims: () => claims, browserOpens: () => browserOpens };
}
async function ready(page: Page) {
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Fleet overview", exact: true })).toBeVisible();
  await expect(page.locator(".session-row")).toHaveCount(1);
  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
}
async function poll(page: Page) { await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange"))); }

test("primary desktop claims an exact remote session before opening once without terminal input", async ({ page }) => {
  const state = await setup(page); await ready(page);
  await expect.poll(() => state.events.map((item) => item.type)).toEqual(["claim", "open", "complete"]);
  expect(state.events[0].value).toEqual({ clientId: expect.stringMatching(/^[a-f0-9]{32}$/) });
  expect(state.events[1].value).toEqual({ hostId: "dev", sessionId: session.id });
  expect(state.events[2].value).toEqual({ ...state.events[0].value as object, status: "opened" });
  await poll(page); await expect.poll(state.reads).toBeGreaterThanOrEqual(2);
  expect(state.claims()).toBe(1); expect(state.browserOpens()).toBe(0);
  expect(state.fixture.mutations).toEqual([]); expect(state.fixture.terminalMessages).toEqual([]);
});

for (const role of ["detached", "ordinary embedded fixture"] as const) test(`${role} never consumes native window requests even on the fleet route`, async ({ page }) => {
  const state = await setup(page, role === "detached" ? { primary: false } : { native: false }); await ready(page);
  await page.clock.install(); await page.clock.fastForward(10000); await poll(page);
  expect(state.reads()).toBe(0); expect(state.claims()).toBe(0); expect(state.events).toEqual([]); expect(state.browserOpens()).toBe(0);
});

test("old runtime capability never lists or claims requests", async ({ page }) => {
  const state = await setup(page, { supported: false }); await ready(page);
  await page.clock.install(); await page.clock.fastForward(10000); await poll(page);
  expect(state.reads()).toBe(0); expect(state.events).toEqual([]);
});

for (const reason of ["reused identity", "setup session"] as const) test(`${reason} never authorizes a window`, async ({ page }) => {
  const state = await setup(page);
  if (reason === "reused identity") state.requests[0].sessionCreatedAt = "2026-11-04T20:00:00Z";
  else state.fixture.sessions.dev[0].purpose = "login";
  await ready(page); await expect.poll(state.reads).toBeGreaterThan(0);
  await poll(page); await expect.poll(state.reads).toBeGreaterThan(1);
  expect(state.claims()).toBe(0); expect(state.events).toEqual([]);
});

for (const outcome of ["rejected", "unknown"] as const) test(`${outcome} claim is never replayed or followed by a native open`, async ({ page }) => {
  const state = await setup(page, outcome === "rejected" ? { rejectClaim: true } : { loseClaim: true }); await ready(page);
  await expect.poll(state.claims).toBe(1); await poll(page); await expect.poll(state.reads).toBeGreaterThan(1);
  expect(state.claims()).toBe(1); expect(state.events.map((item) => item.type)).toEqual(["claim"]); expect(state.browserOpens()).toBe(0);
});

test("unknown native open remains claimed and never falls back or retries", async ({ page }) => {
  const state = await setup(page, { rejectOpen: true }); await ready(page);
  await expect(page.getByRole("status").filter({ hasText: "opening it was not confirmed" })).toBeVisible();
  await poll(page); await expect.poll(state.reads).toBeGreaterThan(1);
  expect(state.events.map((item) => item.type)).toEqual(["claim", "open"]); expect(state.requests[0].status).toBe("claimed"); expect(state.browserOpens()).toBe(0);
});

test("fresh identity read after claim blocks an unrelated replacement session", async ({ page }) => {
  const state = await setup(page, { staleAfterClaim: true }); await ready(page);
  await expect.poll(() => state.events.map((item) => item.type)).toEqual(["claim", "complete"]);
  expect(state.requests[0].status).toBe("failed"); expect(state.browserOpens()).toBe(0);
});

test("native windows are limited to two per thirty seconds without losing later pending requests", async ({ page }) => {
  const state = await setup(page, { count: 3 }); await ready(page);
  await expect.poll(() => state.events.filter((item) => item.type === "open").length).toBe(2);
  await poll(page); await expect.poll(state.reads).toBeGreaterThan(1); expect(state.claims()).toBe(2);
  await page.clock.install(); await page.clock.fastForward(31000);
  await expect.poll(() => state.events.filter((item) => item.type === "open").length).toBe(3);
  expect(state.claims()).toBe(3); expect(state.browserOpens()).toBe(0);
});
