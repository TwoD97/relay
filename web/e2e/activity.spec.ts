import { expect, test } from "@playwright/test";
import { mockFleet, openNavigation, remote, work } from "./fixture";
import type { Approval, ObserverState, Session } from "../src/types";

const agent: Session = { ...work, harness: "codex", permissions: { support: "active", detail: "A verified provider hook connected.", mode: "default" } };
const approval = (overrides: Partial<Approval> = {}): Approval => ({ id: "request-1", sessionId: agent.id, sessionCreatedAt: agent.createdAt, provider: "codex", toolName: "Bash", input: { command: "npm test && echo '<script>alert(1)</script>'" }, cwd: agent.cwd, permissionMode: "default", createdAt: new Date().toISOString(), expiresAt: new Date(Date.now() + 600000).toISOString(), status: "pending", ...overrides });
const observer = (enabled = false): ObserverState => ({ limits: { requestsPerHour: 60, remaining: 59 }, config: { enabled, provider: "claude", model: "haiku", intervalSeconds: 300 }, running: false, summaries: [], providers: [{ id: "claude", supported: true }, { id: "codex", supported: false, detail: "Codex cannot disable every tool in a verified native CLI mode." }] });
const inbox = (page: import("@playwright/test").Page, host = "Development") => page.getByRole("region", { name: `${host} approvals`, exact: true });
const summaries = (page: import("@playwright/test").Page, host = "Development") => page.getByRole("region", { name: `${host} summaries`, exact: true });

test("approval inbox scopes identical request IDs to their host and submits exactly once", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [{ ...remote }, { ...remote, id: "other", name: "Other machine" }], sessions: { dev: [{ ...agent }], other: [{ ...agent }] }, approvals: { dev: [approval()], other: [approval()] } });
  await page.goto("/");
  await page.getByRole("button", { name: "2 pending approval requests · Review in Activity" }).click();
  await expect(page.getByRole("heading", { name: "Activity", exact: true })).toBeVisible();
  const card = inbox(page).getByRole("article");
  await expect(card.locator("pre")).toBeVisible();
  await expect(card.locator("pre")).toContainText("<script>alert(1)</script>");
  expect(await card.locator("script").count()).toBe(0);
  await card.getByRole("button", { name: "Approve once", exact: true }).dblclick();
  await expect(card).toContainText("Approved once");
  await expect(card).toContainText("Tool execution or completion is not confirmed");
  expect(fixture.mutations).toEqual([{ method: "POST", path: "/api/hosts/dev/runtime/approvals/request-1/decision", body: { sessionId: agent.id, sessionCreatedAt: agent.createdAt, decision: "allow" }, csrf: "fixture-csrf" }]);
  await expect(inbox(page, "Other machine").getByRole("button", { name: "Approve once", exact: true })).toBeEnabled();
  expect(fixture.terminalMessages.filter((entry) => entry.type === "input")).toEqual([]);
});

test("denial is a distinct exact request action and never sends terminal keys", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent }] }, approvals: { dev: [approval()] } });
  await page.goto("/#activity");
  await inbox(page).getByRole("button", { name: "Deny", exact: true }).click();
  await expect(inbox(page)).toContainText("Denied");
  expect(fixture.mutations[0].body).toEqual({ sessionId: agent.id, sessionCreatedAt: agent.createdAt, decision: "deny" });
  expect(fixture.attachments).toEqual([]);
});

test("Continue in terminal defers a pending hook, while ordinary session navigation makes no decision", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent }] }, approvals: { dev: [approval()] } });
  await page.goto("/#host/dev/session/session-1");
  await expect(page.getByRole("region", { name: `${agent.title} terminal` })).toBeVisible();
  expect(fixture.mutations).toEqual([]);
  await page.getByRole("button", { name: "Review requests", exact: true }).click();
  await inbox(page).getByRole("button", { name: "Continue in terminal", exact: true }).click();
  await expect(page).toHaveURL(/#host\/dev\/session\/session-1$/);
  expect(fixture.mutations[0].body).toEqual({ sessionId: agent.id, sessionCreatedAt: agent.createdAt, decision: "terminal" });
});

test("lost approval reply stays uncertain across polling, navigation and reload without replay", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent }] }, approvals: { dev: [approval()] } });
  let attempts = 0;
  await page.route("**/approvals/*/decision", async (route) => { attempts++; await route.abort("failed"); });
  await page.goto("/#activity");
  await inbox(page).getByRole("button", { name: "Approve once", exact: true }).click();
  await expect(inbox(page).getByRole("alert")).toContainText("Relay will not resend");
  await expect(inbox(page).getByRole("button", { name: "Approve once", exact: true })).toBeDisabled();
  await expect.poll(() => fixture.stateRequests()).toBeGreaterThan(1);
  await inbox(page).getByRole("button", { name: "Open terminal", exact: true }).click();
  await expect(page).toHaveURL(/#host\/dev\/session\/session-1$/);
  await page.goto("/#activity");
  await page.reload();
  await expect(inbox(page)).toContainText("Decision outcome unknown");
  await expect(inbox(page).getByRole("button", { name: "Approve once", exact: true })).toBeDisabled();
  expect(attempts).toBe(1);
});

test("late terminal decision never steals navigation", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent }] }, approvals: { dev: [approval()] } });
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  let calls = 0;
  await page.route("**/approvals/*/decision", async (route) => { calls++; await gate; await route.fallback(); });
  await page.goto("/#activity");
  await inbox(page).getByRole("button", { name: "Continue in terminal", exact: true }).click();
  await expect(inbox(page)).toContainText("Sending decision");
  await page.getByRole("button", { name: "Workspace", exact: true }).click();
  release();
  await expect.poll(() => fixture.mutations.length).toBe(1);
  await expect(page.getByRole("heading", { name: "A home for your fleet." })).toBeVisible();
  expect(calls).toBe(1);
});

test("expired, replaced, offline and unavailable request state never permits a decision", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent }] }, approvals: { dev: [approval({ id: "expired", expiresAt: new Date(Date.now() - 1000).toISOString() }), approval({ id: "replaced", sessionCreatedAt: "2025-01-01T00:00:00Z" }), approval()] } });
  await page.goto("/#activity");
  const cards = inbox(page).getByRole("article");
  await expect(cards.filter({ hasText: "Request expired" }).getByRole("button", { name: "Approve once" })).toHaveCount(0);
  await expect(cards.filter({ hasText: "no longer matches" }).getByRole("button", { name: "Approve once" })).toBeDisabled();
  await expect(cards.filter({ hasText: "no longer matches" }).getByRole("button", { name: "Open terminal" })).toBeDisabled();
  await page.route("**/runtime/approvals", (route) => route.fulfill({ status: 503, json: { error: "Temporary bridge failure" } }));
  await expect(inbox(page).getByRole("alert")).toContainText("Temporary bridge failure");
  for (const button of await inbox(page).getByRole("button", { name: "Approve once", exact: true }).all()) await expect(button).toBeDisabled();
  fixture.hosts[0].status = "disconnected";
  await expect(inbox(page)).toContainText("Machine offline");
  expect(fixture.mutations).toEqual([]);
});

test("unsupported runtime and configured provider retain terminal prompts without invented approval buttons", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent, permissions: { support: "configured", detail: "Review Relay hooks with /hooks before trusting them." }, attention: { kind: "permission", source: "codex-notify", updatedAt: agent.updatedAt } }] } });
  await page.goto("/#activity");
  await expect(inbox(page)).toContainText("Terminal permission prompts only");
  await expect(inbox(page)).toContainText("Review Relay hooks with /hooks");
  await expect(inbox(page).getByRole("button", { name: "Approve once" })).toHaveCount(0);
  await expect(summaries(page)).toContainText("Runtime upgrade required");
  await inbox(page).getByRole("button", { name: "Open terminal", exact: true }).click();
  await expect(page).toHaveURL(/#host\/dev\/session\/session-1$/);
  expect(fixture.mutations).toEqual([]);
});

test("summary settings require explicit host-scoped opt-in and preserve model and cadence", async ({ page }, testInfo) => {
  const fixture = await mockFleet(page, { hosts: [{ ...remote }, { ...remote, id: "other", name: "Other machine" }], sessions: { dev: [{ ...agent }], other: [{ ...agent }] }, approvals: { dev: [], other: [] }, observers: { dev: observer(), other: observer() } });
  await page.goto("/#activity");
  await expect(summaries(page)).toContainText("Summaries off");
  await expect(summaries(page).getByRole("button", { name: "Refresh summary" })).toBeDisabled();
  expect(fixture.mutations).toEqual([]);
  await summaries(page, "Other machine").getByRole("button", { name: "Settings", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Summary settings" });
  await expect(dialog).toContainText("Other machine");
  await expect(dialog.getByRole("checkbox")).not.toBeChecked();
  await expect(dialog).toContainText("Codex cannot disable every tool");
  await expect(dialog).toContainText("60 requests per hour");
  await page.screenshot({ path: `artifacts/screenshots/activity-settings-${testInfo.project.name}.png`, fullPage: true });
  await dialog.getByRole("checkbox").check();
  await dialog.getByLabel("Summary model", { exact: true }).fill("my-small-model");
  await dialog.getByLabel("Check interval (seconds)").fill("60");
  await dialog.getByRole("button", { name: "Save settings", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(summaries(page, "Other machine")).toContainText("my-small-model");
  expect(fixture.mutations).toEqual([{ method: "POST", path: "/api/hosts/other/runtime/observer/config", body: { enabled: true, provider: "claude", model: "my-small-model", intervalSeconds: 60 }, csrf: "fixture-csrf" }]);
  expect(fixture.observers.dev.config.enabled).toBe(false);
});

test("summaries are advisory snapshots with exact identity, honest stale state and explicit refresh", async ({ page }, testInfo) => {
  const state = observer(true);
  state.summaries = [{ sessionId: agent.id, sessionCreatedAt: agent.createdAt, provider: "claude", model: "haiku", status: "ready", summary: "Implemented request validation. Tests are still running. <script>not markup</script>", steps: ["Added request identity checks"], nextSteps: ["Inspect the test results"], blockers: ["Needs a human decision"], sampledAt: "2026-10-09T11:00:00Z", generatedAt: "2026-10-09T11:00:03Z", updatedAt: "2026-10-09T11:00:03Z", sourceStatus: "running", stale: true }];
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent }, { ...agent, id: "setup", title: "Private sign-in", purpose: "login" }, { ...work, id: "shell" }] }, approvals: { dev: [approval()] }, observers: { dev: state } });
  await page.goto("/#activity");
  const region = summaries(page);
  await expect(region.getByRole("article")).toHaveCount(1);
  await expect(region).toContainText("Stale summary");
  await expect(region).toContainText("Reported steps");
  await expect(region).toContainText("Possible blockers");
  await expect(region).toContainText("Suggested next steps");
  await expect(region).toContainText("earlier snapshot");
  expect(await region.locator("script").count()).toBe(0);
  await page.screenshot({ path: `artifacts/screenshots/activity-${testInfo.project.name}.png`, fullPage: true });
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await region.scrollIntoViewIfNeeded();
  await page.screenshot({ path: `artifacts/screenshots/activity-summary-${testInfo.project.name}.png`, fullPage: true });
  await region.getByRole("button", { name: "Refresh summary", exact: true }).click();
  await expect(region).toContainText("Processing one summary");
  expect(fixture.mutations[0].body).toEqual({ sessionId: agent.id, sessionCreatedAt: agent.createdAt });
  expect(fixture.mutations[0].path).toBe("/api/hosts/dev/runtime/observer/refresh");
  expect(fixture.terminalMessages.filter((entry) => entry.type === "input")).toEqual([]);
});

test("unknown summary mutations are not repeated and failed configuration remains inspectable", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent }] }, observers: { dev: observer(true) } });
  let calls = 0;
  await page.route("**/runtime/observer/refresh", async (route) => { calls++; await route.abort("failed"); });
  await page.goto("/#activity");
  await summaries(page).getByRole("button", { name: "Refresh summary", exact: true }).click();
  await expect(summaries(page).getByRole("alert")).toContainText("may have completed");
  await expect(summaries(page).getByRole("button", { name: "Refresh summary", exact: true })).toBeDisabled();
  await expect.poll(() => fixture.stateRequests()).toBeGreaterThan(1);
  expect(calls).toBe(1);
  fixture.observers.dev.summaries = [{ sessionId: agent.id, sessionCreatedAt: agent.createdAt, provider: "claude", model: "haiku", status: "ready", summary: "Authoritative completed summary", steps: [], nextSteps: [], blockers: [], updatedAt: new Date().toISOString(), stale: false, sourceStatus: "running" }];
  await expect(summaries(page)).toContainText("Authoritative completed summary");
  await expect(summaries(page).getByRole("alert")).toHaveCount(0);
  await expect(summaries(page).getByRole("button", { name: "Refresh summary", exact: true })).toBeEnabled();
  expect(calls).toBe(1);
  await page.route("**/runtime/observer/config", (route) => route.fulfill({ status: 503, json: { error: "Could not save configuration" } }));
  await summaries(page).getByRole("button", { name: "Settings", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Summary settings" });
  await dialog.getByRole("checkbox").uncheck();
  await dialog.getByRole("button", { name: "Save settings" }).click();
  await expect(dialog.getByRole("alert")).toContainText("Could not save configuration");
  await expect(dialog.getByRole("button", { name: "Save settings" })).toBeDisabled();
  await dialog.getByRole("button", { name: "Close and check state" }).click();
  await expect(summaries(page)).toContainText("Enabled");
});

test("Activity is keyboard reachable and a reused session ID does not inherit a summary", async ({ page }) => {
  const state = observer();
  state.summaries.push({ sessionId: agent.id, sessionCreatedAt: "2025-01-01T00:00:00Z", provider: "claude", model: "haiku", status: "ready", summary: "OTHER SESSION PRIVATE CONTENT", steps: [], nextSteps: [], blockers: [], updatedAt: "2025-01-01T00:00:00Z", stale: false, sourceStatus: "exited" });
  await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent }] }, observers: { dev: state } });
  await page.goto("/");
  await openNavigation(page);
  const nav = page.getByRole("button", { name: "Activity", exact: true });
  await nav.focus();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/#activity$/);
  await expect(summaries(page)).toContainText("No summary yet");
  await expect(page.getByText("OTHER SESSION PRIVATE CONTENT")).toHaveCount(0);
});


test("another viewer's decision is rejected once and the authoritative result replaces it", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent }] }, approvals: { dev: [approval()] } });
  let calls = 0;
  await page.route("**/approvals/*/decision", async (route) => { calls++; await route.fulfill({ status: 409, json: { error: "Another viewer already decided this request." } }); });
  await page.goto("/#activity");
  await inbox(page).getByRole("button", { name: "Approve once", exact: true }).click();
  await expect(inbox(page)).toContainText("Decision rejected");
  await expect(inbox(page).getByRole("alert")).toContainText("will not be resent");
  await expect(inbox(page).getByRole("button", { name: "Deny", exact: true })).toBeDisabled();
  Object.assign(fixture.approvals.dev[0], { status: "submitted", decision: "deny" });
  await expect(inbox(page)).toContainText("Denied");
  await expect(inbox(page).getByRole("alert")).toHaveCount(0);
  expect(calls).toBe(1);
});

test("an expired request disables locally even when the server keeps returning pending", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent }] }, approvals: { dev: [approval({ expiresAt: new Date(Date.now() + 3500).toISOString() })] } });
  await page.goto("/#activity");
  await expect(inbox(page).getByRole("button", { name: "Approve once" })).toBeEnabled();
  await expect(inbox(page)).toContainText("Request expired");
  await expect(inbox(page).getByRole("button", { name: "Approve once" })).toHaveCount(0);
  expect(fixture.approvals.dev[0].status).toBe("pending");
  expect(fixture.mutations).toEqual([]);
});

test("observer read failures keep existing summaries visible without scheduling model work", async ({ page }) => {
  const state = observer(true);
  state.error = "Claude Code is not authenticated on this machine.";
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent }] }, observers: { dev: state } });
  await page.goto("/#activity");
  await expect(summaries(page).getByRole("alert")).toContainText("not authenticated");
  await page.route("**/runtime/observer", (route) => route.fulfill({ status: 503, json: { error: "Observer bridge is unavailable" } }));
  await expect(summaries(page)).toContainText("Observer bridge is unavailable");
  await expect(summaries(page).getByRole("button", { name: "Refresh summary" })).toBeDisabled();
  await expect(summaries(page).getByRole("button", { name: "Settings" })).toBeDisabled();
  expect(fixture.mutations).toEqual([]);
});


test("a rejected summary refresh can be explicitly retried after its hourly budget recovers", async ({ page }) => {
  const state = observer(true);
  state.limits = { requestsPerHour: 60, remaining: 0 };
  state.summaries = [{ sessionId: agent.id, sessionCreatedAt: agent.createdAt, provider: "claude", model: "haiku", status: "error", summary: "", steps: [], nextSteps: [], blockers: [], updatedAt: "2026-10-09T11:00:00Z", stale: true, sourceStatus: "running", error: "Summary was interrupted. Refresh explicitly." }];
  const fixture = await mockFleet(page, { hosts: [{ ...remote }], sessions: { dev: [{ ...agent, attention: { kind: "permission", source: "codex-hook", updatedAt: agent.updatedAt } }] }, observers: { dev: state } });
  let calls = 0;
  await page.route("**/runtime/observer/refresh", async (route) => {
    calls++;
    if (calls === 1) return route.fulfill({ status: 429, json: { error: "This host has reached its summary limit of 60 requests per hour" } });
    return route.fallback();
  });
  await page.goto("/#activity");
  await summaries(page).getByRole("button", { name: "Refresh summary", exact: true }).click();
  await expect(summaries(page).getByRole("alert")).toContainText("summary limit");
  await expect(summaries(page)).toContainText("The request was rejected");
  fixture.observers.dev.limits!.remaining = 1;
  await expect(summaries(page)).toContainText("1 of 60 summary requests");
  expect(calls).toBe(1);
  await summaries(page).getByRole("button", { name: "Refresh summary", exact: true }).click();
  await expect(summaries(page)).toContainText("Queued");
  expect(calls).toBe(2);
  expect(fixture.mutations).toHaveLength(1);
  expect(fixture.mutations[0].body).toEqual({ sessionId: agent.id, sessionCreatedAt: agent.createdAt });
  await summaries(page).getByRole("button", { name: "Open terminal", exact: true }).click();
  await expect(page.getByText("Codex hook", { exact: true })).toBeVisible();
});
