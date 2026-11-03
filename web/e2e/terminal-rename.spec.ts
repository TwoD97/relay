import { expect, test, type Page, type Request } from "@playwright/test";
import { mockFleet, openNavigation, work } from "./fixture";
import type { Session } from "../src/types";

type Fleet = Awaited<ReturnType<typeof mockFleet>>;
const endpoint = "**/api/hosts/dev/runtime/sessions/session-1";
function input(fixture: Fleet) { return fixture.terminalMessages.filter((message) => message.type === "input").map((message) => message.data).join(""); }
async function open(page: Page) {
  const fixture = await mockFleet(page, { sessions: { local: [], dev: [structuredClone(work), { ...work, id: "second", title: "Another terminal" }] }, terminalOutput: "RENAME_PRESERVED\r\n$ " });
  await page.goto("/#host/dev/session/session-1");
  await page.locator(".terminal-canvas").click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  return fixture;
}
async function successfulRename(page: Page, fixture: Fleet) {
  const requests: Request[] = [];
  await page.route(endpoint, async (route) => {
    if (route.request().method() !== "PATCH") { await route.fallback(); return; }
    requests.push(route.request());
    const target = fixture.sessions.dev.find((session) => session.id === work.id)!;
    target.title = route.request().postDataJSON().title;
    target.updatedAt = new Date().toISOString();
    await route.fulfill({ json: target });
  });
  return requests;
}

test("renaming a terminal updates its header, sidebar and fleet while preserving the PTY and writer lease", async ({ page }) => {
  const fixture = await open(page);
  const requests = await successfulRename(page, fixture);
  await page.keyboard.type("draft before rename");
  const original = await page.locator(".xterm").elementHandle();
  await page.getByRole("button", { name: "Rename terminal", exact: true }).click();
  await page.getByRole("textbox", { name: "Terminal name", exact: true }).fill("  Release checklist  ");
  await page.getByRole("textbox", { name: "Terminal name", exact: true }).press("Enter");
  await expect(page.getByRole("heading", { name: "Release checklist", exact: true })).toBeVisible();
  expect(requests).toHaveLength(1);
  expect(requests[0].postDataJSON()).toEqual({ title: "Release checklist" });
  expect(requests[0].headers()["x-relay-csrf"]).toBe("fixture-csrf");
  expect(await original!.evaluate((element) => element.isConnected && element === document.querySelector(".xterm"))).toBe(true);
  expect(fixture.attachments).toHaveLength(1);
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await expect(page.locator(".xterm-rows")).toContainText("RENAME_PRESERVED");
  await page.locator(".terminal-canvas").click();
  await page.keyboard.type(" and after");
  await expect.poll(() => input(fixture)).toBe("draft before rename and after");
  expect(fixture.terminalMessages.filter((message) => message.type === "claim")).toHaveLength(1);
  await openNavigation(page);
  await expect(page.getByRole("navigation", { name: "Machines and workspaces" }).getByRole("button", { name: /Release checklist/ })).toBeVisible();
  await page.getByRole("button", { name: /Fleet overview/ }).click();
  await expect(page.getByRole("article", { name: "Development: Release checklist", exact: true })).toBeVisible();
  expect(requests).toHaveLength(1);
});

test("clicking the title opens rename and Escape or Cancel leaves the name untouched", async ({ page }) => {
  const fixture = await open(page);
  const requests = await successfulRename(page, fixture);
  await page.getByRole("heading", { name: work.title, exact: true }).click();
  const field = page.getByRole("textbox", { name: "Terminal name", exact: true });
  await expect(field).toBeFocused();
  await field.fill("discarded draft");
  await field.press("Escape");
  await expect(page.getByRole("heading", { name: work.title, exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Rename terminal", exact: true }).click();
  await expect(field).toHaveValue(work.title);
  await field.fill("another discarded draft");
  await page.getByRole("button", { name: "Cancel rename", exact: true }).click();
  expect(requests).toEqual([]);
  expect(input(fixture)).toBe("");
});

test("rename validates blank names, control characters and the 120-byte UTF-8 limit before writing", async ({ page }) => {
  const fixture = await open(page);
  const requests = await successfulRename(page, fixture);
  await page.getByRole("button", { name: "Rename terminal", exact: true }).click();
  const field = page.getByRole("textbox", { name: "Terminal name", exact: true });
  const save = page.getByRole("button", { name: "Save terminal name", exact: true });
  for (const name of ["   ", "bad\u007fname", "bad\u0085name", "é".repeat(61)]) {
    await field.fill(name);
    await expect(save).toBeDisabled();
    await field.press("Enter");
  }
  expect(requests).toEqual([]);
  const valid = "é".repeat(60);
  await field.fill(valid);
  await expect(save).toBeEnabled();
  await save.click();
  await expect(page.getByRole("heading", { name: valid, exact: true })).toBeVisible();
  expect(requests).toHaveLength(1);
  expect(Buffer.byteLength(requests[0].postDataJSON().title, "utf8")).toBe(120);
});

for (const result of ["lost response", "mismatched response"] as const) {
  test(`a rename with ${result} never repeats and checks the exact current session before editing again`, async ({ page }) => {
    const fixture = await open(page);
    const requests: Request[] = [];
    await page.route(endpoint, async (route) => {
      requests.push(route.request());
      fixture.sessions.dev[0].title = "Saved by the runtime";
      if (result === "lost response") await route.abort("failed");
      else await route.fulfill({ json: { ...fixture.sessions.dev[0], id: "different-session" } });
    });
    await page.getByRole("button", { name: "Rename terminal", exact: true }).click();
    await page.getByRole("textbox", { name: "Terminal name", exact: true }).fill("Saved by the runtime");
    await page.getByRole("button", { name: "Save terminal name", exact: true }).click();
    await expect(page.getByRole("alert")).toContainText("has not been repeated");
    await expect(page.getByRole("button", { name: "Save terminal name", exact: true })).toBeDisabled();
    await page.getByRole("button", { name: "Check current name", exact: true }).click();
    await expect(page.getByRole("textbox", { name: "Terminal name", exact: true })).toHaveValue("Saved by the runtime");
    await expect(page.getByRole("textbox", { name: "Terminal name", exact: true })).toBeEnabled();
    await expect(page.getByRole("alert")).toHaveCount(0);
    expect(requests).toHaveLength(1);
    await page.getByRole("button", { name: "Cancel rename", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Saved by the runtime", exact: true })).toBeVisible();
    expect(fixture.attachments).toHaveLength(1);
    expect(input(fixture)).toBe("");
  });
}

test("a rejected rename keeps its draft and permits a new deliberate correction", async ({ page }) => {
  const fixture = await open(page);
  let requests = 0;
  await page.route(endpoint, async (route) => {
    requests++;
    if (requests === 1) { await route.fulfill({ status: 400, json: { error: "This name was rejected" } }); return; }
    fixture.sessions.dev[0].title = route.request().postDataJSON().title;
    await route.fulfill({ json: fixture.sessions.dev[0] });
  });
  await page.getByRole("button", { name: "Rename terminal", exact: true }).click();
  const field = page.getByRole("textbox", { name: "Terminal name", exact: true });
  await field.fill("First draft");
  await field.press("Enter");
  await expect(page.getByRole("alert")).toContainText("This name was rejected");
  await expect(field).toHaveValue("First draft");
  expect(requests).toBe(1);
  await field.fill("Corrected title");
  await field.press("Enter");
  await expect(page.getByRole("heading", { name: "Corrected title", exact: true })).toBeVisible();
  expect(requests).toBe(2);
});

test("a delayed rename response cannot steal a newer session navigation", async ({ page }) => {
  const fixture = await open(page);
  let calls = 0;
  let release!: () => void;
  const pending = new Promise<void>((resolve) => { release = resolve; });
  await page.route(endpoint, async (route) => {
    calls++; await pending;
    fixture.sessions.dev[0].title = route.request().postDataJSON().title;
    await route.fulfill({ json: fixture.sessions.dev[0] });
  });
  await page.getByRole("button", { name: "Rename terminal", exact: true }).click();
  await page.getByRole("textbox", { name: "Terminal name", exact: true }).fill("Renamed in the background");
  await page.getByRole("button", { name: "Save terminal name", exact: true }).click();
  await expect.poll(() => calls).toBe(1);
  await page.evaluate(() => { location.hash = "#host/dev/session/second"; });
  await expect(page.getByRole("heading", { name: "Another terminal", exact: true })).toBeVisible();
  const response = page.waitForResponse((value) => value.url().endsWith("/runtime/sessions/session-1") && value.request().method() === "PATCH");
  release(); await (await response).finished();
  await expect(page).toHaveURL(/#host\/dev\/session\/second$/);
  await expect(page.getByRole("heading", { name: "Another terminal", exact: true })).toBeVisible();
  await expect(page.getByRole("textbox", { name: "Terminal name", exact: true })).toHaveCount(0);
  expect(calls).toBe(1);
});

test("mutable Mastermind title and workspace labels cannot detach its verified session", async ({ page }) => {
  const session: Session = { ...work, id: "coordinator", harness: "codex", mastermindId: "c".repeat(32), agentMode: "manual", title: "Mastermind" };
  const fixture = await mockFleet(page, { sessions: { local: [structuredClone(session)], dev: [] }, mastermind: { revision: 1, enabled: true, status: "running", stage: "Connected", config: { hostId: "local", harness: "codex", agentMode: "manual", cwd: session.cwd }, session: structuredClone(session), updatedAt: session.updatedAt } });
  await page.goto("/#mastermind");
  await page.locator(".terminal-canvas").click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  const original = await page.locator(".xterm").elementHandle();
  const previousReads = fixture.sessionRequests.local;
  fixture.sessions.local[0].title = "My coordinator";
  fixture.sessions.local[0].workspace = "Planning";
  await expect.poll(() => fixture.sessionRequests.local).toBeGreaterThan(previousReads);
  await expect(page.getByText("Session unverified", { exact: true })).toHaveCount(0);
  expect(await original!.evaluate((element) => element.isConnected && element === document.querySelector(".xterm"))).toBe(true);
  expect(fixture.attachments).toHaveLength(1);
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  expect(fixture.mutations).toEqual([]);
});
