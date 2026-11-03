import { expect, test, type Page, type Route } from "@playwright/test";
import { mockFleet, remote } from "./fixture";
import type { MachineStatsSample } from "../src/MachineStats";

const GiB = 1024 ** 3;
const endpoint = "**/api/hosts/dev/stats";
const resources = (page: Page) => page.getByRole("region", { name: "Machine resources", exact: true });
const cpu = (page: Page) => resources(page).getByRole("article", { name: "CPU usage", exact: true });
const ram = (page: Page) => resources(page).getByRole("article", { name: "RAM usage", exact: true });
const disk = (page: Page) => resources(page).getByRole("article", { name: "Storage usage", exact: true });
function sample(): MachineStatsSample {
  return { sampledAt: new Date().toISOString(), platform: "linux", cpu: { usagePercent: 37, logicalCpus: 8, intervalSeconds: 5 }, memory: { totalBytes: 16 * GiB, availableBytes: 12 * GiB, usedBytes: 4 * GiB }, storage: [{ scope: "root", path: "/", totalBytes: 100 * GiB, availableBytes: 30 * GiB, usedBytes: 70 * GiB }, { scope: "home", path: "/home/dev", totalBytes: 100 * GiB, availableBytes: 30 * GiB, usedBytes: 70 * GiB }], warnings: [] };
}
async function visible(page: Page, value: boolean) {
  await page.evaluate((next) => {
    Object.defineProperty(document, "visibilityState", { configurable: true, value: next ? "visible" : "hidden" });
    document.dispatchEvent(new Event("visibilitychange"));
  }, value);
}

test("resources distinguish initial loading, CPU sampling and a measured percentage on the exact machine endpoint", async ({ page }) => {
  const fixture = await mockFleet(page);
  let pending: Route | undefined;
  let calls = 0;
  const paths: string[] = [];
  await page.route(endpoint, (route) => { paths.push(new URL(route.request().url()).pathname); calls++; if (calls === 1) { pending = route; return; } return route.fulfill({ json: sample() }); });
  await page.goto("/#host/dev");
  await expect(cpu(page).getByText("Loading…", { exact: true })).toBeVisible();
  await expect.poll(() => Boolean(pending)).toBe(true);
  const first = sample(); first.cpu.usagePercent = null; first.cpu.intervalSeconds = null;
  await pending!.fulfill({ json: first });
  await expect(cpu(page).getByText("Sampling…", { exact: true })).toBeVisible();
  await expect(cpu(page)).toContainText("CPU usage needs two readings.");
  await expect(cpu(page)).not.toContainText("0%");
  await resources(page).getByRole("button", { name: "Refresh machine resources", exact: true }).click();
  await expect(cpu(page).getByText("37%", { exact: true })).toBeVisible();
  await expect(cpu(page)).toContainText("8 logical CPUs");
  expect(paths).toEqual(["/api/hosts/dev/stats", "/api/hosts/dev/stats"]);
  expect(fixture.mutations).toEqual([]);
});

test("RAM and filesystem values are readable, never summed, and fit both themes and narrow screens", async ({ page }, testInfo) => {
  const fixture = await mockFleet(page);
  const value = sample();
  await page.route(endpoint, (route) => route.fulfill({ json: value }));
  await page.goto("/#host/dev");
  await expect(ram(page).getByText("4 GiB", { exact: true })).toBeVisible();
  await expect(ram(page)).toContainText("used of 16 GiB · 12 GiB available");
  await expect(disk(page).getByText("70 GiB", { exact: true })).toBeVisible();
  await expect(disk(page)).toContainText("30 GiB available of 100 GiB");
  await expect(resources(page).locator("time")).toHaveAttribute("datetime", value.sampledAt);
  await resources(page).getByText("Storage locations (2)", { exact: true }).click();
  await expect(resources(page)).toContainText("values are not added together");
  await expect(resources(page)).toContainText("/home/dev");
  await expect(resources(page)).not.toContainText("140 GiB");
  for (const theme of ["Light", "Dark"]) {
    await page.getByLabel("Appearance", { exact: true }).selectOption(theme.toLowerCase());
    await resources(page).scrollIntoViewIfNeeded();
    const bounds = await resources(page).boundingBox();
    expect(bounds!.x).toBeGreaterThanOrEqual(0);
    expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(page.viewportSize()!.width + 1);
    await page.screenshot({ path: testInfo.outputPath(`machine-resources-${theme.toLowerCase()}.png`), animations: "disabled" });
  }
  expect(fixture.mutations).toEqual([]);
});

test("partial probe failures stay unavailable while genuine zero CPU remains a valid reading", async ({ page }) => {
  const fixture = await mockFleet(page);
  const value = sample(); value.cpu.usagePercent = null; value.cpu.error = "CPU counters unavailable"; value.memory = null; value.storage = []; value.warnings = ["Memory and storage could not be sampled."];
  await page.route(endpoint, (route) => route.fulfill({ json: value }));
  await page.goto("/#host/dev");
  await expect(cpu(page).getByText("Unavailable", { exact: true })).toBeVisible();
  await expect(cpu(page)).toContainText("CPU counters unavailable");
  await expect(cpu(page)).not.toContainText("Sampling");
  await expect(ram(page).getByText("Unavailable", { exact: true })).toBeVisible();
  await expect(disk(page).getByText("Unavailable", { exact: true })).toBeVisible();
  await expect(resources(page)).toContainText("Memory and storage could not be sampled.");
  delete value.cpu.error; value.cpu.usagePercent = 0;
  await resources(page).getByRole("button", { name: "Refresh machine resources", exact: true }).click();
  await expect(cpu(page).getByText("0%", { exact: true })).toBeVisible();
  expect(fixture.mutations).toEqual([]);
});

test("a failed refresh and offline host retain only an explicitly stale sample and recover on deliberate retry", async ({ page }) => {
  const fixture = await mockFleet(page);
  let failed = false;
  let calls = 0;
  const value = sample();
  await page.route(endpoint, (route) => { calls++; return failed ? route.fulfill({ status: 502, json: { error: "SSH statistics unavailable" } }) : route.fulfill({ json: value }); });
  await page.goto("/#host/dev");
  await expect(cpu(page).getByText("37%", { exact: true })).toBeVisible();
  failed = true;
  await resources(page).getByRole("button", { name: "Refresh machine resources", exact: true }).click();
  await expect(resources(page).getByRole("alert")).toContainText("SSH statistics unavailable");
  await expect(resources(page)).toHaveClass(/machine-stats-stale/);
  await expect(resources(page).getByRole("status")).toContainText("current usage may differ");
  await expect(cpu(page).getByText("37%", { exact: true })).toBeVisible();
  failed = false; value.cpu.usagePercent = 18;
  await resources(page).getByRole("button", { name: "Retry machine resources", exact: true }).click();
  await expect(cpu(page).getByText("18%", { exact: true })).toBeVisible();
  await expect(resources(page).getByRole("alert")).toHaveCount(0);
  fixture.hosts.find((host) => host.id === "dev")!.status = "disconnected";
  await expect(resources(page).getByRole("status")).toContainText("Offline. Showing the last available sample.");
  await expect(cpu(page).getByText("18%", { exact: true })).toBeVisible();
  await expect(resources(page).getByRole("button", { name: "Refresh machine resources", exact: true })).toBeDisabled();
  const stoppedAt = calls;
  await page.clock.install(); await page.clock.runFor(6000);
  expect(calls).toBe(stoppedAt);
  expect(fixture.mutations).toEqual([]);
});

test("a late response from another host never replaces the currently selected machine's resources", async ({ page }) => {
  const production = { ...remote, id: "production", name: "Production" };
  const fixture = await mockFleet(page, { hosts: [structuredClone(remote), production], sessions: { dev: [], production: [] } });
  let pending: Route | undefined;
  const paths: string[] = [];
  await page.route("**/api/hosts/*/stats", (route) => {
    const path = new URL(route.request().url()).pathname; paths.push(path);
    if (path.includes("/dev/")) { pending = route; return; }
    const value = sample(); value.cpu.usagePercent = 61;
    return route.fulfill({ json: value });
  });
  await page.goto("/#host/dev");
  await expect.poll(() => Boolean(pending)).toBe(true);
  await page.evaluate(() => { location.hash = "host/production"; });
  await expect(cpu(page).getByText("61%", { exact: true })).toBeVisible();
  await pending!.fulfill({ json: sample() }).catch(() => {});
  await expect(cpu(page).getByText("61%", { exact: true })).toBeVisible();
  await expect(resources(page)).not.toContainText("37%");
  expect(paths).toEqual(["/api/hosts/dev/stats", "/api/hosts/production/stats"]);
  expect(fixture.mutations).toEqual([]);
});

test("statistics poll only while the visible machine overview is mounted and never overlap reads", async ({ page }) => {
  const fixture = await mockFleet(page);
  await page.clock.install();
  let calls = 0;
  let pending: Route | undefined;
  await page.route(endpoint, (route) => { calls++; if (calls === 2) { pending = route; return; } return route.fulfill({ json: sample() }); });
  await page.goto("/#host/dev");
  await expect(cpu(page).getByText("37%", { exact: true })).toBeVisible();
  await page.clock.runFor(5100);
  await expect.poll(() => calls).toBe(2);
  await page.clock.runFor(6000);
  expect(calls).toBe(2);
  await visible(page, false);
  await pending!.fulfill({ json: sample() }).catch(() => {});
  await page.clock.runFor(20000);
  expect(calls).toBe(2);
  await expect(resources(page)).toContainText("Updates paused while this window is hidden.");
  await visible(page, true);
  await expect.poll(() => calls).toBe(3);
  await page.evaluate(() => { location.hash = ""; });
  await expect(page.getByRole("heading", { name: "Fleet overview", exact: true })).toBeVisible();
  await page.clock.runFor(20000);
  expect(calls).toBe(3);
  expect(fixture.mutations).toEqual([]);
});

for (const problem of ["unsupported endpoint", "invalid sample"] as const) {
  test(`${problem} shows unavailable resources without fabricated values`, async ({ page }) => {
    const fixture = await mockFleet(page);
    await page.route(endpoint, (route) => problem === "unsupported endpoint" ? route.fulfill({ status: 404, json: { error: "This controller does not provide resource statistics." } }) : route.fulfill({ json: { ...sample(), cpu: { usagePercent: 120, logicalCpus: 8, intervalSeconds: 5 } } }));
    await page.goto("/#host/dev");
    await expect(resources(page).getByRole("alert")).toContainText(problem === "unsupported endpoint" ? "does not provide resource statistics" : "invalid machine resource sample");
    await expect(cpu(page).getByText("Unavailable", { exact: true })).toBeVisible();
    await expect(ram(page).getByText("Unavailable", { exact: true })).toBeVisible();
    await expect(disk(page).getByText("Unavailable", { exact: true })).toBeVisible();
    await expect(resources(page).getByRole("button", { name: "Retry machine resources", exact: true })).toBeEnabled();
    expect(fixture.mutations).toEqual([]);
  });
}
