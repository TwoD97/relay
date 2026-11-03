import { expect, test, type Locator, type Page } from "@playwright/test";
import { local, mockFleet, openNavigation, remote, work } from "./fixture";
import type { Host, Session } from "../src/types";

const production: Host = { ...remote, id: "production", name: "Production", target: "ops@production.example.test" };
const alpha: Session = { ...work, title: "Development shell" };
// Identical runtime IDs across hosts must never alias a pane or its input.
const beta: Session = { ...work, title: "Production shell", createdAt: "2026-10-07T09:00:00Z" };
const gamma: Session = { ...work, id: "session-3", title: "Build watcher" };
const delta: Session = { ...work, id: "session-4", title: "Deployment log" };
const epsilon: Session = { ...work, id: "session-5", title: "Spare shell" };
const terminalPath = (host: string, session = work.id) => `/api/hosts/${host}/runtime/sessions/${session}/terminal`;
const pane = (page: Page, host: string, session = work.id) => page.locator(`.terminal-pane[data-pane-host="${host}"][data-pane-session="${session}"]`);
const inputText = (fixture: Awaited<ReturnType<typeof mockFleet>>, path: string) => fixture.terminalMessages.filter((message) => message.path === path && message.type === "input").map((message) => message.data).join("");

async function fixtureForPanes(page: Page, occupiedProduction = false) {
  return mockFleet(page, {
    hosts: [structuredClone(local), structuredClone(remote), structuredClone(production)],
    sessions: { local: [], dev: [structuredClone(alpha), structuredClone(gamma), structuredClone(epsilon)], production: [structuredClone(beta), structuredClone(delta)] },
    terminalSessions: {
      [terminalPath("dev")]: { output: "DEVELOPMENT_ONLY\r\n", geometry: { cols: 70, rows: 18 } },
      [terminalPath("production")]: { output: "PRODUCTION_ONLY\r\n", occupied: occupiedProduction, geometry: { cols: 45, rows: 13 } },
      [terminalPath("dev", gamma.id)]: { output: "BUILD_ONLY\r\n" },
      [terminalPath("production", delta.id)]: { output: "DEPLOYMENT_ONLY\r\n" },
    },
  });
}

async function addPane(page: Page, host: Host, session: Session) {
  await page.getByRole("button", { name: "Add terminal pane", exact: true }).first().click();
  const dialog = page.getByRole("dialog", { name: "Add terminal pane", exact: true });
  await expect(dialog).toBeVisible();
  await dialog.getByRole("button", { name: `${session.title} on ${host.name}`, exact: true }).click();
  await expect(dialog).not.toBeVisible();
  const added = pane(page, host.id, session.id);
  await expect(added.locator(".xterm")).toBeVisible();
  await expect(added).toHaveAttribute("data-active", "true");
  const query = new URLSearchParams(new URL(page.url()).hash.split("?")[1]);
  const selected = JSON.parse(query.get("panes")!)[Number(query.get("active"))];
  expect(selected).toMatchObject({ host: host.id, session: session.id, createdAt: session.createdAt });
}

async function claim(target: Locator) {
  await target.getByRole("button", { name: "Take control", exact: true }).click();
  await expect(target.getByText("Controlling", { exact: true })).toBeVisible();
}

async function expectGridFits(target: Locator) {
  await expect.poll(async () => target.locator(".terminal-canvas").evaluate((canvas) => {
    const mount = canvas.querySelector(".terminal-mount")!;
    const screen = canvas.querySelector(".xterm-screen")!;
    const box = canvas.getBoundingClientRect(), content = mount.getBoundingClientRect(), grid = screen.getBoundingClientRect();
    const style = getComputedStyle(canvas);
    const right = box.left + canvas.clientWidth - parseFloat(style.paddingRight);
    const bottom = box.top + canvas.clientHeight - parseFloat(style.paddingBottom);
    return content.width > 80 && content.height > 70 && content.right <= right + 1 && content.bottom <= bottom + 1 && grid.right <= right + 1 && grid.bottom <= bottom + 1;
  })).toBe(true);
}

test("cross-machine panes isolate output, input and leases even when session IDs match", async ({ page }) => {
  const fixture = await fixtureForPanes(page);
  await page.goto("/#host/dev/session/session-1");
  await addPane(page, production, beta);
  const first = pane(page, "dev"), second = pane(page, "production");
  await expect(first.locator(".xterm-rows")).toContainText("DEVELOPMENT_ONLY");
  await expect(first.locator(".xterm-rows")).not.toContainText("PRODUCTION_ONLY");
  await expect(second.locator(".xterm-rows")).toContainText("PRODUCTION_ONLY");
  await expect(second.locator(".xterm-rows")).not.toContainText("DEVELOPMENT_ONLY");
  await expect(first.locator(".xterm-rows > div")).toHaveCount(18);
  await expect(second.locator(".xterm-rows > div")).toHaveCount(13);
  expect(fixture.terminalMessages).toEqual([]);
  await claim(first);
  await first.locator(".xterm-helper-textarea").focus();
  await page.keyboard.type("dev-only");
  await expect(first).toHaveAttribute("data-active", "true");
  await expect.poll(() => inputText(fixture, terminalPath("dev"))).toBe("dev-only");
  expect(inputText(fixture, terminalPath("production"))).toBe("");
  await claim(second);
  await second.locator(".xterm-helper-textarea").focus();
  await page.keyboard.type("prod-only");
  await expect(second).toHaveAttribute("data-active", "true");
  await expect(first).toHaveAttribute("data-active", "false");
  await expect.poll(() => inputText(fixture, terminalPath("production"))).toBe("prod-only");
  expect(inputText(fixture, terminalPath("dev"))).toBe("dev-only");
  expect(fixture.attachments.sort()).toEqual([terminalPath("dev"), terminalPath("production")].sort());
  expect(fixture.mutations).toEqual([]);
});

test("closing a pane detaches only its viewer and leaves both saved sessions untouched", async ({ page }) => {
  const fixture = await fixtureForPanes(page);
  await page.goto("/#host/dev/session/session-1");
  const original = await pane(page, "dev").locator(".xterm").elementHandle();
  await addPane(page, production, beta);
  await pane(page, "production").getByRole("button", { name: "Close pane", exact: true }).click();
  await expect(page.locator(".terminal-pane")).toHaveCount(1);
  await expect.poll(() => fixture.activeTerminalPaths()).toEqual([terminalPath("dev")]);
  expect(await original!.evaluate((element) => element.isConnected)).toBe(true);
  expect(fixture.sessions.dev.find((item) => item.id === alpha.id)?.status).toBe("running");
  expect(fixture.sessions.production.find((item) => item.id === beta.id)?.status).toBe("running");
  expect(fixture.mutations).toEqual([]);
  await addPane(page, production, beta);
  expect(fixture.attachments.filter((path) => path === terminalPath("dev"))).toHaveLength(1);
  expect(fixture.attachments.filter((path) => path === terminalPath("production"))).toHaveLength(2);
});

test("the picker excludes visible sessions, filters by machine and enforces the four-pane limit", async ({ page }) => {
  const fixture = await fixtureForPanes(page);
  await page.goto("/#host/dev/session/session-1");
  await page.getByRole("button", { name: "Add terminal pane", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Add terminal pane", exact: true });
  await expect(dialog.getByRole("button", { name: `${alpha.title} on ${remote.name}`, exact: true })).toHaveCount(0);
  await dialog.getByLabel("Find a session", { exact: true }).fill("Production");
  await expect(dialog.getByRole("button", { name: `${beta.title} on ${production.name}`, exact: true })).toBeVisible();
  await expect(dialog.getByRole("button", { name: `${gamma.title} on ${remote.name}`, exact: true })).toHaveCount(0);
  await dialog.getByRole("button", { name: `${beta.title} on ${production.name}`, exact: true }).click();
  await addPane(page, remote, gamma);
  await addPane(page, production, delta);
  await expect(page.locator(".terminal-pane")).toHaveCount(4);
  const addButtons = page.getByRole("button", { name: "Add terminal pane", exact: true });
  for (const button of await addButtons.all()) await expect(button).toBeDisabled();
  expect(fixture.attachments).toHaveLength(4);
  expect(new Set(fixture.attachments).size).toBe(4);
  expect(fixture.mutations).toEqual([]);
  await pane(page, "production", delta.id).getByRole("button", { name: "Close pane", exact: true }).click();
  await addPane(page, remote, epsilon);
  await expect(page.locator(".terminal-pane")).toHaveCount(4);
  expect(fixture.mutations).toEqual([]);
});

test("expanding and restoring one pane preserves the other DOM, output and connections", async ({ page }) => {
  const fixture = await fixtureForPanes(page);
  await page.goto("/#host/dev/session/session-1");
  await addPane(page, production, beta);
  const first = pane(page, "dev"), second = pane(page, "production");
  const handles = [await first.locator(".xterm").elementHandle(), await second.locator(".xterm").elementHandle()];
  await claim(first);
  await claim(second);
  await expectGridFits(first);
  await expectGridFits(second);
  await second.getByRole("button", { name: "Expand terminal to full window", exact: true }).click();
  await expect(first).toBeHidden();
  await expect(second).toBeVisible();
  await expectGridFits(second);
  fixture.sendTerminalOutput("\r\nBACKGROUND_SURVIVES\r\n", terminalPath("dev"));
  expect(fixture.activeTerminalPaths().sort()).toEqual([terminalPath("dev"), terminalPath("production")].sort());
  await second.getByRole("button", { name: "Restore workspace layout", exact: true }).click();
  await expect(first).toBeVisible();
  await expect(first.locator(".xterm-rows")).toContainText("BACKGROUND_SURVIVES");
  await expectGridFits(first);
  await expectGridFits(second);
  for (const handle of handles) expect(await handle!.evaluate((element) => element.isConnected)).toBe(true);
  expect(fixture.attachments).toHaveLength(2);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
});

test("an occupied pane and a dropped connection do not interrupt another pane's control", async ({ page }) => {
  const fixture = await fixtureForPanes(page, true);
  await page.goto("/#host/dev/session/session-1");
  await addPane(page, production, beta);
  const first = pane(page, "dev"), second = pane(page, "production");
  await claim(first);
  await expect(second.getByRole("button", { name: "Take control", exact: true })).toBeDisabled();
  await second.locator(".terminal-canvas").click();
  await page.keyboard.type("not-authorized");
  expect(inputText(fixture, terminalPath("production"))).toBe("");
  fixture.disconnectTerminals(terminalPath("production"));
  await expect.poll(() => fixture.attachments.filter((path) => path === terminalPath("production")).length).toBe(2);
  await expect(first.getByText("Controlling", { exact: true })).toBeVisible();
  expect(fixture.attachments.filter((path) => path === terminalPath("dev"))).toHaveLength(1);
  await first.locator(".xterm-helper-textarea").focus();
  await page.keyboard.type("still-connected");
  await expect.poll(() => inputText(fixture, terminalPath("dev"))).toBe("still-connected");
  fixture.setTerminalOccupied(false, terminalPath("production"));
  await expect(second.getByRole("button", { name: "Take control", exact: true })).toBeEnabled();
  expect(fixture.terminalMessages.filter((message) => message.path === terminalPath("production") && ["claim", "input"].includes(message.type))).toEqual([]);
  await claim(second);
  await second.getByRole("button", { name: "Release control", exact: true }).click();
  await expect(first.getByText("Controlling", { exact: true })).toBeVisible();
  await expect(second.getByText("Watching", { exact: true })).toBeVisible();
  expect(fixture.mutations).toEqual([]);
});

test("four panes use a wide grid and a narrow scrollable stack with each owned grid fitting", async ({ page }, testInfo) => {
  const fixture = await fixtureForPanes(page);
  await page.goto("/#host/dev/session/session-1");
  await addPane(page, production, beta);
  await addPane(page, remote, gamma);
  await addPane(page, production, delta);
  const panes = [pane(page, "dev"), pane(page, "production"), pane(page, "dev", gamma.id), pane(page, "production", delta.id)];
  for (const target of panes) await claim(target);
  for (const size of [{ width: 1440, height: 1000 }, { width: 390, height: 844 }]) {
    await page.setViewportSize(size);
    for (const target of panes) { await target.scrollIntoViewIfNeeded(); await expectGridFits(target); }
    const boxes = await Promise.all(panes.map((target) => target.boundingBox()));
    if (size.width > 1000) {
      expect(Math.abs(boxes[0]!.y - boxes[1]!.y)).toBeLessThan(2);
      expect(boxes[1]!.x).toBeGreaterThan(boxes[0]!.x + boxes[0]!.width - 2);
      expect(boxes[2]!.y).toBeGreaterThan(boxes[0]!.y + boxes[0]!.height - 2);
    } else {
      for (let i = 1; i < boxes.length; i++) {
        expect(Math.abs(boxes[i]!.x - boxes[0]!.x)).toBeLessThan(2);
        expect(boxes[i]!.y).toBeGreaterThanOrEqual(boxes[i - 1]!.y + boxes[i - 1]!.height - 1);
      }
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1)).toBe(true);
    }
    if (testInfo.project.name === "desktop") await page.screenshot({ path: testInfo.outputPath(`terminal-panes-${size.width}.png`), animations: "disabled" });
  }
  expect(fixture.attachments).toHaveLength(4);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
});

test("restoring a pane URL attaches the exact host and session without creating processes", async ({ page }) => {
  const fixture = await fixtureForPanes(page);
  await page.goto("/#host/dev/session/session-1");
  await addPane(page, production, beta);
  await pane(page, "production").getByRole("button", { name: "Expand terminal to full window", exact: true }).click();
  const url = page.url();
  expect(new URL(url).hash).toContain("terminals?");
  await page.reload();
  await expect(pane(page, "production").locator(".xterm-rows")).toContainText("PRODUCTION_ONLY");
  await expect(pane(page, "dev")).toBeHidden();
  await expect(pane(page, "production")).toHaveAttribute("data-active", "true");
  expect(fixture.attachments.filter((path) => path === terminalPath("dev"))).toHaveLength(2);
  expect(fixture.attachments.filter((path) => path === terminalPath("production"))).toHaveLength(2);
  expect(fixture.terminalMessages.filter((message) => ["claim", "input"].includes(message.type))).toEqual([]);
  expect(fixture.mutations).toEqual([]);
});

test("the compact header keeps details optional and urgent attention visible without remounting the terminal", async ({ page }) => {
  const session: Session = { ...alpha, attention: { kind: "permission", source: "claude-hook", updatedAt: new Date().toISOString() } };
  const fixture = await mockFleet(page, { sessions: { local: [], dev: [session] } });
  await page.goto("/#host/dev/session/session-1");
  const target = pane(page, "dev");
  await expect(target.locator(".xterm")).toBeVisible();
  const original = await target.locator(".xterm").elementHandle();
  const details = target.getByRole("region", { name: "Session details", exact: true });
  const toggle = target.getByRole("button", { name: "Session details", exact: true });
  await expect(toggle).toHaveAttribute("aria-expanded", "false");
  await expect(details).toBeHidden();
  await expect(target.locator(".attention-banner")).toContainText("The agent reported a permission request. Review it in the terminal.");
  await expect(target.getByRole("button", { name: "Mark reviewed" })).toBeVisible();
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-expanded", "true");
  await expect(details).toContainText(alpha.cwd);
  await expect(details).toContainText(alpha.workspace);
  await toggle.press("Enter");
  await expect(details).toBeHidden();
  expect(await original!.evaluate((element) => element.isConnected)).toBe(true);
  expect(fixture.attachments).toHaveLength(1);
  expect(fixture.mutations).toEqual([]);
  expect(fixture.terminalMessages).toEqual([]);
});

test("the picker handles no remaining sessions and an empty search without creating anything", async ({ page }) => {
  const fixture = await mockFleet(page, { sessions: { local: [], dev: [structuredClone(alpha)] } });
  await page.goto("/#host/dev/session/session-1");
  await page.getByRole("button", { name: "Add terminal pane", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Add terminal pane", exact: true });
  await expect(dialog.getByText("All available sessions are already in this view.", { exact: true })).toBeVisible();
  const search = dialog.getByLabel("Find a session", { exact: true });
  await expect(search).toBeFocused();
  await search.fill("not a session");
  await expect(dialog.getByText("No matching sessions.", { exact: true })).toBeVisible();
  await search.press("Escape");
  await expect(dialog).not.toBeVisible();
  await expect(page.getByRole("button", { name: "Add terminal pane", exact: true })).toBeFocused();
  expect(fixture.attachments).toHaveLength(1);
  expect(fixture.mutations).toEqual([]);
});

test("creating a session from the picker uses the chosen machine and appends only the returned session", async ({ page }) => {
  const fixture = await fixtureForPanes(page);
  await page.goto("/#host/dev/session/session-1");
  const original = await pane(page, "dev").locator(".xterm").elementHandle();
  await page.getByRole("button", { name: "Add terminal pane", exact: true }).click();
  await page.getByRole("dialog", { name: "Add terminal pane", exact: true }).getByRole("button", { name: "New session on Production", exact: true }).click();
  const form = page.getByRole("dialog", { name: "Start a session", exact: true });
  await expect(form).toContainText("A fresh terminal on Production.");
  await form.getByRole("radio", { name: "Terminal Your remote shell", exact: true }).check();
  await form.getByLabel("Project folder", { exact: false }).fill("/home/dev/projects/another project");
  await form.getByLabel("Workspace", { exact: false }).fill("Extra work");
  await form.getByLabel("Session name", { exact: true }).fill("Created deliberately");
  await form.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(pane(page, "production", "created-session").locator(".xterm")).toBeVisible();
  await expect(pane(page, "production", "created-session")).toHaveAttribute("data-active", "true");
  await expect(page.locator(".terminal-pane")).toHaveCount(2);
  expect(fixture.mutations).toHaveLength(1);
  expect(fixture.mutations[0]).toMatchObject({ method: "POST", path: "/api/hosts/production/runtime/sessions", body: { title: "Created deliberately", cwd: "/home/dev/projects/another project", workspace: "Extra work", harness: "shell" } });
  expect(await original!.evaluate((element) => element.isConnected)).toBe(true);
  expect(fixture.attachments.filter((path) => path === terminalPath("dev"))).toHaveLength(1);
});

for (const unavailable of ["missing machine", "failed session read"] as const) {
  test(`a restored pane with ${unavailable} exposes recovery without attaching the wrong terminal`, async ({ page }) => {
    const host = unavailable === "missing machine" ? "removed" : "production";
    const fixture = await mockFleet(page, {
      hosts: [structuredClone(remote), structuredClone(production)],
      sessions: { dev: [structuredClone(alpha)], production: [structuredClone(beta)] },
      failHosts: unavailable === "failed session read" ? ["production"] : [],
    });
    const query = new URLSearchParams({ panes: JSON.stringify([{ host: "dev", session: alpha.id, createdAt: alpha.createdAt }, { host, session: beta.id, createdAt: beta.createdAt }]), active: "1" });
    await page.goto(`/#terminals?${query}`);
    const unavailablePane = pane(page, host);
    await expect(unavailablePane.getByRole("button", { name: "Choose a session", exact: true })).toBeVisible();
    await expect(unavailablePane.getByRole("heading")).not.toContainText("Loading");
    if (unavailable === "failed session read") await expect(unavailablePane).toContainText("SSH connection unavailable");
    await expect(unavailablePane.locator(".xterm")).toHaveCount(0);
    await expect(pane(page, "dev").locator(".xterm")).toBeVisible();
    expect(fixture.attachments).toEqual([terminalPath("dev")]);
    await unavailablePane.getByRole("button", { name: "Close pane", exact: true }).click();
    await expect(page.locator(".terminal-pane")).toHaveCount(1);
    expect(fixture.mutations).toEqual([]);
  });
}

test("a replaced session identity disconnects only that pane and is never silently rebound", async ({ page }) => {
  const fixture = await fixtureForPanes(page);
  await page.goto("/#host/dev/session/session-1");
  await addPane(page, production, beta);
  const first = pane(page, "dev"), second = pane(page, "production");
  const original = await first.locator(".xterm").elementHandle();
  fixture.sessions.production[0] = { ...beta, createdAt: "2026-11-03T10:00:00Z", title: "Unrelated replacement" };
  await expect(second).toContainText("The session identity has changed.");
  await expect(second.locator(".xterm")).toHaveCount(0);
  await expect.poll(() => fixture.activeTerminalPaths()).toEqual([terminalPath("dev")]);
  expect(await original!.evaluate((element) => element.isConnected)).toBe(true);
  expect(fixture.attachments).toHaveLength(2);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
  expect(fixture.mutations).toEqual([]);
  await second.getByRole("button", { name: "Choose a session", exact: true }).click();
  await page.getByRole("dialog", { name: "Choose a session", exact: true }).getByRole("button", { name: "Unrelated replacement on Production", exact: true }).click();
  await expect(second.locator(".xterm")).toBeVisible();
  expect(fixture.attachments.filter((path) => path === terminalPath("production"))).toHaveLength(2);
  expect(fixture.attachments.filter((path) => path === terminalPath("dev"))).toHaveLength(1);
  expect(fixture.mutations).toEqual([]);
});

test("sidebar session navigation focuses an existing pane and replaces only the active pane otherwise", async ({ page }) => {
  const fixture = await fixtureForPanes(page);
  await page.goto("/#host/dev/session/session-1");
  await addPane(page, production, beta);
  const original = await pane(page, "production").locator(".xterm").elementHandle();
  await openNavigation(page);
  const navigation = page.getByRole("navigation", { name: "Machines and workspaces", exact: true });
  await navigation.getByRole("button", { name: "Development shell running", exact: true }).click();
  await expect(pane(page, "dev")).toHaveAttribute("data-active", "true");
  await expect(page.locator(".terminal-pane")).toHaveCount(2);
  expect(fixture.attachments).toHaveLength(2);
  await openNavigation(page);
  await navigation.getByRole("button", { name: "Build watcher running", exact: true }).click();
  await expect(pane(page, "dev", gamma.id).locator(".xterm")).toBeVisible();
  await expect(pane(page, "dev", alpha.id)).toHaveCount(0);
  await expect(pane(page, "dev", gamma.id)).toHaveAttribute("data-active", "true");
  expect(await original!.evaluate((element) => element.isConnected)).toBe(true);
  expect(fixture.attachments.filter((path) => path === terminalPath("production"))).toHaveLength(1);
  await expect.poll(() => fixture.activeTerminalPaths().sort()).toEqual([terminalPath("dev", gamma.id), terminalPath("production")].sort());
  expect(fixture.mutations).toEqual([]);
});

test("invalid or excessive pane URLs fail closed without attaching sessions", async ({ page }) => {
  const fixture = await fixtureForPanes(page);
  const valid = [{ host: "dev", session: alpha.id }, { host: "production", session: beta.id }];
  const cases = [
    { panes: [...valid, { host: "dev", session: gamma.id }, { host: "production", session: delta.id }, { host: "dev", session: epsilon.id }], active: "0" },
    { panes: [valid[0], valid[0]], active: "0" },
    { panes: valid, active: "2" },
    { panes: [{ host: "dev\n", session: alpha.id }], active: "0" },
  ];
  for (const invalid of cases) {
    await page.goto(`/#terminals?${new URLSearchParams({ panes: JSON.stringify(invalid.panes), active: invalid.active })}`);
    await expect(page.getByRole("heading", { name: "Fleet overview", exact: true })).toBeVisible();
    await expect(page.locator(".terminal-pane")).toHaveCount(0);
  }
  expect(fixture.attachments).toEqual([]);
  expect(fixture.mutations).toEqual([]);
});
