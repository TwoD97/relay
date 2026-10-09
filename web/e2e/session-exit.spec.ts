import { expect, test } from "@playwright/test";
import type { Session } from "../src/types";
import { mockFleet, work } from "./fixture";

const saved: Session = {
  ...work,
  title: "Review the checkout",
  workspace: "Client delivery",
  cwd: "/srv/Client work/$draft & [review]",
  harness: "claude",
  status: "exited",
  exitCode: 0,
};

for (const [harness, state] of [["claude", "transition"], ["codex", "exited"], ["shell", "exited"], ["shell", "interrupted"]] as const) {
  test(`${harness} ${state} can open a shell in the original folder without removing saved output`, async ({ page }, testInfo) => {
    const original: Session = { ...saved, harness, status: state === "transition" ? "running" : state };
    if (state !== "exited") delete original.exitCode;
    const fixture = await mockFleet(page, {
      sessions: { dev: [original], local: [{ ...work, title: "Same ID on another machine" }] },
      terminalOutput: "ORIGINAL-SAVED-OUTPUT\r\n",
    });
    await page.goto("/#host/dev/session/session-1");
    await expect(page.getByRole("heading", { name: saved.title, exact: true })).toBeVisible();
    if (state === "transition") {
      await expect(page.getByRole("button", { name: "Open terminal here", exact: true })).toHaveCount(0);
      original.status = "exited"; original.exitCode = 0;
    }
    await expect(page.getByText("Saved output", { exact: true })).toBeVisible();
    await expect(page.getByRole("region", { name: `${saved.title} terminal`, exact: true })).toContainText("ORIGINAL-SAVED-OUTPUT");
    if (state === "transition") await page.screenshot({ path: testInfo.outputPath("finished-session-actions.png"), fullPage: true, animations: "disabled" });
    const before = structuredClone(original);
    await page.getByRole("button", { name: "Open terminal here", exact: true }).click();
    await expect(page.getByRole("heading", { name: `${saved.title} · Terminal`, exact: true })).toBeVisible();
    await expect(page).toHaveURL(/#host\/dev\/session\/created-session$/);
    expect(fixture.mutations).toEqual([{
      method: "POST", path: "/api/hosts/dev/runtime/sessions", csrf: "fixture-csrf",
      body: { title: `${saved.title} · Terminal`, workspace: saved.workspace, cwd: saved.cwd, harness: "shell" },
    }]);
    expect(fixture.sessions.dev.find((session) => session.id === saved.id)).toEqual(before);
    expect(fixture.sessions.local).toEqual([{ ...work, title: "Same ID on another machine" }]);
    await page.goto("/#host/dev/session/session-1");
    await expect(page.getByText("Saved output", { exact: true })).toBeVisible();
    await expect(page.getByRole("region", { name: `${saved.title} terminal`, exact: true })).toContainText("ORIGINAL-SAVED-OUTPUT");
  });
}

test("closing a finished session requires confirmation and removes only its saved record", async ({ page }) => {
  const sibling = { ...work, id: "sibling", title: "Keep this working session" };
  const fixture = await mockFleet(page, { sessions: { dev: [{ ...saved }, sibling], local: [{ ...work }] } });
  await page.goto("/#host/dev/session/session-1");
  await page.getByRole("button", { name: "Close session", exact: true }).first().click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByRole("heading", { name: `Close “${saved.title}”?`, exact: true })).toBeVisible();
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  expect(fixture.mutations).toEqual([]);
  await expect(page.getByRole("heading", { name: saved.title, exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Close session", exact: true }).first().click();
  await dialog.getByRole("button", { name: "Close session", exact: true }).click();
  await expect(page).toHaveURL(/#host\/dev$/);
  await expect(page.getByRole("heading", { name: "Development", exact: true })).toBeVisible();
  expect(fixture.mutations).toEqual([{
    method: "DELETE", path: "/api/hosts/dev/runtime/sessions/session-1", body: undefined, csrf: "fixture-csrf",
  }]);
  expect(fixture.sessions.dev).toEqual([sibling]);
  expect(fixture.sessions.local).toEqual([work]);
});

for (const failure of ["rejected", "lost reply"] as const) {
  test(`a ${failure} shell creation keeps the saved session open and never replays the request`, async ({ page }) => {
    const fixture = await mockFleet(page, { sessions: { dev: [{ ...saved }] } });
    let attempts = 0;
    await page.route("**/api/hosts/dev/runtime/sessions", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      attempts++;
      if (failure === "rejected") return route.fulfill({ status: 400, json: { error: "The project folder is no longer available." } });
      // Model an accepted create whose response was lost. Polling may discover
      // it, but must not repeat the mutation or replace the old session view.
      fixture.sessions.dev.push({ ...route.request().postDataJSON(), id: "accepted-once", status: "running", createdAt: work.createdAt, updatedAt: work.updatedAt });
      return route.abort("failed");
    });
    await page.goto("/#host/dev/session/session-1");
    await page.getByRole("button", { name: "Open terminal here", exact: true }).click();
    await expect(page.getByRole("alert")).toContainText(failure === "rejected" ? "The project folder is no longer available." : "The action may have completed");
    await expect(page.getByText("Saved output", { exact: true })).toBeVisible();
    const polls = fixture.sessionRequests.dev;
    await expect.poll(() => fixture.sessionRequests.dev).toBeGreaterThan(polls);
    await expect(page).toHaveURL(/#host\/dev\/session\/session-1$/);
    expect(attempts).toBe(1);
    expect(fixture.sessions.dev.find((session) => session.id === saved.id)).toEqual(saved);
    expect(fixture.sessions.dev.filter((session) => session.id === "accepted-once")).toHaveLength(failure === "lost reply" ? 1 : 0);
    expect(fixture.mutations.some((entry) => entry.method === "DELETE" || entry.path.endsWith("/disconnect"))).toBeFalsy();
  });
}

test("double click and a delayed create reply produce one terminal while close is disabled", async ({ page }) => {
  const fixture = await mockFleet(page, { sessions: { dev: [{ ...saved }] } });
  let release!: () => void;
  const reply = new Promise<void>((resolve) => { release = resolve; });
  let attempts = 0;
  await page.route("**/api/hosts/dev/runtime/sessions", async (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    attempts++;
    await reply;
    return route.fallback();
  });
  await page.goto("/#host/dev/session/session-1");
  await page.getByRole("button", { name: "Open terminal here", exact: true }).click({ clickCount: 2 });
  await expect.poll(() => attempts).toBe(1);
  await expect(page.getByRole("button", { name: /Open(ing)? terminal/ })).toBeDisabled();
  const close = page.getByRole("button", { name: "Close session", exact: true });
  for (let i = 0; i < await close.count(); i++) await expect(close.nth(i)).toBeDisabled();
  await expect(page).toHaveURL(/#host\/dev\/session\/session-1$/);
  release();
  await expect(page.getByRole("heading", { name: `${saved.title} · Terminal`, exact: true })).toBeVisible();
  expect(attempts).toBe(1);
  expect(fixture.sessions.dev).toHaveLength(2);
  expect(fixture.sessions.dev.find((session) => session.id === saved.id)).toEqual(saved);
});

test("running sessions and setup jobs do not offer ordinary finished-session actions", async ({ page }) => {
  const sessions: Session[] = [{ ...saved, id: "running", status: "running", exitCode: undefined },
    ...(["install", "login", "update", "repair"] as const).map((purpose) => ({ ...saved, id: purpose, purpose }))];
  const fixture = await mockFleet(page, { sessions: { dev: sessions } });
  for (const session of sessions) {
    await page.goto(`/#host/dev/session/${session.id}`);
    await expect(page.getByRole("heading", { name: saved.title, exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Open terminal here", exact: true })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Close session", exact: true })).toHaveCount(0);
  }
  expect(fixture.mutations).toEqual([]);
});

test("a disconnected machine keeps its saved session but disables terminal and close actions", async ({ page }) => {
  const fixture = await mockFleet(page, { sessions: { dev: [{ ...saved }] } });
  await page.goto("/#host/dev/session/session-1");
  await expect(page.getByRole("button", { name: "Open terminal here", exact: true })).toBeEnabled();
  fixture.hosts.find((host) => host.id === "dev")!.status = "disconnected";
  await expect(page.getByText("This machine is disconnected.", { exact: false })).toBeVisible();
  await expect(page.getByRole("button", { name: "Open terminal here", exact: true })).toBeDisabled();
  const close = page.getByRole("button", { name: "Close session", exact: true });
  await expect(close.first()).toBeVisible();
  for (let i = 0; i < await close.count(); i++) await expect(close.nth(i)).toBeDisabled();
  expect(fixture.sessions.dev).toEqual([saved]);
  expect(fixture.mutations).toEqual([]);
});
