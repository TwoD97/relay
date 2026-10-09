import { expect, test } from "@playwright/test";
import { mockFleet, openNavigation, remote, work } from "./fixture";

test("startup reconnect happens once and queued setup attaches without restarting SSH", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [{ ...remote, status: "disconnected", stage: "Ready to connect" }], autoReconnect: true, deferSetupAttachments: 1 });
  await page.goto("/");
  await expect.poll(() => fixture.reconnectRequests.length).toBe(1);
  expect(fixture.reconnectRequests[0].csrf).toBe("fixture-csrf");
  await expect(page.getByText("Waiting for another host setup")).toBeVisible();
  await page.goto("/#host/dev");
  await page.getByRole("button", { name: "Open setup", exact: true }).click();
  await expect.poll(() => fixture.attachments.filter((path) => path.endsWith("setup-terminal")).length).toBeGreaterThanOrEqual(2);
  await expect(page.getByRole("region", { name: "SSH setup terminal" })).toContainText("Password:");
  await expect.poll(() => fixture.stateRequests()).toBeGreaterThan(1);
  expect(fixture.reconnectRequests).toHaveLength(1);
  expect(fixture.mutations.filter((entry) => entry.path.endsWith("/connect"))).toHaveLength(0);
});

test("a lost startup reconnect reply leaves the fleet usable and retries only on request", async ({ page }) => {
  const fixture = await mockFleet(page);
  let attempts = 0;
  await page.route("**/api/reconnect", async (route) => {
    attempts++;
    if (attempts === 1) return route.abort("failed");
    return route.fallback();
  });
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "A home for your fleet." })).toBeVisible();
  await expect(page.getByRole("alert")).toContainText("The action may have completed");
  await expect.poll(() => fixture.stateRequests()).toBeGreaterThan(1);
  expect(attempts).toBe(1);
  await page.getByRole("button", { name: "Retry connections", exact: true }).click();
  await expect(page.getByRole("alert")).toHaveCount(0);
  expect(attempts).toBe(2);
});

test("runtime update keeps sessions available and distinguishes installed from running versions", async ({ page }) => {
  const fixture = await mockFleet(page);
  await page.goto("/#host/dev");
  const maintenance = page.getByRole("region", { name: "Relay runtime maintenance" });
  await maintenance.getByRole("button", { name: "Update or repair runtime", exact: true }).click();
  await expect(maintenance.getByRole("status")).toContainText("Checking installed runtime");
  await expect(maintenance.getByRole("button", { name: "Working…", exact: true })).toBeDisabled();
  await expect(page.getByRole("button", { name: "New session", exact: true }).first()).toBeEnabled();
  expect(fixture.sessions.dev).toEqual([work]);
  expect(fixture.mutations).toContainEqual(expect.objectContaining({ method: "POST", path: "/api/hosts/dev/repair-runtime", csrf: "fixture-csrf" }));
  fixture.hosts.find((host) => host.id === "dev")!.runtimeOperation = { status: "completed", stage: "Runtime installed; running sessions preserved", installedVersion: "0.2.0", runningVersion: "0.1.0", restartRequired: true };
  await expect(maintenance.getByRole("status")).toContainText("planned runtime restart");
  await expect(maintenance).toContainText("0.2.0");
  await expect(maintenance).toContainText("0.1.0");
  await expect(maintenance.getByRole("button", { name: "Update or repair runtime", exact: true })).toBeEnabled();
  expect(fixture.mutations.some((entry) => entry.method === "DELETE" || entry.path.endsWith("/disconnect"))).toBeFalsy();
});

test("runtime maintenance failures are visible and local installations have no remote repair action", async ({ page }) => {
  const fixture = await mockFleet(page);
  fixture.hosts.find((host) => host.id === "dev")!.runtimeOperation = { status: "error", stage: "Runtime repair failed", error: "Cannot write the release directory." };
  await page.goto("/#host/dev");
  await expect(page.getByRole("region", { name: "Relay runtime maintenance" }).getByRole("alert")).toContainText("Cannot write");
  await page.goto("/#host/local");
  await expect(page.getByRole("region", { name: "Relay runtime maintenance" })).toContainText("Managed by the Relay installation");
  await expect(page.getByRole("button", { name: "Update or repair runtime", exact: true })).toHaveCount(0);
});

for (const [harness, name] of [["codex", "Codex"], ["claude", "Claude Code"]] as const) {
  for (const action of ["update", "repair"] as const) {
    test(`${name} ${action} stays in the background and successful cleanup preserves working sessions`, async ({ page }, testInfo) => {
      const fixture = await mockFleet(page);
      await page.goto("/#host/dev");
      const agent = page.getByRole("article", { name, exact: true });
      if (harness === "codex") await expect(agent).toContainText("Authenticated");
      await agent.getByRole("button", { name: action === "update" ? "Update agent" : "Repair", exact: true }).click();
      const progress = page.getByRole("region", { name: "Agent maintenance", exact: true }).getByRole("article", { name: `${name} ${action}`, exact: true });
      await expect(progress).toContainText(`Preparing ${name} ${action}`);
      await expect(page).toHaveURL(/#host\/dev$/);
      await expect(page.getByRole("heading", { name: "Development", exact: true })).toBeVisible();
      const job = fixture.maintenanceJobs[0];
      const worker = fixture.startMaintenanceJob(job.id);
      await expect(progress).toContainText(`${action === "update" ? "Updating" : "Repairing"} ${name}`);
      await expect(progress.getByRole("button", { name: "View output", exact: true })).toBeEnabled();
      await expect(agent.getByRole("button", { name: "Update agent", exact: true })).toBeDisabled();
      await expect(agent.getByRole("button", { name: "Repair", exact: true })).toBeDisabled();
      await expect(page.locator(".sessions-list")).not.toContainText(worker.title);
      await expect(page.locator(".host-tree")).not.toContainText(worker.title);
      expect(fixture.attachments).toEqual([]);
      expect(fixture.mutations).toEqual([{ method: "POST", path: `/api/hosts/dev/harnesses/${harness}/${action}`, csrf: "fixture-csrf", body: undefined }]);
      if (harness === "codex" && action === "update") await page.screenshot({ path: testInfo.outputPath("background-maintenance.png"), fullPage: true, animations: "disabled" });
      fixture.finishMaintenanceJob(job.id, "succeeded");
      await expect(progress.getByRole("status")).toContainText(`${name} ${action === "update" ? "updated" : "repaired"} successfully`);
      await expect(progress.getByRole("button", { name: "View output", exact: true })).toHaveCount(0);
      await expect(agent.getByRole("button", { name: action === "update" ? "Update agent" : "Repair", exact: true })).toBeEnabled();
      await expect(page).toHaveURL(/#host\/dev$/);
      expect(fixture.sessions.dev).toEqual([work]);
      expect(fixture.attachments).toEqual([]);
      expect(fixture.mutations.some((entry) => entry.method === "DELETE" || entry.path.endsWith("/disconnect"))).toBeFalsy();
    });
  }
}

test("busy maintenance reports its error without replay or disconnect", async ({ page }) => {
  const fixture = await mockFleet(page);
  let attempts = 0;
  await page.route("**/api/hosts/dev/harnesses/codex/update", async (route) => {
    attempts++;
    await route.fulfill({ status: 409, json: { error: "Codex maintenance is already running." } });
  });
  await page.goto("/#host/dev");
  const codex = page.getByRole("article", { name: "Codex", exact: true });
  await codex.getByRole("button", { name: "Update agent", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("maintenance is already running");
  await expect(codex.getByRole("button", { name: "Update agent", exact: true })).toBeEnabled();
  await expect.poll(() => fixture.stateRequests()).toBeGreaterThan(1);
  expect(attempts).toBe(1);
  expect(fixture.sessions.dev).toEqual([work]);
  expect(fixture.attachments).toEqual([]);
});

test("older remote runtimes hide owned shell workers while unsupported local actions remain disabled", async ({ page }) => {
  const fixture = await mockFleet(page, { legacyRuntime: true });
  await page.goto("/#host/dev");
  await page.getByRole("article", { name: "Codex", exact: true }).getByRole("button", { name: "Update agent", exact: true }).click();
  await expect.poll(() => fixture.maintenanceJobs.length).toBe(1);
  const job = fixture.maintenanceJobs[0];
  const worker = fixture.startMaintenanceJob(job.id);
  expect(worker.purpose).toBeUndefined();
  expect(worker.harness).toBe("shell");
  await expect(page.getByRole("region", { name: "Agent maintenance" })).toContainText("Updating Codex");
  await expect(page.getByRole("region", { name: "Agent maintenance" }).getByRole("button", { name: "View output", exact: true })).toBeEnabled();
  await expect(page.locator(".sessions-list")).not.toContainText(worker.title);
  await expect(page.locator(".host-tree")).not.toContainText(worker.title);
  await expect(page).toHaveURL(/#host\/dev$/);
  expect(fixture.mutations).toContainEqual(expect.objectContaining({ path: "/api/hosts/dev/harnesses/codex/update" }));
  fixture.finishMaintenanceJob(job.id, "succeeded");
  await expect(page.getByRole("region", { name: "Agent maintenance" })).toContainText("Codex updated successfully");
  expect(fixture.sessions.dev).toEqual([work]);
  await page.goto("/#host/local");
  const localCodex = page.getByRole("article", { name: "Codex", exact: true });
  await expect(localCodex.getByRole("button", { name: "Update agent", exact: true })).toBeDisabled();
  await expect(localCodex.getByRole("button", { name: "Repair", exact: true })).toBeDisabled();
  await expect(localCodex).toContainText("need a newer running Relay runtime");
  expect(fixture.mutations.some((entry) => entry.path.includes("/hosts/local/"))).toBeFalsy();
});

for (const status of ["failed", "uncertain"] as const) {
  test(`${status} background maintenance preserves output and opens it only on request`, async ({ page }) => {
    const fixture = await mockFleet(page, { legacyRuntime: true, terminalOutput: "MAINTENANCE-DIAGNOSTIC\r\n" });
    await page.goto("/#host/dev");
    await page.getByRole("article", { name: "Codex", exact: true }).getByRole("button", { name: "Repair", exact: true }).click();
    await expect.poll(() => fixture.maintenanceJobs.length).toBe(1);
    const job = fixture.maintenanceJobs[0];
    const worker = fixture.startMaintenanceJob(job.id);
    const error = status === "failed" ? "The installer exited with code 1." : "The startup result is uncertain. Inspect output before retrying.";
    fixture.finishMaintenanceJob(job.id, status, error);
    const progress = page.getByRole("region", { name: "Agent maintenance" });
    await expect(progress.getByRole("alert")).toContainText(error);
    await expect(page).toHaveURL(/#host\/dev$/);
    expect(fixture.attachments).toEqual([]);
    await expect(page.locator(".sessions-list")).not.toContainText(worker.title);
    await progress.getByRole("button", { name: "View output", exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`#host/dev/session/${worker.id}$`));
    await expect(page.getByText("Saved output", { exact: true })).toBeVisible();
    await expect(page.getByRole("region", { name: `${worker.title} terminal`, exact: true })).toContainText("MAINTENANCE-DIAGNOSTIC");
    expect(fixture.sessions.dev).toContainEqual(worker);
    expect(fixture.sessions.dev.find((session) => session.id === work.id)).toEqual(work);
    expect(fixture.mutations.some((entry) => entry.method === "DELETE" || entry.path.endsWith("/disconnect"))).toBeFalsy();
  });
}

test("running output is optional and server cleanup returns an open worker view to its machine", async ({ page }) => {
  const fixture = await mockFleet(page, { terminalOutput: "INSTALLER-PROGRESS\r\n" });
  await page.goto("/#host/dev");
  await page.getByRole("article", { name: "Codex", exact: true }).getByRole("button", { name: "Update agent", exact: true }).click();
  await expect.poll(() => fixture.maintenanceJobs.length).toBe(1);
  const job = fixture.maintenanceJobs[0];
  const worker = fixture.startMaintenanceJob(job.id);
  await page.getByRole("region", { name: "Agent maintenance" }).getByRole("button", { name: "View output", exact: true }).click();
  await expect(page.getByRole("region", { name: `${worker.title} terminal`, exact: true })).toContainText("INSTALLER-PROGRESS");
  fixture.finishMaintenanceJob(job.id, "succeeded");
  await expect(page).toHaveURL(/#host\/dev$/);
  await expect(page.getByRole("heading", { name: "Development", exact: true })).toBeVisible();
  await expect(page.getByRole("region", { name: "Agent maintenance" })).toContainText("Codex updated successfully");
  expect(fixture.sessions.dev).toEqual([work]);
});

test("background completion never changes a newer navigation choice", async ({ page }) => {
  const fixture = await mockFleet(page);
  await page.goto("/#host/dev");
  await page.getByRole("article", { name: "Codex", exact: true }).getByRole("button", { name: "Update agent", exact: true }).click();
  await expect.poll(() => fixture.maintenanceJobs.length).toBe(1);
  const job = fixture.maintenanceJobs[0];
  fixture.startMaintenanceJob(job.id);
  await page.getByRole("button", { name: "Workspace", exact: true }).click();
  await expect(page.getByRole("heading", { name: "A home for your fleet.", exact: true })).toBeVisible();
  fixture.finishMaintenanceJob(job.id, "succeeded");
  const polls = fixture.stateRequests();
  await expect.poll(() => fixture.stateRequests()).toBeGreaterThan(polls + 1);
  await expect(page.getByRole("heading", { name: "A home for your fleet.", exact: true })).toBeVisible();
  expect(fixture.sessions.dev).toEqual([work]);
  expect(fixture.attachments).toEqual([]);
});

for (const legacyRuntime of [false, true]) {
  test(`server-owned ${legacyRuntime ? "legacy" : "current"} maintenance survives reload without browser tracking`, async ({ page }) => {
    const fixture = await mockFleet(page, { legacyRuntime });
    await page.goto("/#host/dev");
    await page.getByRole("article", { name: "Claude Code", exact: true }).getByRole("button", { name: "Repair", exact: true }).click();
    await expect.poll(() => fixture.maintenanceJobs.length).toBe(1);
    const job = fixture.maintenanceJobs[0];
    const worker = fixture.startMaintenanceJob(job.id);
    await expect(page.getByRole("region", { name: "Agent maintenance" })).toContainText("Repairing Claude Code");
    await page.evaluate(() => sessionStorage.clear());
    await page.reload();
    await expect(page.getByRole("region", { name: "Agent maintenance" })).toContainText("Repairing Claude Code");
    await expect(page.locator(".sessions-list")).not.toContainText(worker.title);
    await expect(page).toHaveURL(/#host\/dev$/);
    fixture.finishMaintenanceJob(job.id, "succeeded");
    await expect(page.getByRole("region", { name: "Agent maintenance" })).toContainText("Claude Code repaired successfully");
    expect(fixture.sessions.dev).toEqual([work]);
    expect(fixture.mutations.filter((entry) => entry.path.endsWith("/repair"))).toHaveLength(1);
    expect(fixture.attachments).toEqual([]);
  });
}

test("unowned saved maintenance logs and an ordinary shell with a maintenance title remain visible", async ({ page }) => {
  const fixture = await mockFleet(page, { sessions: { dev: [
    { ...work, id: "completed", title: "Update Codex", purpose: "update", harness: "codex", status: "exited", exitCode: 0 },
    { ...work, id: "ordinary", title: "Repair Claude Code", workspace: "Setup" },
  ] } });
  await page.goto("/#host/dev");
  await expect(page.locator(".sessions-list").getByRole("button", { name: /Update Codex/ })).toBeVisible();
  await expect(page.locator(".sessions-list").getByRole("button", { name: /Repair Claude Code/ })).toBeVisible();
  await page.goto("/#host/dev/session/completed");
  await expect(page.getByText("Saved output", { exact: true })).toBeVisible();
  await page.goto("/#host/dev/session/ordinary");
  await expect(page.getByRole("heading", { name: "Repair Claude Code", exact: true })).toBeVisible();
  const ordinary = fixture.sessions.dev.find((session) => session.id === "ordinary")!;
  ordinary.status = "exited"; ordinary.exitCode = 0;
  await expect(page.getByText("Saved output", { exact: true })).toBeVisible();
  await expect(page).toHaveURL(/#host\/dev\/session\/ordinary$/);
  expect(fixture.sessions.dev).toHaveLength(2);
  expect(fixture.mutations).toEqual([]);
});

test("an old cleanup record cannot hide or navigate away from a different session identity", async ({ page }) => {
  const fixture = await mockFleet(page, { maintenanceJobs: [{
    id: "old-job", hostId: "dev", harness: "codex", action: "update", status: "succeeded", stage: "Maintenance completed", cleanupStatus: "removed",
    createdAt: work.createdAt, updatedAt: work.updatedAt, sessionId: work.id, sessionCreatedAt: "2026-10-06T08:00:00Z",
  }] });
  // Deliberately deliver job state before the session lookup. Missing data is
  // not evidence that the selected record belongs to the old cleanup job.
  let release!: () => void;
  const sessionsReady = new Promise<void>((resolve) => { release = resolve; });
  await page.route("**/api/hosts/dev/runtime/sessions", async (route) => { await sessionsReady; await route.fallback(); });
  await page.goto("/#host/dev/session/session-1");
  await expect.poll(() => fixture.stateRequests()).toBeGreaterThan(0);
  release();
  await expect(page.getByRole("heading", { name: work.title, exact: true })).toBeVisible();
  await expect(page).toHaveURL(/#host\/dev\/session\/session-1$/);
  await page.goto("/#host/dev");
  await expect(page.locator(".sessions-list").getByRole("button", { name: new RegExp(work.title) })).toBeVisible();
  expect(fixture.sessions.dev).toEqual([work]);
  expect(fixture.mutations).toEqual([]);
});

test("uncertain cleanup retains output and disables viewing while the host is offline", async ({ page }) => {
  const fixture = await mockFleet(page);
  await page.goto("/#host/dev");
  await page.getByRole("article", { name: "Codex", exact: true }).getByRole("button", { name: "Update agent", exact: true }).click();
  await expect.poll(() => fixture.maintenanceJobs.length).toBe(1);
  const job = fixture.maintenanceJobs[0];
  const worker = fixture.startMaintenanceJob(job.id);
  fixture.finishMaintenanceJob(job.id, "uncertain", "The removal response was lost; it was not repeated.");
  worker.status = "exited"; worker.exitCode = 0; job.cleanupStatus = "uncertain";
  const progress = page.getByRole("region", { name: "Agent maintenance" });
  await expect(progress.getByRole("alert")).toContainText("Could not confirm cleanup");
  await expect(progress.getByRole("button", { name: "View output", exact: true })).toBeEnabled();
  fixture.hosts.find((host) => host.id === "dev")!.status = "disconnected";
  await expect(progress.getByRole("button", { name: "View output", exact: true })).toBeDisabled();
  expect(fixture.sessions.dev).toContainEqual(worker);
  expect(fixture.mutations.filter((entry) => entry.method === "DELETE")).toEqual([]);
});

for (const [status, contentType] of [[200, "application/json"], [503, "text/html"]] as const) {
  test(`an HTML API reply with status ${status} explains the controller connection without leaking markup`, async ({ page }) => {
    await mockFleet(page);
    let attempts = 0;
    await page.route("**/api/hosts/dev/harnesses/codex/update", async (route) => {
      attempts++;
      expect(route.request().headers().accept).toBe("application/json");
      await route.fulfill({ status, contentType, body: "<!doctype html><html><body>PRIVATE-HTML-MARKER</body></html>" });
    });
    await page.goto("/#host/dev");
    await page.getByRole("article", { name: "Codex", exact: true }).getByRole("button", { name: "Update agent", exact: true }).click();
    await expect(page.getByRole("alert")).toContainText("Relay's API returned a web page");
    await expect(page.getByRole("alert")).toContainText("relay ui");
    await expect(page.getByRole("alert")).not.toContainText("PRIVATE-HTML-MARKER");
    await expect(page.getByRole("alert")).not.toContainText("Unexpected token");
    await expect(page).toHaveURL(/#host\/dev$/);
    expect(attempts).toBe(1);
  });
}

for (const [name, button, title] of [["Claude Code", "Install agent", "Install Claude Code"], ["Codex", "Manage sign-in", "Sign in to Codex"]] as const) {
  test(`${title} remains an interactive setup session rather than a background job`, async ({ page }) => {
    const fixture = await mockFleet(page);
    await page.goto("/#host/dev");
    await page.getByRole("article", { name, exact: true }).getByRole("button", { name: button, exact: true }).click();
    await expect(page.getByRole("heading", { name: title, exact: true })).toBeVisible();
    const job = fixture.sessions.dev.find((session) => session.id === "harness-job")!;
    job.status = "exited"; job.exitCode = 0;
    await expect(page.getByText("Saved output", { exact: true })).toBeVisible();
    await expect(page).toHaveURL(/#host\/dev\/session\/harness-job$/);
    expect(fixture.maintenanceJobs).toEqual([]);
    expect(fixture.sessions.dev).toContainEqual(job);
  });
}

test("a lost background launch reply discovers the accepted job without replaying it", async ({ page }) => {
  const fixture = await mockFleet(page);
  let attempts = 0;
  await page.route("**/api/hosts/dev/harnesses/codex/update", async (route) => {
    attempts++;
    fixture.maintenanceJobs.push({ id: "accepted-job", hostId: "dev", harness: "codex", action: "update", status: "preparing", stage: "Preparing Codex update", createdAt: work.createdAt, updatedAt: work.updatedAt });
    await route.abort("failed");
  });
  await page.goto("/#host/dev");
  await page.getByRole("article", { name: "Codex", exact: true }).getByRole("button", { name: "Update agent", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("The action may have completed");
  await expect(page.getByRole("region", { name: "Agent maintenance" })).toContainText("Preparing Codex update");
  await expect(page).toHaveURL(/#host\/dev$/);
  expect(attempts).toBe(1);
  expect(fixture.maintenanceJobs).toHaveLength(1);
  expect(fixture.attachments).toEqual([]);
});

test("a delayed background launch reply does not replace a newer navigation choice", async ({ page }) => {
  const fixture = await mockFleet(page);
  let release!: () => void;
  const reply = new Promise<void>((resolve) => { release = resolve; });
  let requested = false;
  await page.route("**/api/hosts/dev/harnesses/codex/update", async (route) => {
    requested = true;
    await reply;
    await route.fallback();
  });
  await page.goto("/#host/dev");
  await page.getByRole("article", { name: "Codex", exact: true }).getByRole("button", { name: "Update agent", exact: true }).click();
  await expect.poll(() => requested).toBeTruthy();
  await page.getByRole("button", { name: "Workspace", exact: true }).click();
  release();
  await expect.poll(() => fixture.maintenanceJobs.length).toBe(1);
  const polls = fixture.stateRequests();
  await expect.poll(() => fixture.stateRequests()).toBeGreaterThan(polls);
  await expect(page.getByRole("heading", { name: "A home for your fleet.", exact: true })).toBeVisible();
  expect(fixture.mutations.filter((entry) => entry.path.endsWith("/update"))).toHaveLength(1);
  expect(fixture.attachments).toEqual([]);
});

test("nonfatal skill warnings and maintenance errors stay visible outside filtered navigation", async ({ page }, testInfo) => {
  const fixture = await mockFleet(page);
  const host = fixture.hosts.find((host) => host.id === "dev")!;
  host.setupWarning = "An existing custom relay skill was preserved.";
  host.runtimeOperation = { status: "completed", stage: "Runtime installed; running sessions preserved", installedVersion: "0.2.0", runningVersion: "0.1.0", restartRequired: true };
  await page.goto("/#host/dev");
  await expect(page.getByText(host.setupWarning, { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "New session", exact: true }).first()).toBeEnabled();
  await expect(page.getByText("Coordinate sessions from your agent", { exact: true })).toBeVisible();
  await openNavigation(page);
  await page.getByRole("textbox", { name: "Filter machines and sessions" }).fill("not-a-project");
  const attention = page.getByRole("region", { name: "Needs attention" });
  await expect(attention).toContainText("Agent coordination setup");
  await attention.getByRole("button", { name: /Development/ }).click();
  await expect(page.getByRole("heading", { name: "Development", exact: true })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBeTruthy();
  await page.screenshot({ path: testInfo.outputPath("machine-maintenance.png"), fullPage: true, animations: "disabled" });
  await page.getByRole("region", { name: "Relay runtime maintenance" }).scrollIntoViewIfNeeded();
  await page.screenshot({ path: testInfo.outputPath("runtime-maintenance.png"), fullPage: true, animations: "disabled" });
  host.runtimeOperation = { status: "error", stage: "Runtime repair failed", error: "Disk is full." };
  await openNavigation(page);
  await expect(page.getByRole("region", { name: "Needs attention" })).toContainText("Runtime maintenance failed");
});
