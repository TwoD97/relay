import { expect, test, type Page } from "@playwright/test";
import { mockFleet } from "./fixture";

async function newSession(page: Page, host = "local") {
  await page.goto(`/#host/${host}`);
  await page.getByRole("button", { name: "New session", exact: true }).first().click();
}

test("remote folder browser navigates and starts a session with the selected literal path", async ({ page }, testInfo) => {
  const fixture = await mockFleet(page, { sessions: { local: [], dev: [] } });
  await newSession(page, "dev");
  await page.getByRole("button", { name: "Browse folders", exact: true }).click();
  const browser = page.getByRole("region", { name: "Folder browser for Development" });
  await expect(browser).toBeVisible();
  await browser.getByRole("button", { name: "Open folder projects", exact: true }).click();
  await browser.getByRole("button", { name: "Open folder api service", exact: true }).click();
  await browser.getByRole("button", { name: "Open folder $literal [draft]", exact: true }).click();
  await expect(browser.getByText("This folder has no subfolders.")).toBeVisible();
  expect(fixture.directoryRequests.every((request) => request.host === "dev")).toBeTruthy();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBeTruthy();
  await page.screenshot({ path: testInfo.outputPath("remote-folder-picker.png"), fullPage: true });
  await browser.getByRole("button", { name: "Use this folder", exact: true }).click();
  await expect(page.getByLabel("Project folder")).toHaveValue("/home/dev/projects/api service/$literal [draft]");
  await expect(browser).toHaveCount(0);
  await page.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Terminal", exact: true })).toBeVisible();
  expect(fixture.mutations.at(-1)).toMatchObject({ path: "/api/hosts/dev/runtime/sessions", body: { cwd: "/home/dev/projects/api service/$literal [draft]" } });
});

test("typed path completion supports keyboard navigation and Escape closes only the picker", async ({ page }) => {
  const fixture = await mockFleet(page);
  await newSession(page);
  const input = page.getByLabel("Project folder");
  await input.fill("~/pro");
  const project = page.getByRole("button", { name: "Open folder projects", exact: true });
  await expect(project).toBeVisible();
  expect(fixture.directoryRequests.at(-1)).toMatchObject({ host: "local", path: "~", prefix: "pro" });
  await input.press("ArrowDown");
  await expect(project).toBeFocused();
  await project.press("Enter");
  await expect(page.getByRole("button", { name: "Open folder api service", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Open folder api service", exact: true }).focus();
  await page.keyboard.press("End");
  await expect(page.getByRole("button", { name: "Open folder relay", exact: true })).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog")).toBeVisible();
  await expect(page.getByRole("region", { name: /Folder browser/ })).toHaveCount(0);
  await expect(input).toBeFocused();
  await expect(input).toHaveValue("~/pro");
  expect(fixture.mutations).toEqual([]);
});

test("home, parent, breadcrumbs, hidden folders, and trailing spaces remain literal", async ({ page }) => {
  const fixture = await mockFleet(page);
  await newSession(page);
  await page.getByRole("button", { name: "Browse folders", exact: true }).click();
  await expect(page.getByRole("button", { name: "Open folder projects", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Open folder .config", exact: true })).toHaveCount(0);
  await page.getByLabel("Show hidden folders").check();
  await page.getByRole("button", { name: "Open folder .config", exact: true }).click();
  await expect(page.getByText("This folder has no subfolders.")).toBeVisible();
  await page.getByRole("button", { name: "Parent folder", exact: true }).click();
  await page.getByRole("button", { name: "Open folder projects", exact: true }).click();
  await page.getByRole("navigation", { name: "Folder location" }).getByRole("button", { name: "Root folder", exact: true }).click();
  await expect(page.getByRole("button", { name: "Parent folder", exact: true })).toBeDisabled();
  await page.getByRole("button", { name: "Home folder", exact: true }).click();
  await page.getByRole("button", { name: "Open folder space folder", exact: true }).click();
  await page.getByRole("button", { name: "Use this folder", exact: true }).click();
  await expect(page.getByLabel("Project folder")).toHaveValue("/home/local/space folder ");
  await page.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Terminal", exact: true })).toBeVisible();
  expect(fixture.mutations.at(-1)).toMatchObject({ body: { cwd: "/home/local/space folder " } });
});

test("permission errors are recoverable without losing the session form", async ({ page }) => {
  await mockFleet(page);
  let deny = true;
  await page.route("**/api/hosts/local/directories?*", async (route) => {
    if (deny) return route.fulfill({ status: 403, json: { error: "You do not have permission to read this folder." } });
    return route.fallback();
  });
  await newSession(page);
  await page.getByLabel("Session name").fill("Keep this session title");
  await page.getByRole("button", { name: "Browse folders", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("permission");
  await expect(page.getByRole("button", { name: "Use this folder", exact: true })).toHaveCount(0);
  deny = false;
  await page.getByRole("button", { name: "Try again", exact: true }).click();
  await expect(page.getByRole("button", { name: "Open folder projects", exact: true })).toBeVisible();
  await expect(page.getByLabel("Session name")).toHaveValue("Keep this session title");
});

test("a late directory response cannot replace the newer path or selection", async ({ page }) => {
  await mockFleet(page);
  let release!: () => void;
  const pending = new Promise<void>((resolve) => { release = resolve; });
  let requested = false;
  await page.route("**/api/hosts/local/directories?*", async (route) => {
    if (new URL(route.request().url()).searchParams.get("path") !== "/slow/") return route.fallback();
    requested = true;
    await pending;
    await route.fulfill({ json: { home: "/home/local", path: "/slow", parent: "/", directories: [{ name: "stale folder", path: "/slow/stale folder" }], truncated: false } }).catch(() => {});
  });
  await newSession(page);
  await page.getByLabel("Project folder").fill("/slow/");
  await expect.poll(() => requested).toBeTruthy();
  await expect(page.getByRole("status")).toContainText("Loading folders");
  await page.getByLabel("Project folder").fill("/home/local/projects/");
  await expect(page.getByRole("button", { name: "Open folder api service", exact: true })).toBeVisible();
  release();
  await page.getByRole("button", { name: "Use this folder", exact: true }).click();
  await expect(page.getByLabel("Project folder")).toHaveValue("/home/local/projects");
  await expect(page.getByText("stale folder", { exact: true })).toHaveCount(0);
});

test("unmatched prefixes, dot completion, and disconnected machines are explicit", async ({ page }) => {
  const fixture = await mockFleet(page);
  await newSession(page);
  await page.getByLabel("Project folder").fill("~/absent-prefix");
  await expect(page.getByText("No matching folders.")).toBeVisible();
  await page.getByLabel("Project folder").fill("~/.con");
  await expect(page.getByRole("button", { name: "Open folder .config", exact: true })).toBeVisible();
  expect(fixture.directoryRequests.at(-1)).toMatchObject({ path: "~", prefix: ".con", hidden: false });
  fixture.hosts.find((host) => host.id === "local")!.status = "disconnected";
  await expect(page.getByRole("alert")).toContainText("Reconnect This computer");
  await expect(page.getByRole("button", { name: "Browse folders", exact: true })).toBeDisabled();
  await expect(page.getByRole("button", { name: "Start session", exact: true })).toBeDisabled();
});
